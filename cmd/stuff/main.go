package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gisikw/stuff/internal/stuff"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "stuff:", err)
			os.Exit(1)
		}
		return
	}
	cli := stuff.NewCLI()
	token, err := stuff.TokenFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stuff:", err)
		os.Exit(1)
	}
	cli.Token = token
	if err := cli.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stuff:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context) error {
	couchURL, err := couchEndpoint()
	if err != nil {
		return err
	}
	store, err := stuff.NewCouchStore(couchURL, os.Getenv("STUFF_COUCH_DB"), nil)
	if err != nil {
		return err
	}
	if err := store.Ensure(ctx); err != nil {
		return err
	}
	listen := os.Getenv("STUFF_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:7847"
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	token, err := stuff.TokenFromEnv()
	if err != nil {
		return err
	}
	server := &http.Server{Addr: listen, Handler: stuff.NewServer(store, token, logger).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if token == "" && !isLoopbackListen(listen) {
		logger.Warn("Stuff is listening beyond loopback without STUFF_TOKEN")
	}
	logger.Info("Stuff listening", "address", listen, "database", os.Getenv("STUFF_COUCH_DB"))
	return server.ListenAndServe()
}

func couchEndpoint() (string, error) {
	raw := os.Getenv("STUFF_COUCH_URL")
	if raw == "" {
		raw = "http://127.0.0.1:5984"
	}
	passwordFile := os.Getenv("STUFF_COUCH_PASSWORD_FILE")
	if passwordFile == "" {
		return raw, nil
	}
	password, err := os.ReadFile(passwordFile)
	if err != nil {
		return "", fmt.Errorf("read STUFF_COUCH_PASSWORD_FILE: %w", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse STUFF_COUCH_URL: %w", err)
	}
	user := os.Getenv("STUFF_COUCH_USER")
	if user == "" {
		user = "stuff"
	}
	u.User = url.UserPassword(user, strings.TrimSpace(string(password)))
	return u.String(), nil
}

func isLoopbackListen(s string) bool {
	return strings.HasPrefix(s, "127.0.0.1:") || strings.HasPrefix(s, "localhost:") || strings.HasPrefix(s, "[::1]:")
}

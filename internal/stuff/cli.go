package stuff

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type CLI struct {
	BaseURL  string
	Token    string
	Client   *http.Client
	In       io.Reader
	Out, Err io.Writer
}

func NewCLI() *CLI {
	base := os.Getenv("STUFF_URL")
	if base == "" {
		base = "http://127.0.0.1:7847"
	}
	token, _ := TokenFromEnv()
	return &CLI{BaseURL: strings.TrimRight(base, "/"), Token: token, Client: http.DefaultClient, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}

func TokenFromEnv() (string, error) {
	if token := os.Getenv("STUFF_TOKEN"); token != "" {
		return token, nil
	}
	file := os.Getenv("STUFF_TOKEN_FILE")
	if file == "" {
		return "", nil
	}
	value, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read STUFF_TOKEN_FILE: %w", err)
	}
	return strings.TrimSpace(string(value)), nil
}

func (c *CLI) Run(ctx context.Context, args []string) error {
	pretty := false
	clean := args[:0]
	for _, a := range args {
		if a == "--pretty" {
			pretty = true
		} else {
			clean = append(clean, a)
		}
	}
	args = clean
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" ||
		(len(args) > 1 && (args[len(args)-1] == "--help" || args[len(args)-1] == "-h")) {
		fmt.Fprint(c.Out, Help)
		return nil
	}
	switch args[0] {
	case "add":
		return c.add(ctx, args[1:])
	case "get":
		if len(args) != 2 {
			return usage("stuff get ITEM")
		}
		return c.getJSON(ctx, "/v1/items/"+args[1], pretty)
	case "update":
		return c.update(ctx, args[1:], pretty)
	case "find":
		return c.query(ctx, "/v1/items/_find", args[1:], pretty)
	case "note":
		return c.note(ctx, args[1:], pretty)
	case "view":
		return c.view(ctx, args[1:], pretty)
	case "schema":
		return c.schema(ctx, args[1:], pretty)
	case "schemas":
		return c.getJSON(ctx, "/v1/schemas", pretty)
	case "describe":
		if len(args) != 1 {
			return usage("stuff describe")
		}
		return c.getJSON(ctx, "/v1/describe", pretty)
	case "explain":
		return c.query(ctx, "/v1/explain/items", args[1:], pretty)
	default:
		return usage("unknown command " + args[0] + "; run `stuff --help`")
	}
}

func (c *CLI) add(ctx context.Context, args []string) error {
	fs := newFlags("add")
	meta := fs.String("meta", "{}", "JSON or @FILE")
	validate := fs.String("validate", "", "Schema name")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usage("stuff add NAME [--meta JSON|@FILE] [--validate SCHEMA]")
	}
	m, err := readAnyJSONSource(*meta, c.In)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	return c.create(ctx, "/v1/items", Document{"name": fs.Arg(0), "metadata": m, "validate": *validate})
}

func (c *CLI) update(ctx context.Context, args []string, pretty bool) error {
	fs := newFlags("update")
	name := fs.String("name", "", "new name")
	meta := fs.String("meta", "", "JSON or @FILE")
	rev := fs.String("revision", "", "optimistic revision")
	validate := fs.String("validate", "", "Schema name")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usage("stuff update ITEM [--name NAME] [--meta JSON|@FILE] [--revision REV] [--validate SCHEMA]")
	}
	body := Document{"revision": *rev, "validate": *validate}
	if *name != "" {
		body["name"] = *name
	}
	if *meta != "" {
		m, e := readAnyJSONSource(*meta, c.In)
		if e != nil {
			return fmt.Errorf("metadata: %w", e)
		}
		body["metadata"] = m
	}
	return c.requestJSON(ctx, http.MethodPatch, "/v1/items/"+fs.Arg(0), body, pretty, false)
}

type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

func (c *CLI) note(ctx context.Context, args []string, pretty bool) error {
	if len(args) == 0 {
		return usage("stuff note add|get|find ...")
	}
	switch args[0] {
	case "add":
		fs := newFlags("note add")
		meta := fs.String("meta", "{}", "JSON or @FILE")
		var attaches stringsFlag
		fs.Var(&attaches, "attach", "file to attach (repeatable)")
		if err := fs.Parse(flagsFirst(args[1:])); err != nil {
			return err
		}
		if fs.NArg() < 1 || fs.NArg() > 2 {
			return usage("stuff note add ITEM [TEXT] [--meta JSON|@FILE] [--attach FILE ...]")
		}
		m, err := readAnyJSONSource(*meta, c.In)
		if err != nil {
			return fmt.Errorf("metadata: %w", err)
		}
		body := Document{"item_id": fs.Arg(0), "metadata": m}
		if fs.NArg() == 2 {
			body["text"] = fs.Arg(1)
		}
		if len(attaches) > 0 {
			xs := make([]any, 0, len(attaches))
			for _, p := range attaches {
				b, e := os.ReadFile(p)
				if e != nil {
					return fmt.Errorf("attachment %s: %w", p, e)
				}
				mt := mime.TypeByExtension(filepath.Ext(p))
				if mt == "" {
					mt = "application/octet-stream"
				}
				xs = append(xs, Document{"name": filepath.Base(p), "media_type": mt, "data": base64.StdEncoding.EncodeToString(b)})
			}
			body["attachments"] = xs
		}
		return c.create(ctx, "/v1/notes", body)
	case "get":
		if len(args) != 2 {
			return usage("stuff note get NOTE")
		}
		return c.getJSON(ctx, "/v1/notes/"+args[1], pretty)
	case "find":
		return c.query(ctx, "/v1/notes/_find", args[1:], pretty)
	default:
		return usage("stuff note add|get|find ...")
	}
}

func (c *CLI) view(ctx context.Context, args []string, pretty bool) error {
	if len(args) == 0 {
		return usage("stuff view add|get|update ...")
	}
	switch args[0] {
	case "add":
		fs := newFlags("view add")
		schema := fs.String("schema", "", "advisory Schema name")
		if err := fs.Parse(flagsFirst(args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return usage("stuff view add NAME @RENDERER [--schema SCHEMA]")
		}
		renderer, err := readRendererSource(fs.Arg(1), c.In)
		if err != nil {
			return fmt.Errorf("renderer: %w", err)
		}
		body := Document{"name": fs.Arg(0), "renderer": renderer}
		if *schema != "" {
			body["schema"] = *schema
		}
		return c.create(ctx, "/v1/views", body)
	case "get":
		if len(args) != 2 {
			return usage("stuff view get VIEW")
		}
		return c.getJSON(ctx, "/v1/views/"+args[1], pretty)
	case "update":
		fs := newFlags("view update")
		name := fs.String("name", "", "new name")
		schema := fs.String("schema", "", "advisory Schema name")
		clearSchema := fs.Bool("clear-schema", false, "remove the advisory Schema reference")
		rev := fs.String("revision", "", "optimistic revision")
		if err := fs.Parse(flagsFirst(args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 2 || (*schema != "" && *clearSchema) {
			return usage("stuff view update VIEW @RENDERER [--name NAME] [--schema SCHEMA | --clear-schema] [--revision REV]")
		}
		renderer, err := readRendererSource(fs.Arg(1), c.In)
		if err != nil {
			return fmt.Errorf("renderer: %w", err)
		}
		body := Document{"renderer": renderer, "revision": *rev}
		if *name != "" {
			body["name"] = *name
		}
		if *schema != "" {
			body["schema"] = *schema
		} else if *clearSchema {
			body["schema"] = nil
		}
		return c.requestJSON(ctx, http.MethodPatch, "/v1/views/"+fs.Arg(0), body, pretty, false)
	default:
		return usage("stuff view add|get|update ...")
	}
}

func readRendererSource(src string, in io.Reader) (string, error) {
	var b []byte
	var err error
	switch {
	case src == "-":
		b, err = io.ReadAll(in)
	case strings.HasPrefix(src, "@"):
		b, err = os.ReadFile(strings.TrimPrefix(src, "@"))
	default:
		return "", fmt.Errorf("renderer source must be @FILE or - for stdin, got %q", src)
	}
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errors.New("renderer is not valid UTF-8")
	}
	return string(b), nil
}

func (c *CLI) schema(ctx context.Context, args []string, pretty bool) error {
	if len(args) == 0 {
		return usage("stuff schema add|get|check ...")
	}
	switch args[0] {
	case "add":
		if len(args) != 3 {
			return usage("stuff schema add NAME @SCHEMA")
		}
		raw, err := readJSONSource(args[2], c.In)
		if err != nil {
			return fmt.Errorf("schema: %w", err)
		}
		return c.requestJSON(ctx, http.MethodPost, "/v1/schemas", Document{"name": args[1], "schema": raw}, false, true)
	case "get":
		if len(args) != 2 {
			return usage("stuff schema get NAME")
		}
		return c.getJSON(ctx, "/v1/schemas/"+args[1], pretty)
	case "check":
		if len(args) < 2 {
			return usage("stuff schema check NAME ITEM | --meta JSON|@FILE")
		}
		fs := newFlags("schema check")
		meta := fs.String("meta", "", "candidate metadata")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		body := Document{}
		if *meta != "" {
			m, e := readAnyJSONSource(*meta, c.In)
			if e != nil {
				return e
			}
			body["metadata"] = m
		} else if fs.NArg() == 1 {
			body["item_id"] = fs.Arg(0)
		} else {
			return usage("stuff schema check NAME ITEM | --meta JSON|@FILE")
		}
		return c.requestJSON(ctx, http.MethodPost, "/v1/schemas/"+args[1]+"/check", body, pretty, false)
	default:
		return usage("stuff schema add|get|check ...")
	}
}

func (c *CLI) query(ctx context.Context, path string, args []string, pretty bool) error {
	if len(args) > 1 {
		return usage("query accepts one @FILE argument or JSON from stdin")
	}
	var src string
	if len(args) == 1 {
		src = args[0]
	} else {
		src = "-"
	}
	q, err := readJSONSource(src, c.In)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	return c.requestJSON(ctx, http.MethodPost, path, q, pretty, false)
}
func (c *CLI) create(ctx context.Context, path string, body any) error {
	return c.requestJSON(ctx, http.MethodPost, path, body, false, true)
}
func (c *CLI) getJSON(ctx context.Context, path string, pretty bool) error {
	return c.requestJSON(ctx, http.MethodGet, path, nil, pretty, false)
}

func (c *CLI) requestJSON(ctx context.Context, method, path string, body any, pretty, idOnly bool) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("Stuff service: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Path, Reason, Expected string } `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Reason != "" {
			return fmt.Errorf("%s: %s (expected %s)", e.Error.Path, e.Error.Reason, e.Error.Expected)
		}
		return fmt.Errorf("Stuff service returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("Stuff service returned invalid JSON: %w", err)
	}
	if idOnly {
		m, _ := value.(map[string]any)
		id, _ := m["id"].(string)
		if id == "" {
			id, _ = m["name"].(string)
		}
		if id == "" {
			return errors.New("Stuff service create response did not contain an ID")
		}
		fmt.Fprintln(c.Out, id)
		return nil
	}
	var out []byte
	if pretty {
		out, err = json.MarshalIndent(value, "", "  ")
	} else {
		out, err = json.Marshal(value)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, string(out))
	return nil
}

func readAnyJSONSource(src string, in io.Reader) (any, error) {
	var b []byte
	var err error
	switch {
	case src == "-":
		b, err = io.ReadAll(in)
	case strings.HasPrefix(src, "@"):
		b, err = os.ReadFile(strings.TrimPrefix(src, "@"))
	default:
		b = []byte(src)
	}
	if err != nil {
		return nil, err
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("expected exactly one JSON value")
	}
	return out, nil
}

func readJSONSource(src string, in io.Reader) (any, error) {
	out, err := readAnyJSONSource(src, in)
	if err != nil {
		return nil, err
	}
	if _, ok := out.(map[string]any); !ok {
		return nil, errors.New("expected a JSON object")
	}
	return out, nil
}

// flagsFirst supports the documented style where flags follow positional
// arguments. Stuff's current flags all take exactly one value.
func flagsFirst(args []string) []string {
	flags, positional := make([]string, 0, len(args)), make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") && args[i] != "-" {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, args[i])
		}
	}
	return append(flags, positional...)
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

type usageError string

func (e usageError) Error() string { return string(e) }
func usage(s string) error         { return usageError(s) }

const Help = `Stuff stores inert Items, Notes, and Views with arbitrary metadata.

Usage:
  stuff add NAME [--meta JSON|@FILE] [--validate SCHEMA]
  stuff get ITEM
  stuff update ITEM [--name NAME] [--meta JSON|@FILE] [--revision REV] [--validate SCHEMA]
  stuff find [@QUERY | stdin]
  stuff note add ITEM [TEXT] [--meta JSON|@FILE] [--attach FILE ...]
  stuff note get NOTE
  stuff note find [@QUERY | stdin]
  stuff view add NAME @RENDERER [--schema SCHEMA]
  stuff view get VIEW
  stuff view update VIEW @RENDERER [--name NAME] [--schema SCHEMA | --clear-schema] [--revision REV]
  stuff schema add NAME @SCHEMA
  stuff schema get NAME
  stuff schema check NAME ITEM
  stuff schema check NAME --meta JSON|@FILE
  stuff schemas
  stuff describe
  stuff explain [@QUERY | stdin]
  stuff serve

Creates print only the new ID. Other commands emit stable compact JSON; add --pretty
for explicit indentation. Diagnostics go to stderr and failures are nonzero. Queries use
full CouchDB Mango semantics. Stuff records work; it never dispatches or orchestrates it.

Environment: STUFF_URL, STUFF_TOKEN or STUFF_TOKEN_FILE. The serve command also uses STUFF_COUCH_URL,
STUFF_COUCH_DB, STUFF_COUCH_USER, STUFF_COUCH_PASSWORD_FILE, and STUFF_LISTEN.
`

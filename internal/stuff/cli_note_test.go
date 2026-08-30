package stuff

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCLINoteAddReadsMultilineUTF8FromStdin(t *testing.T) {
	var got Document
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/notes" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusCreated, Document{"id": "note_test"})
	}))
	defer ts.Close()

	var out bytes.Buffer
	cli := &CLI{BaseURL: ts.URL, Client: ts.Client(), In: strings.NewReader("first line\nsecond café\n第三行"), Out: &out, Err: io.Discard}
	if err := cli.Run(context.Background(), []string{"note", "add", "item_test", "-"}); err != nil {
		t.Fatal(err)
	}
	if got["item_id"] != "item_test" || got["text"] != "first line\nsecond café\n第三行" {
		t.Fatalf("note request = %#v", got)
	}
}

func TestCLINoteAddRejectsForgottenStdinMarker(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()

	cli := &CLI{BaseURL: "http://127.0.0.1:1", Client: http.DefaultClient, In: read, Out: io.Discard, Err: io.Discard}
	err = cli.Run(context.Background(), []string{"note", "add", "item_test"})
	if err == nil || !strings.Contains(err.Error(), "use `-`") {
		t.Fatalf("error = %v, want stdin marker guidance", err)
	}
}

func TestCLINoteAddAllowsExplicitMetadataOnlyNoteWithRedirectedStdin(t *testing.T) {
	var got Document
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusCreated, Document{"id": "note_meta"})
	}))
	defer ts.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()

	cli := &CLI{BaseURL: ts.URL, Client: ts.Client(), In: read, Out: io.Discard, Err: io.Discard}
	if err := cli.Run(context.Background(), []string{"note", "add", "item_test", "--meta", `{"kind":"repair"}`}); err != nil {
		t.Fatal(err)
	}
	if got["text"] != nil || got["metadata"].(map[string]any)["kind"] != "repair" {
		t.Fatalf("note request = %#v", got)
	}
}

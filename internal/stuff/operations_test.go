package stuff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testOperations() Document {
	return Document{
		"findProject": Document{"request": Document{
			"method": http.MethodPost, "path": "/v1/items/_find",
			"body": Document{"selector": Document{"metadata.project": "$args.project"}, "limit": 20},
		}},
		"updateItem": Document{"request": Document{
			"method": http.MethodPatch, "path": "/v1/items/$args.id",
			"body": Document{"metadata": "$args.metadata", "revision": "$args.revision"},
		}},
	}
}

func putOperationFixture(t *testing.T, store *memoryStore) {
	t.Helper()
	if _, err := store.Create(t.Context(), "view_ops", Document{
		"stuff_kind": "view", "name": "Operations", "renderer": viewHTML,
		"operations": testOperations(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "item_origin", Document{
		"stuff_kind": "item", "name": "Origin", "view_id": "view_ops", "metadata": Document{"project": "home"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestViewOperationsPersistUpdateAndClear(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	operations := testOperations()
	code, created := request(t, h, http.MethodPost, "/v1/views", Document{"name": "Ops", "renderer": viewHTML, "operations": operations})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %#v", code, created)
	}
	id := created["id"].(string)
	code, got := request(t, h, http.MethodGet, "/v1/views/"+id, nil)
	if code != http.StatusOK || string(mustJSON(t, got["operations"])) != string(mustJSON(t, operations)) {
		t.Fatalf("get operations: %d %#v", code, got)
	}
	replacement := Document{"only": Document{"request": Document{"method": "GET", "path": "/v1/items/$args.id"}}}
	code, updated := request(t, h, http.MethodPatch, "/v1/views/"+id, Document{"operations": replacement, "revision": created["revision"]})
	if code != http.StatusOK || string(mustJSON(t, updated["operations"])) != string(mustJSON(t, replacement)) {
		t.Fatalf("update operations: %d %#v", code, updated)
	}
	code, cleared := request(t, h, http.MethodPatch, "/v1/views/"+id, Document{"operations": nil, "revision": updated["revision"]})
	if code != http.StatusOK {
		t.Fatalf("clear operations: %d %#v", code, cleared)
	}
	if _, present := cleared["operations"]; present {
		t.Fatalf("cleared operations remain: %#v", cleared)
	}
}

type operationRecordingStore struct {
	*memoryStore
	query Document
}

func (s *operationRecordingStore) Find(ctx context.Context, kind string, query Document) (Document, error) {
	s.query = cloneDocument(query)
	return s.memoryStore.Find(ctx, kind, query)
}

func TestViewOperationMangoAndRawObjectUpdate(t *testing.T) {
	memory := newMemoryStore()
	putOperationFixture(t, memory)
	store := &operationRecordingStore{memoryStore: memory}
	h := NewServer(store, "secret", nil).Handler()

	code, found := request(t, h, http.MethodPost, "/read/items/item_origin/operation", Document{
		"operation": "findProject", "args": Document{"project": "garden", "ignored": true},
	})
	if code != http.StatusOK || found["docs"] == nil {
		t.Fatalf("find operation: %d %#v", code, found)
	}
	selector := store.query["selector"].(map[string]any)
	if selector["metadata.project"] != "garden" {
		t.Fatalf("Mango template was not substituted: %#v", store.query)
	}

	metadata := Document{"state": "done", "nested": []any{"raw", float64(2)}}
	code, updated := request(t, h, http.MethodPost, "/read/items/item_origin/operation", Document{
		"operation": "updateItem", "args": Document{"id": "item_origin", "metadata": metadata, "revision": "1-test"},
	})
	if code != http.StatusOK || !reflect.DeepEqual(updated["metadata"], map[string]any(metadata)) {
		t.Fatalf("raw object update: %d %#v", code, updated)
	}
	stored, err := memory.Get(t.Context(), "item_origin")
	if err != nil || !reflect.DeepEqual(stored["metadata"], map[string]any(metadata)) {
		t.Fatalf("stored raw object: %#v %v", stored, err)
	}
}

func TestViewOperationSubstitutionAndErrors(t *testing.T) {
	path, err := substituteOperationPath("/v1/items/$args.id/attachments/$args.name", map[string]any{"id": "a/b", "name": "one two?#"})
	if err != nil || path != "/v1/items/a%2Fb/attachments/one%20two%3F%23" {
		t.Fatalf("path escaping: %q %v", path, err)
	}
	partial, err := substituteOperationBody("prefix $args.value", map[string]any{"value": Document{"x": true}})
	if err != nil || partial != "prefix $args.value" {
		t.Fatalf("body partial interpolation occurred: %#v %v", partial, err)
	}

	store := newMemoryStore()
	putOperationFixture(t, store)
	h := NewServer(store, "", nil).Handler()
	cases := []struct {
		name     string
		body     Document
		status   int
		contains string
	}{
		{"missing", Document{"operation": "updateItem", "args": Document{"id": "item_origin", "metadata": Document{}}}, 400, "missing operation argument"},
		{"unknown", Document{"operation": "absent", "args": Document{}}, 404, "unknown View operation"},
		{"non-scalar path", Document{"operation": "updateItem", "args": Document{"id": Document{"not": "scalar"}, "metadata": Document{}, "revision": "1-test"}}, 400, "not a scalar"},
		{"conflict", Document{"operation": "updateItem", "args": Document{"id": "item_origin", "metadata": Document{}, "revision": "9-stale"}}, 409, "revision conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := request(t, h, http.MethodPost, "/read/items/item_origin/operation", tc.body)
			if code != tc.status || !strings.Contains(errReason(out), tc.contains) {
				t.Fatalf("got %d %#v", code, out)
			}
		})
	}
}

func TestViewOperationTemplatesStayOutOfRendererSurfaceAndPromiseProtocol(t *testing.T) {
	store := newMemoryStore()
	putOperationFixture(t, store)
	h := NewServer(store, "", nil).Handler()

	snapshot := htmlRequest(h, http.MethodGet, "/read/items/item_origin/snapshot")
	frame := htmlRequest(h, http.MethodGet, "/read/items/item_origin/view")
	host := htmlRequest(h, http.MethodGet, "/read/items/item_origin")
	for name, body := range map[string]string{"snapshot": snapshot.Body.String(), "frame": frame.Body.String(), "host": host.Body.String()} {
		if strings.Contains(body, "findProject") || strings.Contains(body, "/v1/items/_find") {
			t.Fatalf("operation template leaked in %s: %s", name, body)
		}
	}
	if !strings.Contains(frame.Body.String(), "window.stuff") || !strings.Contains(frame.Body.String(), "return new Promise") || !strings.Contains(frame.Body.String(), "request.resolve(message.result)") || !strings.Contains(frame.Body.String(), "error.status=message.status") || !strings.Contains(frame.Body.String(), "error.result=message.result") {
		t.Fatalf("iframe Promise API lacks result/error behavior: %s", frame.Body.String())
	}
	script := htmlRequest(h, http.MethodGet, "/read/view-host.js").Body.String()
	for _, want := range []string{"stuff:view-operation", "JSON.parse(message.args)", "frame.dataset.operation", "status: response.status", "result"} {
		if !strings.Contains(script, want) {
			t.Fatalf("host operation protocol missing %q: %s", want, script)
		}
	}
}

func TestCLIViewOperationsJSONSetAndClear(t *testing.T) {
	var body Document
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = Document{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusCreated, Document{"id": "view_test"})
		} else {
			writeJSON(w, http.StatusOK, Document{"id": "view_test", "revision": "2-test"})
		}
	}))
	defer ts.Close()
	dir := t.TempDir()
	renderer := filepath.Join(dir, "view.html")
	operations := filepath.Join(dir, "operations.json")
	if err := os.WriteFile(renderer, []byte(viewHTML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(operations, mustJSON(t, testOperations()), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BaseURL: ts.URL, Client: ts.Client(), In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	if err := cli.Run(t.Context(), []string{"view", "add", "Ops", "@" + renderer, "--operations", "@" + operations}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["operations"].(map[string]any); !ok {
		t.Fatalf("create did not send operations: %#v", body)
	}
	if err := cli.Run(t.Context(), []string{"view", "update", "view_test", "--clear-operations"}); err != nil {
		t.Fatal(err)
	}
	if value, present := body["operations"]; !present || value != nil {
		t.Fatalf("clear did not send null: %#v", body)
	}
	if _, present := body["renderer"]; present {
		t.Fatalf("operations-only update unexpectedly sent renderer: %#v", body)
	}
}

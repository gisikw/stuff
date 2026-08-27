package stuff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu          sync.Mutex
	docs        map[string]Document
	revs        map[string]int
	attachments map[string][]byte
}

func newMemoryStore() *memoryStore {
	return &memoryStore{docs: map[string]Document{}, revs: map[string]int{}, attachments: map[string][]byte{}}
}
func (m *memoryStore) Ensure(context.Context) error { return nil }
func (m *memoryStore) Create(_ context.Context, id string, d Document) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.docs[id]; ok {
		return "", &StoreError{Status: 409, Reason: "conflict"}
	}
	m.revs[id] = 1
	rev := "1-test"
	x := cloneDocument(d)
	x["_id"] = id
	x["_rev"] = rev
	m.docs[id] = x
	return rev, nil
}
func (m *memoryStore) Get(_ context.Context, id string) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.docs[id]
	if !ok {
		return nil, &StoreError{Status: 404, Reason: "missing"}
	}
	return cloneDocument(d), nil
}
func (m *memoryStore) Put(_ context.Context, id, rev string, d Document) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.docs[id]
	if !ok {
		return "", &StoreError{Status: 404, Reason: "missing"}
	}
	if old["_rev"] != rev {
		return "", &StoreError{Status: 409, Reason: "Document update conflict."}
	}
	m.revs[id]++
	revision := fmt.Sprintf("%d-test", m.revs[id])
	x := cloneDocument(d)
	x["_id"] = id
	x["_rev"] = revision
	m.docs[id] = x
	return revision, nil
}
func (m *memoryStore) Find(_ context.Context, kind string, q Document) (Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	docs := []any{}
	for _, d := range m.docs {
		if d["stuff_kind"] == kind {
			docs = append(docs, publicDocument(d))
		}
	}
	return Document{"docs": docs, "bookmark": "test"}, nil
}
func (m *memoryStore) Explain(_ context.Context, kind string, q Document) (Document, error) {
	return Document{"kind": kind, "query": q}, nil
}
func (m *memoryStore) Indexes(context.Context) (Document, error) {
	return Document{"indexes": []any{}}, nil
}
func (m *memoryStore) Attachment(_ context.Context, id, name string) (io.ReadCloser, http.Header, error) {
	b, ok := m.attachments[id+"/"+name]
	if !ok {
		return nil, nil, &StoreError{Status: 404, Reason: "missing attachment"}
	}
	h := http.Header{"Content-Type": []string{"application/octet-stream"}}
	return io.NopCloser(bytes.NewReader(b)), h, nil
}

func request(t *testing.T, h http.Handler, method, path string, body any) (int, Document) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out Document
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid response JSON (%d): %s", w.Code, w.Body.String())
	}
	return w.Code, out
}
func errReason(d Document) string {
	e, _ := d["error"].(map[string]any)
	s, _ := e["reason"].(string)
	return s
}

func TestHealthIsPublicButDataRequiresToken(t *testing.T) {
	h := NewServer(newMemoryStore(), "secret", nil).Handler()
	code, health := request(t, h, "GET", "/health", nil)
	if code != 200 || health["status"] != "ok" {
		t.Fatalf("health: %d %#v", code, health)
	}
	code, unauthorized := request(t, h, "GET", "/v1/describe", nil)
	if code != 401 || !strings.Contains(errReason(unauthorized), "bearer") {
		t.Fatalf("auth: %d %#v", code, unauthorized)
	}
}

func TestValidationIsPointInTimeOnly(t *testing.T) {
	store := newMemoryStore()
	srv := NewServer(store, "", nil)
	srv.now = func() time.Time { return time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC) }
	h := srv.Handler()
	schema := Document{"type": "object", "required": []any{"project"}, "properties": Document{"project": Document{"type": "string"}}, "additionalProperties": true}
	code, _ := request(t, h, "POST", "/v1/schemas", Document{"name": "todo", "schema": schema})
	if code != 200 {
		t.Fatalf("schema status %d", code)
	}
	code, badBody := request(t, h, "POST", "/v1/items", Document{"name": "bad", "metadata": Document{}, "validate": "todo"})
	if code != 400 || !strings.Contains(errReason(badBody), "todo") {
		t.Fatalf("expected validation failure, got %d %#v", code, badBody)
	}
	code, created := request(t, h, "POST", "/v1/items", Document{"name": "good", "metadata": Document{"project": "home"}, "validate": "todo"})
	if code != 201 {
		t.Fatalf("create: %d %#v", code, created)
	}
	id := created["id"].(string)
	code, updated := request(t, h, "PATCH", "/v1/items/"+id, Document{"metadata": Document{"location": "garage"}})
	if code != 200 {
		t.Fatalf("unvalidated drift rejected: %d %#v", code, updated)
	}
	doc, _ := store.Get(context.Background(), id)
	if _, ok := doc["schema"]; ok {
		t.Fatal("validation persisted a schema association")
	}
	if _, ok := doc["validate"]; ok {
		t.Fatal("validation persisted conformance state")
	}
}

func TestMetadataAcceptsAnyBoundedJSON(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	code, created := request(t, h, "POST", "/v1/items", Document{"name": "scalar", "metadata": []any{"loose", json.Number("3")}})
	if code != 201 {
		t.Fatalf("create: %d %#v", code, created)
	}
	id := created["id"].(string)
	code, updated := request(t, h, "PATCH", "/v1/items/"+id, Document{"metadata": nil})
	if code != 200 {
		t.Fatalf("null update: %d %#v", code, updated)
	}
	if value, exists := updated["metadata"]; !exists || value != nil {
		t.Fatalf("explicit null was not retained: %#v", updated)
	}
}

func TestMalformedSelectorIsNotSilentlyBroadened(t *testing.T) {
	h := NewServer(newMemoryStore(), "", nil).Handler()
	code, out := request(t, h, "POST", "/v1/items/_find", Document{"selector": "not-an-object"})
	if code != 400 || !strings.Contains(errReason(out), "selector must") {
		t.Fatalf("got %d %#v", code, out)
	}
}

func TestRevisionConflictIsCorrective(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	_, created := request(t, h, "POST", "/v1/items", Document{"name": "x", "metadata": Document{}})
	id := created["id"].(string)
	code, out := request(t, h, "PATCH", "/v1/items/"+id, Document{"name": "y", "revision": "1-wrong"})
	if code != 409 {
		t.Fatalf("status %d: %#v", code, out)
	}
	if !strings.Contains(errReason(out), "revision conflict") {
		t.Fatalf("not corrective: %#v", out)
	}
}

func TestCouchEndpointEscapesAttachmentNamesOnce(t *testing.T) {
	store, err := NewCouchStore("http://stuff:secret@127.0.0.1:5984", "stuff", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := store.endpoint("note_x", "report one.html")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/stuff/note_x/report one.html" {
		t.Fatalf("decoded path: %q (%s)", u.Path, raw)
	}
	if strings.Contains(raw, "%2520") || !strings.Contains(raw, "%20") {
		t.Fatalf("path was not escaped exactly once: %s", raw)
	}
	if strings.Contains(raw, "secret") || strings.Contains(raw, "stuff@") {
		t.Fatalf("credentials leaked into endpoint: %s", raw)
	}
}

func TestPrepareQueryPreservesMangoAndProjection(t *testing.T) {
	q := Document{"selector": Document{"$or": []any{Document{"id": "item_x"}, Document{"metadata": Document{"id": "caller-value"}}}}, "fields": []any{"name"}, "sort": []any{Document{"updated_at": "desc"}}, "bookmark": "opaque"}
	got := prepareQuery("item", q)
	fields := got["fields"].([]any)
	if len(fields) != 1 || fields[0] != "name" {
		t.Fatalf("projection changed: %#v", fields)
	}
	sel := got["selector"].(map[string]any)["$and"].([]any)[1].(map[string]any)
	or := sel["$or"].([]any)
	if _, ok := or[0].(map[string]any)["_id"]; !ok {
		t.Fatalf("public id not mapped: %#v", or[0])
	}
	nested := or[1].(map[string]any)["metadata"].(map[string]any)
	if nested["id"] != "caller-value" {
		t.Fatalf("metadata object mutated: %#v", nested)
	}
	if got["bookmark"] != "opaque" {
		t.Fatal("Mango envelope field dropped")
	}
}

func TestNestedCommandHelpDoesNotContactService(t *testing.T) {
	var stdout bytes.Buffer
	c := &CLI{BaseURL: "http://127.0.0.1:1", Client: http.DefaultClient, In: strings.NewReader(""), Out: &stdout, Err: io.Discard}
	if err := c.Run(context.Background(), []string{"note", "find", "--help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "full CouchDB Mango semantics") {
		t.Fatalf("unexpected help: %s", stdout.String())
	}
}

func TestCLIAddPrintsOnlyIDAndAllowsTrailingFlags(t *testing.T) {
	var seen Document
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, 201, Document{"id": "item_test", "revision": "1-x"})
	}))
	defer ts.Close()
	var stdout, stderr bytes.Buffer
	c := &CLI{BaseURL: ts.URL, Client: ts.Client(), In: strings.NewReader(""), Out: &stdout, Err: &stderr}
	if err := c.Run(context.Background(), []string{"add", "Find my keys", "--meta", `{"project":"home"}`}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "item_test\n" {
		t.Fatalf("stdout contract: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	if seen["name"] != "Find my keys" {
		t.Fatalf("name: %#v", seen)
	}
}

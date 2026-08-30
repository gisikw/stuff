package stuff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const viewHTML = "<!doctype html><html><head><title>Report</title></head><body><p>ok</p><script>inert</script></body></html>"

func TestViewCreateGetRoundTrip(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	code, created := request(t, h, "POST", "/v1/views", Document{"name": "Report", "renderer": viewHTML})
	if code != 201 {
		t.Fatalf("create: %d %#v", code, created)
	}
	id, _ := created["id"].(string)
	if !strings.HasPrefix(id, "view_") {
		t.Fatalf("id: %q", id)
	}
	rev, _ := created["revision"].(string)
	if rev == "" {
		t.Fatalf("no revision: %#v", created)
	}
	code, got := request(t, h, "GET", "/v1/views/"+id, nil)
	if code != 200 {
		t.Fatalf("get: %d %#v", code, got)
	}
	if got["id"] != id || got["name"] != "Report" || got["renderer"] != viewHTML {
		t.Fatalf("fields: %#v", got)
	}
	if got["created_at"] == nil || got["updated_at"] == nil || got["revision"] != rev {
		t.Fatalf("envelope: %#v", got)
	}
	if _, ok := got["stuff_kind"]; ok {
		t.Fatal("internal stuff_kind leaked into public document")
	}
	code, out := request(t, h, "GET", "/v1/views/view_missing", nil)
	if code != 404 {
		t.Fatalf("missing view: %d %#v", code, out)
	}
}

func TestViewUpdateAdvancesRevisionAndRejectsConflicts(t *testing.T) {
	store := newMemoryStore()
	srv := NewServer(store, "", nil)
	srv.now = func() time.Time { return time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC) }
	h := srv.Handler()
	_, created := request(t, h, "POST", "/v1/views", Document{"name": "Report", "renderer": viewHTML})
	id := created["id"].(string)
	firstRev := created["revision"].(string)

	code, out := request(t, h, "PATCH", "/v1/views/"+id, Document{"renderer": "<html>x</html>", "revision": "9-wrong"})
	if code != 409 || !strings.Contains(errReason(out), "revision conflict") {
		t.Fatalf("conflict: %d %#v", code, out)
	}

	second := "<!doctype html><html><body>two</body></html>"
	code, updated := request(t, h, "PATCH", "/v1/views/"+id, Document{"name": "Report 2", "renderer": second, "revision": firstRev})
	if code != 200 {
		t.Fatalf("update: %d %#v", code, updated)
	}
	if updated["name"] != "Report 2" || updated["renderer"] != second {
		t.Fatalf("fields: %#v", updated)
	}
	if updated["created_at"] != "2026-08-27T03:00:00Z" || updated["updated_at"] != "2026-08-27T03:00:00Z" {
		t.Fatalf("timestamps: %#v", updated)
	}
	newRev, _ := updated["revision"].(string)
	if newRev == "" || newRev == firstRev {
		t.Fatalf("revision not advanced: %#v", updated)
	}

	code, out = request(t, h, "PATCH", "/v1/views/"+id, Document{"name": "Report 3", "revision": firstRev})
	if code != 409 {
		t.Fatalf("stale revision after update: %d %#v", code, out)
	}

	code, out = request(t, h, "PATCH", "/v1/views/"+id, Document{"revision": newRev})
	if code != 400 || !strings.Contains(errReason(out), "changes nothing") {
		t.Fatalf("empty update: %d %#v", code, out)
	}

	code, out = request(t, h, "PATCH", "/v1/views/"+id, Document{"name": "  "})
	if code != 400 || !strings.Contains(errReason(out), "name cannot be empty") {
		t.Fatalf("empty name: %d %#v", code, out)
	}

	itemStore := newMemoryStore()
	if _, err := itemStore.Create(context.Background(), "item_x", Document{"stuff_kind": "item", "name": "x", "created_at": "t", "updated_at": "t"}); err != nil {
		t.Fatal(err)
	}
	other := NewServer(itemStore, "", nil).Handler()
	code, out = request(t, other, "PATCH", "/v1/views/item_x", Document{"renderer": viewHTML})
	if code != 404 {
		t.Fatalf("kind boundary: %d %#v", code, out)
	}
}

func TestViewRendererMustBeBoundedUTF8(t *testing.T) {
	if err := validRenderer("\xff\xfe"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 accepted: %v", err)
	}
	if err := validRenderer(""); err == nil {
		t.Fatal("empty renderer accepted")
	}
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()

	big := strings.Repeat("a", MaxRendererBytes+1)
	code, out := request(t, h, "POST", "/v1/views", Document{"name": "Big", "renderer": big})
	if code != 400 || !strings.Contains(errReason(out), "renderer exceeds") {
		t.Fatalf("oversized: %d %#v", code, out)
	}
	code, out = request(t, h, "PATCH", "/v1/views/view_missing", Document{"renderer": big})
	if code != 404 {
		t.Fatalf("oversized update on missing view: %d %#v", code, out)
	}

	// encoding/json normally replaces invalid UTF-8 in JSON strings with
	// U+FFFD. View endpoints reject the raw body first so malformed renderer
	// bytes cannot silently change while crossing the API boundary.
	raw := append(append([]byte(`{"name":"Bad","renderer":"`), 0xff, 0xfe), []byte(`"}`)...)
	req := httptest.NewRequest(http.MethodPost, "/v1/views", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "not valid UTF-8") {
		t.Fatalf("non-UTF8 body: %d %s", w.Code, w.Body.String())
	}
}

func TestViewSchemaReferenceRoundTrip(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	code, _ := request(t, h, "POST", "/v1/schemas", Document{"name": "report-html", "schema": Document{"type": "object"}})
	if code != 200 {
		t.Fatalf("schema: %d", code)
	}

	code, created := request(t, h, "POST", "/v1/views", Document{"name": "Report", "renderer": viewHTML, "schema": "report-html"})
	if code != 201 {
		t.Fatalf("create: %d %#v", code, created)
	}
	id := created["id"].(string)
	code, got := request(t, h, "GET", "/v1/views/"+id, nil)
	if code != 200 || got["schema"] != "report-html" {
		t.Fatalf("schema not round-tripped: %d %#v", code, got)
	}

	code, out := request(t, h, "POST", "/v1/views", Document{"name": "Bad", "renderer": viewHTML, "schema": "missing-schema"})
	if code != 400 || !strings.Contains(errReason(out), "does not exist") {
		t.Fatalf("unknown schema: %d %#v", code, out)
	}

	code, _ = request(t, h, "POST", "/v1/schemas", Document{"name": "report-v2", "schema": Document{"type": "object"}})
	if code != 200 {
		t.Fatalf("schema v2: %d", code)
	}
	code, updated := request(t, h, "PATCH", "/v1/views/"+id, Document{"schema": "report-v2", "revision": created["revision"].(string)})
	if code != 200 || updated["schema"] != "report-v2" {
		t.Fatalf("schema update: %d %#v", code, updated)
	}
	code, out = request(t, h, "PATCH", "/v1/views/"+id, Document{"schema": "missing-schema"})
	if code != 400 || !strings.Contains(errReason(out), "does not exist") {
		t.Fatalf("unknown schema on update: %d %#v", code, out)
	}
	code, updated = request(t, h, "PATCH", "/v1/views/"+id, Document{"schema": nil})
	if code != 200 {
		t.Fatalf("clear schema: %d %#v", code, updated)
	}
	if _, ok := updated["schema"]; ok {
		t.Fatalf("cleared schema still present: %#v", updated)
	}

	code, plain := request(t, h, "POST", "/v1/views", Document{"name": "Plain", "renderer": viewHTML})
	if code != 201 {
		t.Fatalf("plain create: %d %#v", code, plain)
	}
	code, plainGot := request(t, h, "GET", "/v1/views/"+plain["id"].(string), nil)
	if code != 200 {
		t.Fatalf("plain get: %d", code)
	}
	if _, ok := plainGot["schema"]; ok {
		t.Fatalf("schema field present without a reference: %#v", plainGot)
	}
}

func TestViewCapabilitiesAreExplicitValidatedAndClearable(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	code, created := request(t, h, "POST", "/v1/views", Document{
		"name": "Projection", "renderer": viewHTML,
		"capabilities": []any{"find_notes", "find_items", "find_notes"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create with capabilities: %d %#v", code, created)
	}
	id := created["id"].(string)
	code, got := request(t, h, "GET", "/v1/views/"+id, nil)
	capabilities, _ := got["capabilities"].([]any)
	if code != http.StatusOK || len(capabilities) != 2 || capabilities[0] != "find_items" || capabilities[1] != "find_notes" {
		t.Fatalf("capabilities not canonical: %d %#v", code, got)
	}

	var out Document
	for _, unsupported := range []string{"mutate_items", "update_linked_status"} {
		code, out = request(t, h, "POST", "/v1/views", Document{"name": "Bad", "renderer": viewHTML, "capabilities": []any{unsupported}})
		if code != http.StatusBadRequest || !strings.Contains(errReason(out), "unknown View capability") {
			t.Fatalf("unsupported mutation capability %q accepted: %d %#v", unsupported, code, out)
		}
	}
	code, out = request(t, h, "PATCH", "/v1/views/"+id, Document{"capabilities": "find_items"})
	if code != http.StatusBadRequest || !strings.Contains(errReason(out), "must be an array") {
		t.Fatalf("non-array capabilities accepted: %d %#v", code, out)
	}
	code, cleared := request(t, h, "PATCH", "/v1/views/"+id, Document{"capabilities": nil, "revision": created["revision"]})
	if code != http.StatusOK {
		t.Fatalf("clear capabilities: %d %#v", code, cleared)
	}
	if _, present := cleared["capabilities"]; present {
		t.Fatalf("cleared capabilities still present: %#v", cleared)
	}
}

func TestCLIViewCommandsFollowConventions(t *testing.T) {
	var lastBody Document
	lastPath, lastMethod := "", ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath, lastMethod = r.URL.Path, r.Method
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			lastBody = Document{}
			_ = json.NewDecoder(r.Body).Decode(&lastBody)
		}
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, 201, Document{"id": "view_test", "revision": "1-x"})
		case http.MethodPatch:
			writeJSON(w, 200, Document{"id": "view_test", "name": "Report 2", "renderer": viewHTML, "revision": "2-x"})
		default:
			writeJSON(w, 200, Document{"id": "view_test", "name": "Report", "renderer": viewHTML, "revision": "1-x"})
		}
	}))
	defer ts.Close()
	file := filepath.Join(t.TempDir(), "report.html")
	if err := os.WriteFile(file, []byte(viewHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	c := &CLI{BaseURL: ts.URL, Client: ts.Client(), In: strings.NewReader(""), Out: &stdout, Err: &stderr}

	if err := c.Run(context.Background(), []string{"view", "add", "Report", "@" + file, "--schema", "report-html", "--capabilities", "find_notes,find_items"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "view_test\n" {
		t.Fatalf("create stdout contract: %q", stdout.String())
	}
	if lastMethod != http.MethodPost || lastPath != "/v1/views" {
		t.Fatalf("create target: %s %s", lastMethod, lastPath)
	}
	if lastBody["name"] != "Report" || lastBody["renderer"] != viewHTML || lastBody["schema"] != "report-html" {
		t.Fatalf("create body: %#v", lastBody)
	}
	capabilities, _ := lastBody["capabilities"].([]any)
	if len(capabilities) != 2 || capabilities[0] != "find_notes" || capabilities[1] != "find_items" {
		t.Fatalf("create capabilities: %#v", lastBody)
	}

	stdout.Reset()
	if err := c.Run(context.Background(), []string{"view", "get", "view_test"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), `{"id":"view_test"`) {
		t.Fatalf("compact JSON contract: %q", stdout.String())
	}

	stdout.Reset()
	if err := c.Run(context.Background(), []string{"view", "update", "view_test", "@" + file, "--name", "Report 2", "--revision", "1-x"}); err != nil {
		t.Fatal(err)
	}
	if lastMethod != http.MethodPatch || lastPath != "/v1/views/view_test" {
		t.Fatalf("update target: %s %s", lastMethod, lastPath)
	}
	if lastBody["renderer"] != viewHTML || lastBody["name"] != "Report 2" || lastBody["revision"] != "1-x" {
		t.Fatalf("update body: %#v", lastBody)
	}
	if _, ok := lastBody["schema"]; ok {
		t.Fatalf("omitted --schema must not be sent: %#v", lastBody)
	}

	stdout.Reset()
	if err := c.Run(context.Background(), []string{"view", "update", "view_test", "@" + file, "--clear-schema", "--clear-capabilities"}); err != nil {
		t.Fatal(err)
	}
	if value, ok := lastBody["schema"]; !ok || value != nil {
		t.Fatalf("--clear-schema must send null: %#v", lastBody)
	}
	if value, ok := lastBody["capabilities"]; !ok || value != nil {
		t.Fatalf("--clear-capabilities must send null: %#v", lastBody)
	}
	if err := c.Run(context.Background(), []string{"view", "update", "view_test", "@" + file, "--schema", "report-html", "--clear-schema"}); err == nil {
		t.Fatal("combined --schema and --clear-schema accepted")
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	bad := filepath.Join(t.TempDir(), "binary.html")
	if err := os.WriteFile(bad, append([]byte{0xff, 0xfe}, []byte("<html>")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(context.Background(), []string{"view", "add", "Bad", "@" + bad}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("non-UTF8 renderer file accepted: %v", err)
	}
}

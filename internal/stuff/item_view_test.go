package stuff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestView(t *testing.T, h http.Handler, name string) string {
	t.Helper()
	code, created := request(t, h, "POST", "/v1/views", Document{"name": name, "renderer": viewHTML})
	if code != 201 {
		t.Fatalf("create view %q: %d %#v", name, code, created)
	}
	return created["id"].(string)
}

func TestItemViewReferenceRoundTrip(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	viewA := newTestView(t, h, "A")
	viewB := newTestView(t, h, "B")

	// Omitted on create leaves the reference absent from the envelope.
	code, created := request(t, h, "POST", "/v1/items", Document{"name": "plain", "metadata": Document{}})
	if code != 201 {
		t.Fatalf("plain create: %d %#v", code, created)
	}
	code, got := request(t, h, "GET", "/v1/items/"+created["id"].(string), nil)
	if code != 200 {
		t.Fatalf("plain get: %d %#v", code, got)
	}
	if _, ok := got["view_id"]; ok {
		t.Fatalf("view_id present without a reference: %#v", got)
	}

	// Explicit null on create is treated as absent.
	code, created = request(t, h, "POST", "/v1/items", Document{"name": "null", "view_id": nil})
	if code != 201 {
		t.Fatalf("null create: %d %#v", code, created)
	}
	code, got = request(t, h, "GET", "/v1/items/"+created["id"].(string), nil)
	if code != 200 {
		t.Fatalf("null get: %d %#v", code, got)
	}
	if _, ok := got["view_id"]; ok {
		t.Fatalf("view_id present after null create: %#v", got)
	}

	// A non-empty reference round-trips through create and get.
	code, created = request(t, h, "POST", "/v1/items", Document{"name": "linked", "metadata": Document{"area": "home"}, "view_id": viewA})
	if code != 201 {
		t.Fatalf("linked create: %d %#v", code, created)
	}
	id := created["id"].(string)
	code, got = request(t, h, "GET", "/v1/items/"+id, nil)
	if code != 200 || got["view_id"] != viewA {
		t.Fatalf("round trip: %d %#v", code, got)
	}
	if meta, _ := got["metadata"].(map[string]any); meta["area"] != "home" {
		t.Fatalf("metadata lost alongside reference: %#v", got)
	}

	// Update sets the reference; metadata is untouched.
	code, updated := request(t, h, "PATCH", "/v1/items/"+id, Document{"view_id": viewB, "revision": created["revision"].(string)})
	if code != 200 || updated["view_id"] != viewB {
		t.Fatalf("update set: %d %#v", code, updated)
	}
	if meta, _ := updated["metadata"].(map[string]any); meta["area"] != "home" {
		t.Fatalf("metadata clobbered by reference update: %#v", updated)
	}

	// Omitting view_id on update preserves the existing reference.
	code, updated = request(t, h, "PATCH", "/v1/items/"+id, Document{"name": "renamed", "revision": updated["revision"].(string)})
	if code != 200 || updated["view_id"] != viewB {
		t.Fatalf("update omission lost reference: %d %#v", code, updated)
	}

	// Explicit null clears the reference.
	code, updated = request(t, h, "PATCH", "/v1/items/"+id, Document{"view_id": nil})
	if code != 200 {
		t.Fatalf("update clear: %d %#v", code, updated)
	}
	if _, ok := updated["view_id"]; ok {
		t.Fatalf("cleared view_id still present: %#v", updated)
	}
	code, got = request(t, h, "GET", "/v1/items/"+id, nil)
	if code != 200 {
		t.Fatalf("post-clear get: %d %#v", code, got)
	}
	if _, ok := got["view_id"]; ok {
		t.Fatalf("cleared view_id persisted: %#v", got)
	}
}

func TestItemViewReferenceRejectsUnknownAndWrongKind(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	newTestView(t, h, "Real")
	code, created := request(t, h, "POST", "/v1/items", Document{"name": "item", "metadata": Document{}})
	if code != 201 {
		t.Fatalf("item: %d %#v", code, created)
	}
	itemID := created["id"].(string)
	code, noteCreated := request(t, h, "POST", "/v1/notes", Document{"item_id": itemID, "metadata": Document{}})
	if code != 201 {
		t.Fatalf("note: %d %#v", code, noteCreated)
	}
	noteID := noteCreated["id"].(string)
	code, _ = request(t, h, "POST", "/v1/schemas", Document{"name": "todo", "schema": Document{"type": "object"}})
	if code != 200 {
		t.Fatalf("schema: %d", code)
	}

	cases := []struct{ label, ref, want string }{
		{"unknown view", "view_missing", "does not exist"},
		{"item ID", itemID, "must reference a View"},
		{"note ID", noteID, "must reference a View"},
		{"schema ID", "schema:todo", "must reference a View"},
	}
	for _, tc := range cases {
		code, out := request(t, h, "POST", "/v1/items", Document{"name": "bad", "view_id": tc.ref})
		if code != 400 || !strings.Contains(errReason(out), tc.want) {
			t.Fatalf("create %s: %d %#v", tc.label, code, out)
		}
		code, out = request(t, h, "PATCH", "/v1/items/"+itemID, Document{"view_id": tc.ref})
		if code != 400 || !strings.Contains(errReason(out), tc.want) {
			t.Fatalf("update %s: %d %#v", tc.label, code, out)
		}
	}

	// Rejected writes leave the stored Item untouched.
	code, got := request(t, h, "GET", "/v1/items/"+itemID, nil)
	if code != 200 {
		t.Fatalf("get after rejections: %d %#v", code, got)
	}
	if _, ok := got["view_id"]; ok {
		t.Fatalf("rejected reference persisted: %#v", got)
	}

	// Empty strings and non-string values are structured errors on both paths.
	code, out := request(t, h, "POST", "/v1/items", Document{"name": "empty", "view_id": ""})
	if code != 400 || !strings.Contains(errReason(out), "cannot be empty") {
		t.Fatalf("empty create: %d %#v", code, out)
	}
	code, out = request(t, h, "PATCH", "/v1/items/"+itemID, Document{"view_id": ""})
	if code != 400 || !strings.Contains(errReason(out), "cannot be empty") {
		t.Fatalf("empty update: %d %#v", code, out)
	}
	code, out = request(t, h, "POST", "/v1/items", Document{"name": "typed", "view_id": 42})
	if code != 400 || !strings.Contains(errReason(out), "must be a string or null") {
		t.Fatalf("non-string create: %d %#v", code, out)
	}
	code, out = request(t, h, "PATCH", "/v1/items/"+itemID, Document{"view_id": Document{"id": "view_x"}})
	if code != 400 || !strings.Contains(errReason(out), "must be a string or null") {
		t.Fatalf("non-string update: %d %#v", code, out)
	}
}

func TestItemViewReferenceRevisionConflict(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	viewA := newTestView(t, h, "A")
	viewB := newTestView(t, h, "B")
	_, created := request(t, h, "POST", "/v1/items", Document{"name": "conflict", "view_id": viewA})
	id := created["id"].(string)
	firstRev := created["revision"].(string)

	code, out := request(t, h, "PATCH", "/v1/items/"+id, Document{"view_id": viewB, "revision": "9-wrong"})
	if code != 409 || !strings.Contains(errReason(out), "revision conflict") {
		t.Fatalf("conflict: %d %#v", code, out)
	}
	code, updated := request(t, h, "PATCH", "/v1/items/"+id, Document{"view_id": viewB, "revision": firstRev})
	if code != 200 || updated["view_id"] != viewB {
		t.Fatalf("update: %d %#v", code, updated)
	}
	code, out = request(t, h, "PATCH", "/v1/items/"+id, Document{"view_id": nil, "revision": firstRev})
	if code != 409 {
		t.Fatalf("stale revision after update: %d %#v", code, out)
	}
}

func TestCLIItemViewFlagsBuildExpectedBodies(t *testing.T) {
	var lastBody Document
	lastPath, lastMethod := "", ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath, lastMethod = r.URL.Path, r.Method
		lastBody = Document{}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&lastBody)
		}
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, 201, Document{"id": "item_test", "revision": "1-x"})
		default:
			writeJSON(w, 200, Document{"id": "item_test", "name": "n", "revision": "2-x"})
		}
	}))
	defer ts.Close()
	var stdout, stderr bytes.Buffer
	c := &CLI{BaseURL: ts.URL, Client: ts.Client(), In: strings.NewReader(""), Out: &stdout, Err: &stderr}

	if err := c.Run(context.Background(), []string{"add", "Linked", "--meta", `{"area":"home"}`, "--view", "view_test"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "item_test\n" {
		t.Fatalf("create stdout contract: %q", stdout.String())
	}
	if lastMethod != http.MethodPost || lastPath != "/v1/items" {
		t.Fatalf("create target: %s %s", lastMethod, lastPath)
	}
	if lastBody["view_id"] != "view_test" {
		t.Fatalf("add body: %#v", lastBody)
	}
	if meta, _ := lastBody["metadata"].(map[string]any); meta["area"] != "home" {
		t.Fatalf("add metadata: %#v", lastBody)
	}

	stdout.Reset()
	if err := c.Run(context.Background(), []string{"add", "Plain"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := lastBody["view_id"]; ok {
		t.Fatalf("omitted --view must not be sent: %#v", lastBody)
	}

	stdout.Reset()
	if err := c.Run(context.Background(), []string{"update", "item_test", "--view", "view_other", "--revision", "1-x"}); err != nil {
		t.Fatal(err)
	}
	if lastMethod != http.MethodPatch || lastPath != "/v1/items/item_test" {
		t.Fatalf("update target: %s %s", lastMethod, lastPath)
	}
	if lastBody["view_id"] != "view_other" || lastBody["revision"] != "1-x" {
		t.Fatalf("update body: %#v", lastBody)
	}

	stdout.Reset()
	if err := c.Run(context.Background(), []string{"update", "item_test", "--clear-view"}); err != nil {
		t.Fatal(err)
	}
	if value, ok := lastBody["view_id"]; !ok || value != nil {
		t.Fatalf("--clear-view must send null: %#v", lastBody)
	}

	err := c.Run(context.Background(), []string{"update", "item_test", "--view", "view_test", "--clear-view"})
	if err == nil {
		t.Fatal("combined --view and --clear-view accepted")
	}
	if !strings.Contains(err.Error(), "--view VIEW | --clear-view") {
		t.Fatalf("combined flags diagnostic: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestMetadataViewKeysDoNotTriggerViewBehavior(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	view := newTestView(t, h, "Real")

	// Arbitrary metadata keys named view/view_id/status are inert data, even
	// when they name a nonexistent View or mimic the reference field.
	meta := Document{"view": view, "view_id": "view_missing", "status": "done", "view_name": "Report"}
	code, created := request(t, h, "POST", "/v1/items", Document{"name": "inert", "metadata": meta})
	if code != 201 {
		t.Fatalf("create: %d %#v", code, created)
	}
	id := created["id"].(string)
	code, got := request(t, h, "GET", "/v1/items/"+id, nil)
	if code != 200 {
		t.Fatalf("get: %d %#v", code, got)
	}
	if _, ok := got["view_id"]; ok {
		t.Fatalf("metadata view_id promoted to the envelope: %#v", got)
	}
	gotMeta, _ := got["metadata"].(map[string]any)
	if gotMeta["view_id"] != "view_missing" || gotMeta["view"] != view || gotMeta["status"] != "done" {
		t.Fatalf("metadata not preserved verbatim: %#v", got)
	}

	// Updating metadata with those keys neither sets nor clears the reference.
	code, updated := request(t, h, "PATCH", "/v1/items/"+id, Document{"metadata": Document{"view_id": nil, "status": "blocked"}})
	if code != 200 {
		t.Fatalf("update: %d %#v", code, updated)
	}
	if _, ok := updated["view_id"]; ok {
		t.Fatalf("metadata view_id null touched the envelope: %#v", updated)
	}

	// An explicit top-level reference still works alongside inert metadata.
	code, updated = request(t, h, "PATCH", "/v1/items/"+id, Document{"view_id": view, "metadata": meta})
	if code != 200 || updated["view_id"] != view {
		t.Fatalf("reference with inert metadata: %d %#v", code, updated)
	}
	if gotMeta, _ = updated["metadata"].(map[string]any); gotMeta["view_id"] != "view_missing" {
		t.Fatalf("metadata overwritten by reference handling: %#v", updated)
	}
}

func TestDescribeEnvelopesListItemViewReference(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	code, out := request(t, h, "GET", "/v1/describe", nil)
	if code != 200 {
		t.Fatalf("describe: %d %#v", code, out)
	}
	envelopes, _ := out["envelopes"].(map[string]any)
	item, _ := envelopes["item"].([]any)
	if item == nil {
		t.Fatalf("missing item envelope: %#v", out)
	}
	found := false
	for _, field := range item {
		if field == "view_id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("item envelope lacks view_id: %#v", item)
	}
}

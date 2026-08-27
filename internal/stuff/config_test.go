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

func TestReaderConfigAbsentGetAndClearDoNotCreateDocument(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()

	code, got := request(t, h, http.MethodGet, "/v1/config/reader", nil)
	if code != http.StatusOK || got["home_item_id"] != nil || got["revision"] != nil || got["updated_at"] != nil {
		t.Fatalf("default config envelope: %d %#v", code, got)
	}
	if _, err := store.Get(t.Context(), readerConfigID); !isStoreNotFound(err) {
		t.Fatalf("GET created ReaderConfig: %v", err)
	}

	code, cleared := request(t, h, http.MethodPatch, "/v1/config/reader", Document{"home_item_id": nil})
	if code != http.StatusOK || cleared["home_item_id"] != nil || cleared["revision"] != nil {
		t.Fatalf("absent clear: %d %#v", code, cleared)
	}
	if _, err := store.Get(t.Context(), readerConfigID); !isStoreNotFound(err) {
		t.Fatalf("absent clear created ReaderConfig: %v", err)
	}
}

func TestReaderConfigSetGetConflictClearAndFindIsolation(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_home", "item", "Home", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	h := NewServer(store, "", nil).Handler()

	code, set := request(t, h, http.MethodPatch, "/v1/config/reader", Document{"home_item_id": "item_home"})
	if code != http.StatusOK || set["home_item_id"] != "item_home" || set["revision"] == nil || set["updated_at"] == nil {
		t.Fatalf("first set: %d %#v", code, set)
	}
	revision := set["revision"].(string)

	code, got := request(t, h, http.MethodGet, "/v1/config/reader", nil)
	if code != http.StatusOK || got["home_item_id"] != "item_home" || got["revision"] != revision {
		t.Fatalf("get set config: %d %#v", code, got)
	}
	if got["id"] != nil || got["stuff_kind"] != nil {
		t.Fatalf("internal config identity leaked: %#v", got)
	}

	code, conflict := request(t, h, http.MethodPatch, "/v1/config/reader", Document{"home_item_id": "item_home", "revision": "9-wrong"})
	if code != http.StatusConflict || !strings.Contains(errReason(conflict), "revision conflict") {
		t.Fatalf("config conflict: %d %#v", code, conflict)
	}

	code, cleared := request(t, h, http.MethodPatch, "/v1/config/reader", Document{"home_item_id": nil, "revision": revision})
	if code != http.StatusOK || cleared["home_item_id"] != nil || cleared["revision"] == revision {
		t.Fatalf("clear config: %d %#v", code, cleared)
	}
	clearedRevision := cleared["revision"]
	code, clearedAgain := request(t, h, http.MethodPatch, "/v1/config/reader", Document{"home_item_id": nil, "revision": "stale-is-irrelevant-for-noop"})
	if code != http.StatusOK || clearedAgain["revision"] != clearedRevision {
		t.Fatalf("idempotent clear mutated or conflicted: %d %#v", code, clearedAgain)
	}

	code, found := request(t, h, http.MethodPost, "/v1/items/_find", Document{"selector": Document{}, "limit": 200})
	if code != http.StatusOK {
		t.Fatalf("find Items: %d %#v", code, found)
	}
	docs, _ := found["docs"].([]any)
	for _, raw := range docs {
		doc, _ := raw.(map[string]any)
		if doc["id"] == readerConfigID {
			t.Fatalf("ReaderConfig leaked into Item find: %#v", found)
		}
	}
}

func TestReaderConfigRejectsInvalidHomeReferences(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_valid", "item", "Valid", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	putReadFixture(t, store, "note_wrong", "note", "", "item_valid", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "wrong")
	h := NewServer(store, "", nil).Handler()

	cases := []struct {
		value any
		want  string
	}{
		{"", "cannot be empty"},
		{42, "string or null"},
		{"item_missing", "does not exist"},
		{"note_wrong", "must reference an Item"},
	}
	for _, tc := range cases {
		code, out := request(t, h, http.MethodPatch, "/v1/config/reader", Document{"home_item_id": tc.value})
		if code != http.StatusBadRequest || !strings.Contains(errReason(out), tc.want) {
			t.Fatalf("value %#v: %d %#v", tc.value, code, out)
		}
	}
	code, out := request(t, h, http.MethodPatch, "/v1/config/reader", Document{"revision": "1-x"})
	if code != http.StatusBadRequest || !strings.Contains(errReason(out), "changes nothing") {
		t.Fatalf("missing home_item_id: %d %#v", code, out)
	}
}

func TestReaderConfigAPIIsProtected(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "secret-token", nil).Handler()

	unauthorized := htmlRequest(h, http.MethodGet, "/v1/config/reader")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("config API unexpectedly public: %d %s", unauthorized.Code, unauthorized.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/config/reader", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized config GET: %d %s", w.Code, w.Body.String())
	}
}

func TestHomepageDefaultConfiguredGenericCustomAndStale(t *testing.T) {
	store := newMemoryStore()
	h := NewServer(store, "", nil).Handler()
	root := htmlRequest(h, http.MethodGet, "/")
	if root.Code != http.StatusSeeOther || root.Header().Get("Location") != "/read" {
		t.Fatalf("default home: %d %#v", root.Code, root.Header())
	}

	putReadFixture(t, store, "item_home", "item", "Generic Home", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	if _, err := store.Create(t.Context(), readerConfigID, Document{"stuff_kind": "reader_config", "home_item_id": "item_home", "updated_at": "2026-08-27T01:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	root = htmlRequest(h, http.MethodGet, "/")
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "Generic Home") || !strings.Contains(root.Body.String(), "<h2>Metadata</h2>") {
		t.Fatalf("configured generic home: %d %s", root.Code, root.Body.String())
	}

	putViewUIFixture(t, store, "item_custom_home", "view_custom_home", Document{"home": true})
	store.mu.Lock()
	store.docs[readerConfigID]["home_item_id"] = "item_custom_home"
	store.mu.Unlock()
	root = htmlRequest(h, http.MethodGet, "/")
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), `<iframe id="stuff-view"`) || !strings.Contains(root.Body.String(), "/read/items/item_custom_home/view") {
		t.Fatalf("configured View home: %d %s", root.Code, root.Body.String())
	}

	store.mu.Lock()
	store.docs[readerConfigID]["home_item_id"] = "item_missing"
	store.mu.Unlock()
	root = htmlRequest(h, http.MethodGet, "/")
	if root.Code != http.StatusSeeOther || root.Header().Get("Location") != "/read" {
		t.Fatalf("stale home did not fall back: %d %#v %s", root.Code, root.Header(), root.Body.String())
	}
}

func TestCLIReaderConfigCommands(t *testing.T) {
	var method, path string
	var body Document
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		writeJSON(w, http.StatusOK, Document{"home_item_id": body["home_item_id"], "revision": "2-x", "updated_at": "now"})
	}))
	defer ts.Close()
	var stdout, stderr bytes.Buffer
	cli := &CLI{BaseURL: ts.URL, Client: ts.Client(), In: strings.NewReader(""), Out: &stdout, Err: &stderr}

	if err := cli.Run(context.Background(), []string{"config", "get"}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/v1/config/reader" {
		t.Fatalf("config get target: %s %s", method, path)
	}
	stdout.Reset()
	body = nil
	if err := cli.Run(context.Background(), []string{"config", "set-home", "item_home", "--revision", "1-x"}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || path != "/v1/config/reader" || body["home_item_id"] != "item_home" || body["revision"] != "1-x" {
		t.Fatalf("set-home request: %s %s %#v", method, path, body)
	}
	stdout.Reset()
	body = nil
	if err := cli.Run(context.Background(), []string{"config", "clear-home"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["home_item_id"]; !ok || body["home_item_id"] != nil {
		t.Fatalf("clear-home request: %#v", body)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

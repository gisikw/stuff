package stuff

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func htmlRequest(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func putReadFixture(t *testing.T, store *memoryStore, id, kind, name, itemID, created, updated, text string) {
	t.Helper()
	doc := Document{"stuff_kind": kind, "created_at": created, "updated_at": updated, "metadata": Document{}}
	if name != "" {
		doc["name"] = name
	}
	if itemID != "" {
		doc["item_id"] = itemID
	}
	if text != "" {
		doc["text"] = text
	}
	if _, err := store.Create(t.Context(), id, doc); err != nil {
		t.Fatal(err)
	}
}

func TestReadSurfaceIsPublicButAPIRemainsProtected(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_public", "item", "Visible after identity gate", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	h := NewServer(store, "secret", nil).Handler()

	read := htmlRequest(h, http.MethodGet, "/read")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), "Visible after identity gate") {
		t.Fatalf("read surface: %d %s", read.Code, read.Body.String())
	}
	if got := read.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("missing restrictive CSP: %q", got)
	}

	api := htmlRequest(h, http.MethodGet, "/v1/describe")
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("API unexpectedly public: %d %s", api.Code, api.Body.String())
	}
}

func TestReadActivityIncludesLinkedNotesAndSortsDeterministically(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_a", "item", "Older item, newer note", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	putReadFixture(t, store, "item_b", "item", "Newer item", "", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "")
	putReadFixture(t, store, "note_a", "note", "", "item_a", "2026-08-27T03:00:00Z", "2026-08-27T03:00:00Z", "overnight result")

	w := htmlRequest(NewServer(store, "", nil).Handler(), http.MethodGet, "/read")
	body := w.Body.String()
	first := strings.Index(body, "Older item, newer note")
	second := strings.Index(body, "Newer item")
	if w.Code != http.StatusOK || first < 0 || second < 0 || first >= second {
		t.Fatalf("effective activity order incorrect: %d %s", w.Code, body)
	}
	if !strings.Contains(body[first:second], "note activity") {
		t.Fatalf("note-driven activity was not identified: %s", body[first:second])
	}
}

func TestReadPaginationAndDetailMarkdownSafety(t *testing.T) {
	store := newMemoryStore()
	for i := 0; i < readPageSize+1; i++ {
		id := "item_" + string(rune('a'+i))
		putReadFixture(t, store, id, "item", "Item "+id, "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	}
	putReadFixture(t, store, "note_markup", "note", "", "item_a", "2026-08-27T04:00:00Z", "2026-08-27T04:00:00Z", "# Report\n\n<script>alert(1)</script>\n\n- safe")

	h := NewServer(store, "secret", nil).Handler()
	page := htmlRequest(h, http.MethodGet, "/read?page=2")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "page 2 of 2") {
		t.Fatalf("second page: %d %s", page.Code, page.Body.String())
	}
	activityScript := htmlRequest(h, http.MethodGet, "/read/activity.js")
	if activityScript.Code != http.StatusOK || !strings.Contains(activityScript.Body.String(), "stuff.read.lastSeen") {
		t.Fatalf("last-seen helper: %d %s", activityScript.Code, activityScript.Body.String())
	}

	detail := htmlRequest(h, http.MethodGet, "/read/items/item_a")
	body := detail.Body.String()
	if detail.Code != http.StatusOK || !strings.Contains(body, "<h1>Report</h1>") {
		t.Fatalf("Markdown heading not rendered: %d %s", detail.Code, body)
	}
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("raw HTML was not rendered as inert text: %s", body)
	}
}

func TestReadAttachmentUsesSafeDownloadAndUnknownItemIs404(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_x", "item", "Artifacts", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	putReadFixture(t, store, "note_x", "note", "", "item_x", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "report")
	store.attachments["note_x/report.html"] = []byte("<script>never inline</script>")
	store.mu.Lock()
	store.docs["note_x"]["_attachments"] = map[string]any{"report.html": map[string]any{"content_type": "text/html", "length": 29}}
	store.docs["note_x"]["stuff_attachment_meta"] = map[string]any{"report.html": map[string]any{"bytes": 29, "media_type": "text/html"}}
	store.mu.Unlock()

	h := NewServer(store, "secret", nil).Handler()
	detail := htmlRequest(h, http.MethodGet, "/read/items/item_x")
	if !strings.Contains(detail.Body.String(), "/read/notes/note_x/attachments/report.html") {
		t.Fatalf("browser-safe attachment link missing: %s", detail.Body.String())
	}
	download := htmlRequest(h, http.MethodGet, "/read/notes/note_x/attachments/report.html")
	if download.Code != http.StatusOK || !strings.Contains(download.Header().Get("Content-Disposition"), "attachment") || download.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe attachment response: %d %#v", download.Code, download.Header())
	}
	body, _ := io.ReadAll(download.Result().Body)
	if string(body) != "<script>never inline</script>" {
		t.Fatalf("attachment body changed: %q", body)
	}
	view := htmlRequest(h, http.MethodGet, "/read/notes/note_x/attachments/report.html/view")
	viewCSP := view.Header().Get("Content-Security-Policy")
	if view.Code != http.StatusOK || !strings.Contains(view.Header().Get("Content-Disposition"), "inline") || !strings.Contains(viewCSP, "sandbox") || !strings.Contains(viewCSP, "default-src 'none'") {
		t.Fatalf("HTML view is not isolated: %d %#v", view.Code, view.Header())
	}

	missing := htmlRequest(h, http.MethodGet, "/read/items/item_missing")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing Item status: %d", missing.Code)
	}
}

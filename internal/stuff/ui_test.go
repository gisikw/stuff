package stuff

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func htmlRequest(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func readFormRequest(h http.Handler, method, path string, values url.Values, token string) *httptest.ResponseRecorder {
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if values != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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

func TestReadItemCreatesMultilineNoteWithServerTimestampsAndRedirect(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_target", "item", "Target", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	srv := NewServer(store, "secret", nil)
	srv.now = func() time.Time { return time.Date(2026, 8, 30, 12, 34, 56, 789, time.UTC) }
	h := srv.Handler()

	page := htmlRequest(h, http.MethodGet, "/read/items/item_target")
	for _, want := range []string{`<form class="note-form" method="post" action="/read/items/item_target/notes">`, `<label for="new-note-text">Add a Note</label>`, `textarea id="new-note-text" name="text"`, `form-action 'self'`} {
		if !strings.Contains(page.Body.String(), want) && !strings.Contains(page.Header().Get("Content-Security-Policy"), want) {
			t.Fatalf("accessible Note form missing %q: %s %#v", want, page.Body.String(), page.Header())
		}
	}

	text := "first line\nsecond café line\n<script>alert(1)</script>"
	created := readFormRequest(h, http.MethodPost, "/read/items/item_target/notes", url.Values{"text": {text}}, "secret")
	if created.Code != http.StatusSeeOther || created.Header().Get("Location") != "/read/items/item_target" || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("POST-redirect-GET response: %d %#v %s", created.Code, created.Header(), created.Body.String())
	}

	var note Document
	for _, doc := range store.docs {
		if doc["stuff_kind"] == "note" {
			note = doc
		}
	}
	stamp := "2026-08-30T12:34:56.000000789Z"
	if note == nil || note["item_id"] != "item_target" || note["text"] != text || note["created_at"] != stamp || note["updated_at"] != stamp {
		t.Fatalf("stored Note envelope: %#v", note)
	}
	if metadata, ok := note["metadata"].(map[string]any); !ok || len(metadata) != 0 {
		t.Fatalf("default Note metadata changed: %#v", note["metadata"])
	}

	redirected := htmlRequest(h, http.MethodGet, created.Header().Get("Location"))
	body := redirected.Body.String()
	if redirected.Code != http.StatusOK || !strings.Contains(body, "first line<br>second café line") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") || strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("new Note was not safely rendered after redirect: %d %s", redirected.Code, body)
	}
}

func TestReadItemNoteCreationBindsTargetAndRejectsInvalidBodies(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_a", "item", "A", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	putReadFixture(t, store, "item_b", "item", "B", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	h := NewServer(store, "", nil).Handler()

	cases := []struct {
		name   string
		values url.Values
		status int
		want   string
	}{
		{"empty", url.Values{"text": {" \n\t"}}, http.StatusBadRequest, "cannot be empty"},
		{"oversized", url.Values{"text": {strings.Repeat("x", maxReadNoteTextBytes+1)}}, http.StatusRequestEntityTooLarge, "byte limit"},
		{"substituted target", url.Values{"text": {"wrong target"}, "item_id": {"item_b"}}, http.StatusBadRequest, "exactly one text field"},
		{"duplicate body", url.Values{"text": {"one", "two"}}, http.StatusBadRequest, "exactly one text field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := readFormRequest(h, http.MethodPost, "/read/items/item_a/notes", tc.values, "")
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("invalid form response: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, doc := range store.docs {
		if doc["stuff_kind"] == "note" {
			t.Fatalf("invalid request created Note: %#v", doc)
		}
	}
}

func TestReadItemNoteCreationHonorsBrowserAuthMethodAndFormBoundaries(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_auth", "item", "Auth", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	h := NewServer(store, "secret", nil).Handler()
	path := "/read/items/item_auth/notes"

	// Like browser GETs, this narrowly scoped form is authorized by the
	// deployment's identity gate rather than exposing STUFF_TOKEN to HTML.
	browserPost := readFormRequest(h, http.MethodPost, path, url.Values{"text": {"identity-gated"}}, "")
	if browserPost.Code != http.StatusSeeOther {
		t.Fatalf("identity-gated browser mutation required API credentials: %d %s", browserPost.Code, browserPost.Body.String())
	}
	api := htmlRequest(h, http.MethodGet, "/v1/describe")
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("API auth boundary changed: %d %s", api.Code, api.Body.String())
	}
	wrongMethod := readFormRequest(h, http.MethodPut, path, url.Values{"text": {"no"}}, "secret")
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method boundary: %d %#v %s", wrongMethod.Code, wrongMethod.Header(), wrongMethod.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("text=no"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSite := httptest.NewRecorder()
	h.ServeHTTP(crossSite, request)
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("cross-site form accepted: %d %s", crossSite.Code, crossSite.Body.String())
	}
	wrongType := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"text":"no"}`))
	wrongType.Header.Set("Content-Type", "application/json")
	wrongType.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, wrongType)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-form submission accepted: %d %s", response.Code, response.Body.String())
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

func TestReadImageAttachmentUploadUsesBrowserAuthAndLimits(t *testing.T) {
	store := newMemoryStore()
	itemID := "item_upload"
	putReadFixture(t, store, itemID, "item", "Uploads", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	putReadFixture(t, store, "note_upload", "note", "", itemID, "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "image")
	h := NewServer(store, "secret", nil).Handler()
	path := "/read/notes/note_upload/attachments/photo.png"

	upload := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte("browser image")))
	upload.Header.Set("Content-Type", "image/png")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, upload)
	if w.Code != http.StatusOK {
		t.Fatalf("browser upload required bearer credentials: %d %s", w.Code, w.Body.String())
	}
	get := htmlRequest(h, http.MethodGet, path)
	if get.Code != http.StatusOK || get.Body.String() != "browser image" || get.Header().Get("Content-Type") != "image/png" || !strings.Contains(get.Header().Get("Content-Disposition"), "inline") || get.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("image response: %d %q %#v", get.Code, get.Body.String(), get.Header())
	}

	for _, tc := range []struct {
		name, contentType string
		body              io.Reader
	}{
		{"non-image", "text/plain", strings.NewReader("no")},
		{"oversize", "image/png", bytes.NewReader(make([]byte, MaxImageUploadBytes+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, tc.body)
			req.Header.Set("Content-Type", tc.contentType)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", out.Code, out.Body.String())
			}
		})
	}
	crossSite := httptest.NewRequest(http.MethodPost, path, strings.NewReader("cross-site"))
	crossSite.Header.Set("Content-Type", "image/png")
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, crossSite)
	if out.Code != http.StatusForbidden {
		t.Fatalf("cross-site upload accepted: %d %s", out.Code, out.Body.String())
	}
}

func TestReadGenericPolishIsMetadataAgnostic(t *testing.T) {
	store := newMemoryStore()
	itemID := "item_abcdefghijklmnopqrstuvwxyz"
	putReadFixture(t, store, itemID, "item", "Polished", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	putReadFixture(t, store, "note_abcdefghijklmnop", "note", "", itemID, "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "one")
	putReadFixture(t, store, "note_bcdefghijklmnopq", "note", "", itemID, "2026-08-27T03:00:00Z", "2026-08-27T03:00:00Z", "two")
	h := NewServer(store, "", nil).Handler()

	index := htmlRequest(h, http.MethodGet, "/read")
	body := index.Body.String()
	for _, want := range []string{">item_abcdefgh<", "2 Notes", `time data-time datetime="2026-08-27T03:00:00Z"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("activity polish missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, ">"+itemID+"<") {
		t.Fatalf("full Item ID remained visually expanded: %s", body)
	}

	detail := htmlRequest(h, http.MethodGet, "/read/items/"+itemID)
	body = detail.Body.String()
	for _, want := range []string{`data-copy-id="` + itemID + `"`, ">item_abcdefgh<", `id="notes-order"`, `id="notes-list" data-order="oldest"`, "Notes <span class=\"subtle\">(2)</span>", "/read/activity.js"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail polish missing %q: %s", want, body)
		}
	}

	script := htmlRequest(h, http.MethodGet, "/read/activity.js")
	for _, want := range []string{"Intl.RelativeTimeFormat", "stuff.read.notesNewestFirst", "navigator.clipboard.writeText"} {
		if !strings.Contains(script.Body.String(), want) {
			t.Fatalf("read helper missing %q: %s", want, script.Body.String())
		}
	}
	if got := shortID("short"); got != "short" {
		t.Fatalf("short ID changed: %q", got)
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

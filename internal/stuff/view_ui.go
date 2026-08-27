package stuff

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

var readViewHostTemplate = template.Must(template.New("view-host").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.ItemName}} · Stuff</title><style>
:root{color-scheme:light;--ink:#25231f;--muted:#716d64;--line:#dedbd3;--paper:#faf9f6;--accent:#385d54}*{box-sizing:border-box}body{margin:0;background:var(--paper);color:var(--ink);font:15px/1.45 ui-sans-serif,system-ui,-apple-system,sans-serif}.bar{display:flex;align-items:center;justify-content:space-between;gap:1rem;padding:.65rem 1rem;border-bottom:1px solid var(--line);background:#fff}.bar a{color:var(--accent)}.title{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.subtle{color:var(--muted);font-size:.85rem}iframe{display:block;width:100%;height:calc(100vh - 3.25rem);border:0;background:#fff}@media(max-width:520px){.bar{align-items:flex-start;flex-direction:column;gap:.25rem}iframe{height:calc(100vh - 4.75rem)}}
</style></head><body><header class="bar"><div class="title"><strong>{{.ItemName}}</strong> <span class="subtle">· {{.ViewName}}</span></div><a href="{{.PlainPath}}">Safe generic view</a></header><iframe id="stuff-view" title="{{.ViewName}}" sandbox="allow-scripts" data-frame="{{.FramePath}}" data-snapshot="{{.SnapshotPath}}" data-query="{{.QueryPath}}"></iframe><script src="/read/view-host.js" defer></script></body></html>`))

type readViewHostData struct {
	ItemName, ViewName, PlainPath, FramePath, SnapshotPath, QueryPath string
}

func (s *Server) maybeServeReadViewHost(w http.ResponseWriter, r *http.Request, item Document) (bool, string, error) {
	viewID, _ := item["view_id"].(string)
	if viewID == "" {
		return false, "", nil
	}
	view, err := s.store.Get(r.Context(), viewID)
	if err != nil {
		var storeErr *StoreError
		if errors.As(err, &storeErr) && storeErr.Status == http.StatusNotFound {
			return false, "The referenced View is unavailable; showing the safe generic Item view.", nil
		}
		return false, "", err
	}
	if view["stuff_kind"] != "view" {
		return false, "The referenced View is invalid; showing the safe generic Item view.", nil
	}
	id, _ := item["_id"].(string)
	if id == "" {
		id, _ = item["id"].(string)
	}
	escaped := url.PathEscape(id)
	data := readViewHostData{
		ItemName: stringValue(item["name"]), ViewName: stringValue(view["name"]),
		PlainPath:    "/read/items/" + escaped + "?plain=1",
		FramePath:    "/read/items/" + escaped + "/view",
		SnapshotPath: "/read/items/" + escaped + "/snapshot",
		QueryPath:    "/read/items/" + escaped + "/query/",
	}
	renderReadViewHost(w, data)
	return true, "", nil
}

func renderReadViewHost(w http.ResponseWriter, data readViewHostData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := readViewHostTemplate.Execute(w, data); err != nil {
		http.Error(w, "Unable to render View host", http.StatusInternalServerError)
	}
}

func serveReadViewHostScript(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(`(() => {
  const frame = document.getElementById("stuff-view");
  if (!frame) return;
  const snapshot = fetch(frame.dataset.snapshot, {credentials: "same-origin", headers: {Accept: "application/json"}})
    .then((response) => {
      if (!response.ok) throw new Error("snapshot unavailable");
      return response.json();
    });
  const post = (message) => frame.contentWindow.postMessage(message, "*");
  frame.addEventListener("load", () => {
    snapshot.then(post).catch(() => {});
  });
  let active = 0;
  window.addEventListener("message", async (event) => {
    if (event.source !== frame.contentWindow) return;
    const message = event.data;
    if (!message || message.type !== "stuff:view-query" || typeof message.request_id !== "string" || message.request_id.length > 128) return;
    if ((message.resource !== "items" && message.resource !== "notes") || !message.query || typeof message.query !== "object" || Array.isArray(message.query)) return;
    if (active >= 4) {
      post({type: "stuff:view-query-result", request_id: message.request_id, ok: false, error: "too many concurrent queries"});
      return;
    }
    let encoded;
    try {
      encoded = JSON.stringify(message.query);
    } catch (_) {
      return;
    }
    if (encoded.length > 65536) {
      post({type: "stuff:view-query-result", request_id: message.request_id, ok: false, error: "query exceeds 65536 bytes"});
      return;
    }
    active++;
    try {
      const response = await fetch(frame.dataset.query + message.resource, {
        method: "POST", credentials: "same-origin",
        headers: {Accept: "application/json", "Content-Type": "application/json"}, body: encoded
      });
      const result = await response.json();
      post({type: "stuff:view-query-result", request_id: message.request_id, ok: response.ok, result});
    } catch (_) {
      post({type: "stuff:view-query-result", request_id: message.request_id, ok: false, error: "query unavailable"});
    } finally {
      active--;
    }
  });
  frame.src = frame.dataset.frame;
})();
`))
}

func (s *Server) serveReadViewQueryRoute(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/read/items/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 3 || parts[1] != "query" || (parts[2] != "items" && parts[2] != "notes") {
		return false
	}
	itemID, err := url.PathUnescape(parts[0])
	if err != nil || itemID == "" || strings.Contains(itemID, "/") {
		return false
	}
	_, view, err := s.resolveReadView(r.Context(), itemID)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		s.handleError(w, err)
		return true
	}
	if !viewHasCapability(view, "find_"+parts[2]) {
		w.Header().Set("Cache-Control", "no-store")
		s.handleError(w, &apiError{status: http.StatusForbidden, path: "capability", reason: "View is not allowed to query " + parts[2], expected: "grant the explicit find_" + parts[2] + " View capability"})
		return true
	}
	kind := strings.TrimSuffix(parts[2], "s")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.find(w, r, kind); err != nil {
		s.handleError(w, err)
	}
	return true
}

func (s *Server) resolveReadView(ctx context.Context, itemID string) (Document, Document, error) {
	item, err := s.store.Get(ctx, itemID)
	if err != nil {
		return nil, nil, err
	}
	if item["stuff_kind"] != "item" {
		return nil, nil, &StoreError{Status: http.StatusNotFound, Reason: "Item not found"}
	}
	viewID, _ := item["view_id"].(string)
	if viewID == "" {
		return nil, nil, &StoreError{Status: http.StatusNotFound, Reason: "View not found"}
	}
	view, err := s.store.Get(ctx, viewID)
	if err != nil {
		return nil, nil, err
	}
	if view["stuff_kind"] != "view" {
		return nil, nil, &StoreError{Status: http.StatusNotFound, Reason: "View not found"}
	}
	return item, view, nil
}

func (s *Server) serveReadViewDocument(w http.ResponseWriter, r *http.Request, itemID string) {
	_, view, err := s.resolveReadView(r.Context(), itemID)
	if err != nil {
		s.readError(w, err)
		return
	}
	renderer, _ := view["renderer"].(string)
	if renderer == "" {
		readHTTPError(w, http.StatusNotFound, "View not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; font-src data:; connect-src 'none'; media-src 'none'; object-src 'none'; frame-src 'none'; child-src 'none'; worker-src 'none'; manifest-src 'none'; base-uri 'none'; form-action 'none'; navigate-to 'none'; frame-ancestors 'self'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(renderer))
}

func (s *Server) serveReadViewSnapshot(w http.ResponseWriter, r *http.Request, itemID string) {
	item, _, err := s.resolveReadView(r.Context(), itemID)
	if err != nil {
		s.readError(w, err)
		return
	}
	notes, full, err := s.readDocuments(r, "note", Document{"selector": Document{"item_id": itemID}, "limit": MaxPageSize})
	if err != nil {
		s.readError(w, err)
		return
	}
	publicItem := publicDocument(item)
	itemJSON, err := json.Marshal(publicItem)
	if err != nil {
		s.readError(w, err)
		return
	}
	// Keep the snapshot inside Stuff's response bound. The small fixed reserve
	// covers the protocol envelope and JSON punctuation.
	budget := MaxResponseBytes - len(itemJSON) - 1024
	linked := make([]Document, 0, len(notes))
	used := 0
	for _, note := range notes {
		if stringValue(note["item_id"]) != itemID {
			continue
		}
		noteJSON, marshalErr := json.Marshal(note)
		if marshalErr != nil {
			s.readError(w, marshalErr)
			return
		}
		if used+len(noteJSON)+1 > budget {
			full = true
			break
		}
		used += len(noteJSON) + 1
		linked = append(linked, note)
	}
	message := Document{
		"type": "stuff:view-snapshot", "version": 1,
		"item": publicItem, "notes": linked, "truncated": full,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := json.NewEncoder(w).Encode(message); err != nil {
		s.log.Error("rendering View snapshot", "item", itemID, "error", err)
	}
}

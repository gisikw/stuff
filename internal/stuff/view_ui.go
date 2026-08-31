package stuff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var readViewHostTemplate = template.Must(template.New("view-host").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.ItemName}} · Stuff</title><style>
:root{color-scheme:light;--ink:#25231f;--muted:#716d64;--line:#dedbd3;--paper:#faf9f6;--accent:#385d54}*{box-sizing:border-box}body{margin:0;background:var(--paper);color:var(--ink);font:15px/1.45 ui-sans-serif,system-ui,-apple-system,sans-serif}.bar{display:flex;align-items:center;justify-content:space-between;gap:1rem;padding:.65rem 1rem;border-bottom:1px solid var(--line);background:#fff}.bar a{color:var(--accent)}.title{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.subtle{color:var(--muted);font-size:.85rem}iframe{display:block;width:100%;height:calc(100vh - 3.25rem);border:0;background:#fff}@media(max-width:520px){.bar{align-items:flex-start;flex-direction:column;gap:.25rem}iframe{height:calc(100vh - 4.75rem)}}
</style></head><body><header class="bar"><div class="title"><strong>{{.ItemName}}</strong> <span class="subtle">· {{.ViewName}}</span></div><a href="{{.PlainPath}}">Safe generic view</a></header><iframe id="stuff-view" title="{{.ViewName}}" sandbox="allow-scripts allow-top-navigation-by-user-activation" data-frame="{{.FramePath}}" data-snapshot="{{.SnapshotPath}}" data-query="{{.QueryPath}}" data-operation="{{.OperationPath}}"></iframe><script src="/read/view-host.js" defer></script></body></html>`))

type readViewHostData struct {
	ItemName, ViewName, PlainPath, FramePath, SnapshotPath, QueryPath, OperationPath string
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
		PlainPath:     "/read/items/" + escaped + "?plain=1",
		FramePath:     "/read/items/" + escaped + "/view",
		SnapshotPath:  "/read/items/" + escaped + "/snapshot",
		QueryPath:     "/read/items/" + escaped + "/query/",
		OperationPath: "/read/items/" + escaped + "/operation",
	}
	renderReadViewHost(w, data)
	return true, "", nil
}

func renderReadViewHost(w http.ResponseWriter, data readViewHostData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self'; frame-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
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
    if (!message || typeof message.request_id !== "string" || message.request_id.length > 128) return;
    if (message.type === "stuff:view-operation") {
      if (typeof message.operation !== "string" || typeof message.args !== "string" || message.args.length > 1048576) return;
      let args;
      try { args = JSON.parse(message.args); } catch (_) {
        post({type: "stuff:view-operation-result", request_id: message.request_id, ok: false, status: 400, error: "operation args are not valid JSON"});
        return;
      }
      if (!args || typeof args !== "object" || Array.isArray(args)) {
        post({type: "stuff:view-operation-result", request_id: message.request_id, ok: false, status: 400, error: "operation args must be a JSON object"});
        return;
      }
      try {
        const response = await fetch(frame.dataset.operation, {
          method: "POST", credentials: "same-origin",
          headers: {Accept: "application/json", "Content-Type": "application/json"},
          body: JSON.stringify({operation: message.operation, args})
        });
        const result = await response.json();
        post({type: "stuff:view-operation-result", request_id: message.request_id, ok: response.ok, status: response.status, result});
      } catch (_) {
        post({type: "stuff:view-operation-result", request_id: message.request_id, ok: false, status: 503, error: "operation unavailable"});
      }
      return;
    }
    if (message.type !== "stuff:view-query") return;
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

const readViewOperationBootstrap = `<script>(()=>{
  let sequence=0;
  const pending=new Map();
  window.stuff=Object.freeze({invoke(operation,args={}){
    return new Promise((resolve,reject)=>{
      if(typeof operation!=="string"||!operation){reject(new TypeError("operation name is required"));return;}
      let serialized;
      try{serialized=JSON.stringify(args);}catch(error){reject(error);return;}
      const request_id="operation-"+(++sequence)+"-"+Date.now();
      pending.set(request_id,{resolve,reject});
      parent.postMessage({type:"stuff:view-operation",operation,request_id,args:serialized},"*");
    });
  }});
  addEventListener("message",event=>{
    const message=event.data;
    if(event.source!==parent||!message||message.type!=="stuff:view-operation-result")return;
    const request=pending.get(message.request_id);if(!request)return;
    pending.delete(message.request_id);
    if(message.ok){request.resolve(message.result);return;}
    const reason=message.result?.error?.reason||message.error||"operation failed";
    const error=new Error(reason);error.status=message.status;error.result=message.result;request.reject(error);
  });
})();</script>`

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
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-top-navigation-by-user-activation; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; font-src data:; connect-src 'none'; media-src 'none'; object-src 'none'; frame-src 'none'; child-src 'none'; worker-src 'none'; manifest-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(readViewOperationBootstrap + renderer))
}

var operationArgPattern = regexp.MustCompile(`\$args\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*`)

func operationArg(args map[string]any, reference string) (any, bool) {
	var value any = args
	for _, key := range strings.Split(strings.TrimPrefix(reference, "$args."), ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok = object[key]
		if !ok {
			return nil, false
		}
	}
	return value, true
}

func substituteOperationBody(value any, args map[string]any) (any, error) {
	switch value := value.(type) {
	case string:
		if operationArgPattern.MatchString(value) && operationArgPattern.FindString(value) == value {
			replacement, ok := operationArg(args, value)
			if !ok {
				return nil, bad("args", "missing operation argument "+value, "provide every referenced operation argument")
			}
			return replacement, nil
		}
		return value, nil
	case []any:
		out := make([]any, len(value))
		for i, entry := range value {
			replacement, err := substituteOperationBody(entry, args)
			if err != nil {
				return nil, err
			}
			out[i] = replacement
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, entry := range value {
			replacement, err := substituteOperationBody(entry, args)
			if err != nil {
				return nil, err
			}
			out[key] = replacement
		}
		return out, nil
	default:
		return value, nil
	}
}

func substituteOperationPath(path string, args map[string]any) (string, error) {
	var substitutionErr error
	path = operationArgPattern.ReplaceAllStringFunc(path, func(reference string) string {
		value, ok := operationArg(args, reference)
		if !ok {
			substitutionErr = bad("args", "missing operation argument "+reference, "provide every referenced operation argument")
			return reference
		}
		var scalar string
		switch value := value.(type) {
		case nil:
			scalar = "null"
		case string:
			scalar = value
		case bool, json.Number, float64:
			scalar = fmt.Sprint(value)
		default:
			substitutionErr = bad("args", "operation path argument "+reference+" is not a scalar", "a string, number, boolean, or null")
			return reference
		}
		return url.PathEscape(scalar)
	})
	if substitutionErr != nil {
		return "", substitutionErr
	}
	return path, nil
}

type operationResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *operationResponseWriter) Header() http.Header { return w.header }
func (w *operationResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *operationResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (s *Server) serveReadViewOperationRoute(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/read/items/"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, "/operation") {
		return false
	}
	rawID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/operation")
	itemID, err := url.PathUnescape(rawID)
	if err != nil || itemID == "" || strings.Contains(itemID, "/") {
		return false
	}
	_, view, err := s.resolveReadView(r.Context(), itemID)
	if err != nil {
		s.handleError(w, err)
		return true
	}
	var invocation struct {
		Operation string         `json:"operation"`
		Args      map[string]any `json:"args"`
	}
	if err := decodeJSON(r, MaxJSONBytes, &invocation); err != nil {
		s.handleError(w, err)
		return true
	}
	operations, _ := view["operations"].(map[string]any)
	rawOperation, ok := operations[invocation.Operation]
	if !ok {
		s.handleError(w, &apiError{status: http.StatusNotFound, path: "operation", reason: "unknown View operation", expected: "a named operation stored on the Item's current View"})
		return true
	}
	operation, _ := rawOperation.(map[string]any)
	requestTemplate, _ := operation["request"].(map[string]any)
	method, _ := requestTemplate["method"].(string)
	path, _ := requestTemplate["path"].(string)
	if method == "" || path == "" {
		s.handleError(w, bad("operation", "stored operation request is incomplete", "request.method and request.path"))
		return true
	}
	path, err = substituteOperationPath(path, invocation.Args)
	if err != nil {
		s.handleError(w, err)
		return true
	}
	if !strings.HasPrefix(path, "/v1/") {
		s.handleError(w, bad("operation.request.path", "operation must target the /v1/ API surface", "an absolute /v1/... path"))
		return true
	}
	var body []byte
	if template, present := requestTemplate["body"]; present {
		substituted, err := substituteOperationBody(template, invocation.Args)
		if err != nil {
			s.handleError(w, err)
			return true
		}
		body, err = json.Marshal(substituted)
		if err != nil {
			s.handleError(w, err)
			return true
		}
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.IsAbs() {
		s.handleError(w, bad("operation.request.path", "stored operation path is invalid", "an absolute /v1/... path"))
		return true
	}
	internal, err := http.NewRequestWithContext(r.Context(), method, "http://stuff.internal"+path, bytes.NewReader(body))
	if err != nil {
		s.handleError(w, bad("operation.request", "stored operation request is invalid", "an HTTP method and /v1/... path"))
		return true
	}
	// Keep escaped argument bytes opaque to the API router. In particular, an
	// escaped slash must not become a second route segment.
	internal.URL.Path = parsed.EscapedPath()
	internal.URL.RawPath = ""
	if body != nil {
		internal.Header.Set("Content-Type", "application/json")
	}
	if s.token != "" {
		internal.Header.Set("Authorization", "Bearer "+s.token)
	}
	captured := &operationResponseWriter{header: make(http.Header)}
	s.serveHTTP(captured, internal)
	for _, name := range []string{"Content-Type"} {
		if value := captured.header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self'")
	status := captured.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(captured.body.Bytes())
	return true
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

package stuff

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	MaxJSONBytes       = 1 << 20
	MaxQueryBytes      = 64 << 10
	MaxAttachmentBytes = 16 << 20
	MaxResponseBytes   = 16 << 20
	MaxPageSize        = 200
)

type Server struct {
	store Store
	token string
	log   *slog.Logger
	now   func() time.Time
}

func NewServer(store Store, token string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: store, token: token, log: logger, now: time.Now}
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, Document{"status": "ok"})
		return
	}
	if r.Method == http.MethodGet && s.serveReadRoute(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		writeError(w, http.StatusUnauthorized, "authorization", "missing or invalid bearer token", "set STUFF_TOKEN to the service token")
		return
	}
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1"), "/")
	parts := strings.Split(p, "/")
	if p == "" {
		writeJSON(w, 200, Document{"name": "Stuff", "version": "v1"})
		return
	}

	var err error
	switch {
	case p == "items" && r.Method == http.MethodPost:
		err = s.createItem(w, r)
	case p == "items/_find" && r.Method == http.MethodPost:
		err = s.find(w, r, "item")
	case len(parts) == 2 && parts[0] == "items" && r.Method == http.MethodGet:
		err = s.get(w, r, parts[1], "item")
	case len(parts) == 2 && parts[0] == "items" && r.Method == http.MethodPatch:
		err = s.updateItem(w, r, parts[1])
	case p == "notes" && r.Method == http.MethodPost:
		err = s.createNote(w, r)
	case p == "notes/_find" && r.Method == http.MethodPost:
		err = s.find(w, r, "note")
	case len(parts) == 2 && parts[0] == "notes" && r.Method == http.MethodGet:
		err = s.get(w, r, parts[1], "note")
	case len(parts) == 4 && parts[0] == "notes" && parts[2] == "attachments" && r.Method == http.MethodGet:
		err = s.attachment(w, r, parts[1], parts[3])
	case p == "schemas" && r.Method == http.MethodGet:
		err = s.listSchemas(w, r)
	case p == "schemas" && r.Method == http.MethodPost:
		err = s.putSchema(w, r)
	case len(parts) == 2 && parts[0] == "schemas" && r.Method == http.MethodGet:
		err = s.getSchema(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "schemas" && parts[2] == "check" && r.Method == http.MethodPost:
		err = s.checkSchema(w, r, parts[1])
	case p == "describe" && r.Method == http.MethodGet:
		err = s.describe(w, r)
	case p == "explain/items" && r.Method == http.MethodPost:
		err = s.explain(w, r, "item")
	case p == "explain/notes" && r.Method == http.MethodPost:
		err = s.explain(w, r, "note")
	default:
		writeError(w, http.StatusNotFound, "path", "unknown endpoint", "see `stuff --help`")
		return
	}
	if err != nil {
		s.handleError(w, err)
	}
}

type apiError struct {
	status                 int
	path, reason, expected string
}

func (e *apiError) Error() string             { return e.reason }
func bad(path, reason, expected string) error { return &apiError{400, path, reason, expected} }

func (s *Server) handleError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.path, ae.reason, ae.expected)
		return
	}
	var se *StoreError
	if errors.As(err, &se) {
		status := se.Status
		if status == http.StatusConflict {
			writeError(w, status, "revision", "revision conflict: "+se.Reason, "fetch the record and retry with its current revision")
			return
		}
		if status == http.StatusNotFound {
			writeError(w, status, "id", se.Reason, "check the Item, Note, or Schema identifier")
			return
		}
		if status < 400 || status > 599 {
			status = 502
		}
		writeError(w, status, "couchdb", se.Reason, "repair the request using CouchDB Mango semantics")
		return
	}
	s.log.Error("request failed", "error", err)
	writeError(w, 500, "server", err.Error(), "inspect the Stuff service logs")
}

func decodeJSON(r *http.Request, max int64, out any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, max+1))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return bad("$", "invalid JSON: "+err.Error(), "a single JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return bad("$", "request exceeds its limit or contains trailing JSON", fmt.Sprintf("at most %d bytes", max))
	}
	return nil
}

type optionalJSON struct {
	Present bool
	Value   any
}

func (o *optionalJSON) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&o.Value); err != nil {
		return err
	}
	o.Present = true
	return nil
}

func metadata(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, bad("metadata", "metadata is not valid JSON: "+err.Error(), "any JSON value")
	}
	if len(b) > MaxJSONBytes {
		return nil, bad("metadata", "metadata exceeds the payload limit", fmt.Sprintf("at most %d encoded bytes", MaxJSONBytes))
	}
	return v, nil
}

func (s *Server) createItem(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name     string       `json:"name"`
		Metadata optionalJSON `json:"metadata"`
		Validate string       `json:"validate"`
	}
	if err := decodeJSON(r, MaxJSONBytes, &in); err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return bad("name", "name is required", "a non-empty Item name")
	}
	metaValue := in.Metadata.Value
	if !in.Metadata.Present {
		metaValue = map[string]any{}
	}
	meta, err := metadata(metaValue)
	if err != nil {
		return err
	}
	if in.Validate != "" {
		if err := s.validate(r.Context(), in.Validate, meta); err != nil {
			return err
		}
	}
	id, err := newID("item_")
	if err != nil {
		return err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	doc := Document{"stuff_kind": "item", "name": in.Name, "created_at": now, "updated_at": now, "metadata": meta}
	rev, err := s.store.Create(r.Context(), id, doc)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, Document{"id": id, "revision": rev})
	return nil
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, id, kind string) error {
	doc, err := s.store.Get(r.Context(), id)
	if err != nil {
		return err
	}
	if doc["stuff_kind"] != kind {
		return &StoreError{Status: 404, Reason: "not found"}
	}
	writeJSON(w, 200, publicDocument(doc))
	return nil
}

func (s *Server) updateItem(w http.ResponseWriter, r *http.Request, id string) error {
	var in struct {
		Name     *string      `json:"name"`
		Metadata optionalJSON `json:"metadata"`
		Revision string       `json:"revision"`
		Validate string       `json:"validate"`
	}
	if err := decodeJSON(r, MaxJSONBytes, &in); err != nil {
		return err
	}
	doc, err := s.store.Get(r.Context(), id)
	if err != nil {
		return err
	}
	if doc["stuff_kind"] != "item" {
		return &StoreError{Status: 404, Reason: "Item not found"}
	}
	if in.Name == nil && !in.Metadata.Present {
		return bad("$", "update changes nothing", "provide name and/or metadata")
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			return bad("name", "name cannot be empty", "a non-empty Item name")
		}
		doc["name"] = n
	}
	if in.Metadata.Present {
		meta, e := metadata(in.Metadata.Value)
		if e != nil {
			return e
		}
		doc["metadata"] = meta
	}
	meta := doc["metadata"]
	if in.Validate != "" {
		if err := s.validate(r.Context(), in.Validate, meta); err != nil {
			return err
		}
	}
	rev, _ := doc["_rev"].(string)
	if in.Revision != "" {
		rev = in.Revision
	}
	doc["updated_at"] = s.now().UTC().Format(time.RFC3339Nano)
	newRev, err := s.store.Put(r.Context(), id, rev, doc)
	if err != nil {
		return err
	}
	doc["_rev"] = newRev
	writeJSON(w, 200, publicDocument(doc))
	return nil
}

type attachmentInput struct{ Name, MediaType, Data string }

func (a *attachmentInput) UnmarshalJSON(b []byte) error {
	var x struct {
		Name      string `json:"name"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	a.Name, a.MediaType, a.Data = x.Name, x.MediaType, x.Data
	return nil
}

func (s *Server) createNote(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ItemID      string            `json:"item_id"`
		Text        *string           `json:"text"`
		Metadata    optionalJSON      `json:"metadata"`
		Attachments []attachmentInput `json:"attachments"`
	}
	if err := decodeJSON(r, MaxAttachmentBytes*2+MaxJSONBytes, &in); err != nil {
		return err
	}
	if in.ItemID == "" {
		return bad("item_id", "item_id is required", "an existing Item ID")
	}
	item, err := s.store.Get(r.Context(), in.ItemID)
	if err != nil {
		return err
	}
	if item["stuff_kind"] != "item" {
		return bad("item_id", "Item does not exist", "an existing Item ID from `stuff add` or `stuff find`")
	}
	metaValue := in.Metadata.Value
	if !in.Metadata.Present {
		metaValue = map[string]any{}
	}
	meta, err := metadata(metaValue)
	if err != nil {
		return err
	}
	id, err := newID("note_")
	if err != nil {
		return err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	doc := Document{"stuff_kind": "note", "item_id": in.ItemID, "created_at": now, "updated_at": now, "metadata": meta}
	if in.Text != nil {
		doc["text"] = *in.Text
	}
	if len(in.Attachments) > 0 {
		couch := map[string]any{}
		metas := map[string]any{}
		total := 0
		seen := map[string]bool{}
		for i, a := range in.Attachments {
			if a.Name == "" || a.Name == "." || a.Name == ".." || strings.ContainsAny(a.Name, "/\\") {
				return bad(fmt.Sprintf("attachments[%d].name", i), "attachment name is invalid", "a base filename without path separators")
			}
			if seen[a.Name] {
				return bad(fmt.Sprintf("attachments[%d].name", i), "attachment names must be unique", "a distinct base filename")
			}
			seen[a.Name] = true
			data, e := base64.StdEncoding.DecodeString(a.Data)
			if e != nil {
				return bad(fmt.Sprintf("attachments[%d].data", i), "attachment data is not base64", "standard padded base64")
			}
			total += len(data)
			if total > MaxAttachmentBytes {
				return bad("attachments", "attachments exceed total size limit", fmt.Sprintf("at most %d decoded bytes", MaxAttachmentBytes))
			}
			mediaType := a.MediaType
			if mediaType == "" {
				mediaType = "application/octet-stream"
			}
			sum := sha256.Sum256(data)
			couch[a.Name] = map[string]any{"content_type": mediaType, "data": a.Data}
			metas[a.Name] = map[string]any{"sha256": hex.EncodeToString(sum[:]), "bytes": len(data), "media_type": mediaType, "url": "/v1/notes/" + url.PathEscape(id) + "/attachments/" + url.PathEscape(a.Name)}
		}
		doc["_attachments"] = couch
		doc["stuff_attachment_meta"] = metas
	}
	rev, err := s.store.Create(r.Context(), id, doc)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, Document{"id": id, "revision": rev})
	return nil
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request, id, name string) error {
	doc, err := s.store.Get(r.Context(), id)
	if err != nil {
		return err
	}
	if doc["stuff_kind"] != "note" {
		return &StoreError{Status: 404, Reason: "Note not found"}
	}
	body, headers, err := s.store.Attachment(r.Context(), id, name)
	if err != nil {
		return err
	}
	defer body.Close()
	w.Header().Set("Content-Type", headers.Get("Content-Type"))
	w.Header().Set("Content-Length", headers.Get("Content-Length"))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.WriteHeader(200)
	_, err = io.Copy(w, body)
	return err
}

func (s *Server) find(w http.ResponseWriter, r *http.Request, kind string) error {
	var q Document
	if err := decodeJSON(r, MaxQueryBytes, &q); err != nil {
		return err
	}
	selector, ok := q["selector"]
	if !ok {
		return bad("selector", "selector is required", `{"selector":{},"limit":50}`)
	}
	if _, ok := selector.(map[string]any); !ok {
		return bad("selector", "selector must be a JSON object", `{"selector":{}}`)
	}
	if n, ok := numberInt(q["limit"]); ok && n > MaxPageSize {
		return bad("limit", "page limit exceeds service maximum", fmt.Sprintf("an integer from 1 to %d", MaxPageSize))
	}
	if _, ok := q["limit"]; !ok {
		q["limit"] = 50
	}
	out, err := s.store.Find(r.Context(), kind, q)
	if err != nil {
		return err
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) explain(w http.ResponseWriter, r *http.Request, kind string) error {
	var q Document
	if err := decodeJSON(r, MaxQueryBytes, &q); err != nil {
		return err
	}
	selector, ok := q["selector"]
	if !ok {
		return bad("selector", "selector is required", `{"selector":{}}`)
	}
	if _, ok := selector.(map[string]any); !ok {
		return bad("selector", "selector must be a JSON object", `{"selector":{}}`)
	}
	out, err := s.store.Explain(r.Context(), kind, q)
	if err != nil {
		return err
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) putSchema(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name   string `json:"name"`
		Schema any    `json:"schema"`
	}
	if err := decodeJSON(r, MaxJSONBytes, &in); err != nil {
		return err
	}
	if !validSchemaName(in.Name) {
		return bad("name", "Schema name is invalid", "1-128 letters, digits, dots, underscores, or hyphens; begin with a letter or digit")
	}
	if _, ok := in.Schema.(map[string]any); !ok {
		return bad("schema", "Schema must be a JSON object", "a standard JSON Schema document")
	}
	if _, err := compileSchema(in.Name, in.Schema); err != nil {
		return bad("schema", "invalid JSON Schema: "+err.Error(), "a valid Draft 2020-12 JSON Schema")
	}
	id := "schema:" + in.Name
	old, err := s.store.Get(r.Context(), id)
	rev := ""
	created := s.now().UTC().Format(time.RFC3339Nano)
	if err == nil {
		rev, _ = old["_rev"].(string)
		if x, ok := old["created_at"].(string); ok {
			created = x
		}
	} else {
		var se *StoreError
		if !errors.As(err, &se) || se.Status != 404 {
			return err
		}
	}
	doc := Document{"stuff_kind": "schema", "name": in.Name, "schema": in.Schema, "created_at": created, "updated_at": s.now().UTC().Format(time.RFC3339Nano)}
	var newRev string
	if rev == "" {
		newRev, err = s.store.Create(r.Context(), id, doc)
	} else {
		newRev, err = s.store.Put(r.Context(), id, rev, doc)
	}
	if err != nil {
		return err
	}
	writeJSON(w, 200, Document{"name": in.Name, "revision": newRev})
	return nil
}

func (s *Server) getSchema(w http.ResponseWriter, r *http.Request, name string) error {
	doc, err := s.store.Get(r.Context(), "schema:"+name)
	if err != nil {
		return err
	}
	if doc["stuff_kind"] != "schema" {
		return &StoreError{Status: 404, Reason: "Schema not found"}
	}
	writeJSON(w, 200, publicDocument(doc))
	return nil
}

func (s *Server) listSchemas(w http.ResponseWriter, r *http.Request) error {
	out, err := s.store.Find(r.Context(), "schema", Document{"selector": Document{}, "fields": []any{"name", "created_at", "updated_at"}, "limit": MaxPageSize})
	if err != nil {
		return err
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) checkSchema(w http.ResponseWriter, r *http.Request, name string) error {
	var in struct {
		ItemID   string       `json:"item_id"`
		Metadata optionalJSON `json:"metadata"`
	}
	if err := decodeJSON(r, MaxJSONBytes, &in); err != nil {
		return err
	}
	var meta any
	if in.Metadata.Present {
		meta = in.Metadata.Value
	}
	if in.ItemID != "" {
		doc, err := s.store.Get(r.Context(), in.ItemID)
		if err != nil {
			return err
		}
		if doc["stuff_kind"] != "item" {
			return bad("item_id", "Item does not exist", "an existing Item ID")
		}
		meta = doc["metadata"]
	}
	if in.ItemID == "" && !in.Metadata.Present {
		return bad("$", "provide item_id or metadata", `{"item_id":"item_..."} or {"metadata":{...}}`)
	}
	m, err := metadata(meta)
	if err != nil {
		return err
	}
	if err := s.validate(r.Context(), name, m); err != nil {
		return err
	}
	writeJSON(w, 200, Document{"valid": true, "schema": name})
	return nil
}

type offlineSchemaLoader struct{}

func (offlineSchemaLoader) Load(location string) (any, error) {
	return nil, fmt.Errorf("external schema resource %q is unavailable; Stuff validation is offline", location)
}

func compileSchema(name string, raw any) (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(offlineSchemaLoader{})
	if err := compiler.AddResource("urn:stuff:schema:"+name, raw); err != nil {
		return nil, err
	}
	return compiler.Compile("urn:stuff:schema:" + name)
}

func (s *Server) validate(ctx context.Context, name string, meta any) error {
	doc, err := s.store.Get(ctx, "schema:"+name)
	if err != nil {
		return bad("validate", fmt.Sprintf("Schema %q does not exist", name), "a name from `stuff schemas`")
	}
	sch, err := compileSchema(name, doc["schema"])
	if err != nil {
		return fmt.Errorf("stored Schema %q is invalid: %w", name, err)
	}
	if err := sch.Validate(meta); err != nil {
		var ve *jsonschema.ValidationError
		path := "metadata"
		if errors.As(err, &ve) && len(ve.InstanceLocation) > 0 {
			path += "." + strings.Join(ve.InstanceLocation, ".")
		}
		return bad(path, fmt.Sprintf("metadata does not satisfy Schema %q: %s", name, err.Error()), "metadata matching `stuff schema get "+name+"`")
	}
	return nil
}

func (s *Server) describe(w http.ResponseWriter, r *http.Request) error {
	items, err := s.store.Find(r.Context(), "item", Document{"selector": Document{}, "limit": MaxPageSize})
	if err != nil {
		return err
	}
	notes, err := s.store.Find(r.Context(), "note", Document{"selector": Document{}, "limit": MaxPageSize})
	if err != nil {
		return err
	}
	indexes, err := s.store.Indexes(r.Context())
	if err != nil {
		return err
	}
	observed := map[string]*observation{}
	observeDocs(observed, "item", items)
	observeDocs(observed, "note", notes)
	fields := make([]any, 0, len(observed))
	keys := make([]string, 0, len(observed))
	for k := range observed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fields = append(fields, observed[k].public(k))
	}
	out := Document{
		"items": lenDocs(items), "notes": lenDocs(notes), "sample_limit": MaxPageSize,
		"envelopes":       Document{"item": []any{"id", "name", "created_at", "updated_at", "revision", "metadata"}, "note": []any{"id", "item_id", "created_at", "updated_at", "revision", "text", "metadata", "attachments"}},
		"mango":           Document{"version": "CouchDB 3.x Mango", "operators": []any{"$eq", "$ne", "$gt", "$gte", "$lt", "$lte", "$exists", "$type", "$in", "$nin", "$all", "$size", "$elemMatch", "$allMatch", "$and", "$or", "$not", "$nor", "$beginsWith", "$regex", "$mod", "$keyMapMatch", "$text"}},
		"limits":          Document{"limit_max": MaxPageSize, "selector_bytes_max": MaxQueryBytes, "metadata_bytes_max": MaxJSONBytes, "attachment_bytes_max": MaxAttachmentBytes, "response_bytes_max": MaxResponseBytes},
		"observed_fields": fields, "indexes": indexes,
		"text_search": hasIndexType(indexes, "text"),
		"examples":    Document{"find_items": `stuff find <<'JSON'\n{"selector":{"metadata.area":"familiar"},"limit":20}\nJSON`, "find_notes": `stuff note find <<'JSON'\n{"selector":{"metadata.kind":"decision"},"limit":20}\nJSON`},
		"scope":       "Stores inert Items, Notes, attachments, and advisory Schemas. It does not dispatch, schedule, retry, lock, reconcile, or enforce workflow.",
	}
	writeJSON(w, 200, out)
	return nil
}

type observation struct {
	types    map[string]bool
	present  int
	examples []any
}

func (o *observation) public(path string) Document {
	ts := make([]string, 0, len(o.types))
	for t := range o.types {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	return Document{"path": path, "types": ts, "present": o.present, "examples": o.examples}
}
func observeDocs(out map[string]*observation, kind string, result Document) {
	docs, _ := result["docs"].([]any)
	for _, raw := range docs {
		d, _ := raw.(map[string]any)
		if metadata, ok := d["metadata"]; ok {
			observeValue(out, kind+".metadata", metadata)
		}
	}
}
func observeValue(out map[string]*observation, prefix string, value any) {
	m, ok := value.(map[string]any)
	if ok {
		for k, v := range m {
			observeValue(out, prefix+"."+k, v)
		}
		return
	}
	o := out[prefix]
	if o == nil {
		o = &observation{types: map[string]bool{}}
		out[prefix] = o
	}
	o.present++
	o.types[jsonType(value)] = true
	if len(o.examples) < 3 && !containsJSON(o.examples, value) {
		o.examples = append(o.examples, value)
	}
}
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}
func containsJSON(xs []any, v any) bool {
	b, _ := json.Marshal(v)
	for _, x := range xs {
		a, _ := json.Marshal(x)
		if string(a) == string(b) {
			return true
		}
	}
	return false
}
func lenDocs(d Document) int { x, _ := d["docs"].([]any); return len(x) }
func hasIndexType(indexes Document, wanted string) bool {
	xs, _ := indexes["indexes"].([]any)
	for _, raw := range xs {
		if index, ok := raw.(map[string]any); ok && index["type"] == wanted {
			return true
		}
	}
	return false
}
func numberInt(v any) (int, bool) {
	switch x := v.(type) {
	case json.Number:
		n, e := x.Int64()
		return int(n), e == nil
	case float64:
		return int(x), x == float64(int(x))
	case int:
		return x, true
	}
	return 0, false
}

func validSchemaName(name string) bool {
	if len(name) < 1 || len(name) > 128 {
		return false
	}
	for i, r := range name {
		alphaNum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alphaNum && (i == 0 || r != '.' && r != '_' && r != '-') {
			return false
		}
	}
	return true
}

func newID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, path, reason, expected string) {
	writeJSON(w, status, Document{"error": Document{"path": path, "reason": reason, "expected": expected}})
}

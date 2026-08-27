package stuff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Document map[string]any

type Store interface {
	Ensure(context.Context) error
	Create(context.Context, string, Document) (string, error)
	Get(context.Context, string) (Document, error)
	Put(context.Context, string, string, Document) (string, error)
	Find(context.Context, string, Document) (Document, error)
	Explain(context.Context, string, Document) (Document, error)
	Indexes(context.Context) (Document, error)
	Attachment(context.Context, string, string) (io.ReadCloser, http.Header, error)
}

type StoreError struct {
	Status int
	Reason string
}

func (e *StoreError) Error() string { return e.Reason }

type CouchStore struct {
	base     *url.URL
	db       string
	client   *http.Client
	username string
	password string
}

func NewCouchStore(rawURL, db string, client *http.Client) (*CouchStore, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid STUFF_COUCH_URL %q", rawURL)
	}
	if db == "" {
		db = "stuff"
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	username, password := "", ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
		u.User = nil
	}
	return &CouchStore{base: u, db: db, client: client, username: username, password: password}, nil
}

func (s *CouchStore) endpoint(parts ...string) string {
	endpoint := strings.TrimRight(s.base.String(), "/") + "/" + url.PathEscape(s.db)
	for _, part := range parts {
		endpoint += "/" + url.PathEscape(part)
	}
	return endpoint
}

func (s *CouchStore) authorize(req *http.Request) {
	if s.username != "" {
		req.SetBasicAuth(s.username, s.password)
	}
}

func (s *CouchStore) request(ctx context.Context, method, endpoint string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.authorize(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var ce struct{ Error, Reason string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ce)
		reason := ce.Reason
		if reason == "" {
			reason = resp.Status
		}
		return nil, &StoreError{Status: resp.StatusCode, Reason: reason}
	}
	return resp, nil
}

func decodeResponse(resp *http.Response, out any) error {
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, MaxResponseBytes+1)).Decode(out)
}

func (s *CouchStore) Ensure(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.endpoint(), nil)
	if err != nil {
		return err
	}
	s.authorize(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusPreconditionFailed {
		return nil
	}
	var ce struct{ Reason string }
	_ = json.NewDecoder(resp.Body).Decode(&ce)
	return &StoreError{Status: resp.StatusCode, Reason: "creating CouchDB database: " + ce.Reason}
}

func (s *CouchStore) Create(ctx context.Context, id string, doc Document) (string, error) {
	resp, err := s.request(ctx, http.MethodPut, s.endpoint(id), doc)
	if err != nil {
		return "", err
	}
	var result struct {
		Rev string `json:"rev"`
	}
	if err := decodeResponse(resp, &result); err != nil {
		return "", err
	}
	return result.Rev, nil
}

func (s *CouchStore) Get(ctx context.Context, id string) (Document, error) {
	resp, err := s.request(ctx, http.MethodGet, s.endpoint(id), nil)
	if err != nil {
		return nil, err
	}
	var doc Document
	return doc, decodeResponse(resp, &doc)
}

func (s *CouchStore) Put(ctx context.Context, id, rev string, doc Document) (string, error) {
	doc["_rev"] = rev
	resp, err := s.request(ctx, http.MethodPut, s.endpoint(id), doc)
	if err != nil {
		return "", err
	}
	var result struct {
		Rev string `json:"rev"`
	}
	if err := decodeResponse(resp, &result); err != nil {
		return "", err
	}
	return result.Rev, nil
}

func (s *CouchStore) Find(ctx context.Context, kind string, query Document) (Document, error) {
	query = prepareQuery(kind, query)
	resp, err := s.request(ctx, http.MethodPost, s.endpoint("_find"), query)
	if err != nil {
		return nil, err
	}
	var out Document
	if err := decodeResponse(resp, &out); err != nil {
		return nil, err
	}
	publicFindResult(out)
	return out, nil
}

func (s *CouchStore) Explain(ctx context.Context, kind string, query Document) (Document, error) {
	query = prepareQuery(kind, query)
	resp, err := s.request(ctx, http.MethodPost, s.endpoint("_explain"), query)
	if err != nil {
		return nil, err
	}
	var out Document
	return out, decodeResponse(resp, &out)
}

func (s *CouchStore) Indexes(ctx context.Context) (Document, error) {
	resp, err := s.request(ctx, http.MethodGet, s.endpoint("_index"), nil)
	if err != nil {
		return nil, err
	}
	var out Document
	return out, decodeResponse(resp, &out)
}

func (s *CouchStore) Attachment(ctx context.Context, id, name string) (io.ReadCloser, http.Header, error) {
	resp, err := s.request(ctx, http.MethodGet, s.endpoint(id, name), nil)
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, resp.Header, nil
}

func prepareQuery(kind string, in Document) Document {
	q := cloneDocument(in)
	sel, _ := q["selector"].(map[string]any)
	if sel == nil {
		sel = map[string]any{}
	}
	sel = rewriteSelector(sel)
	// CouchDB does not treat an empty object inside $and as a match-all clause;
	// it can silently return no documents. This matters for describe's bounded
	// sample and for callers issuing the natural {"selector":{}} query. Omit the
	// vacuous clause and select the public kind directly.
	if len(sel) == 0 {
		q["selector"] = map[string]any{"stuff_kind": kind}
	} else {
		q["selector"] = map[string]any{"$and": []any{map[string]any{"stuff_kind": kind}, sel}}
	}
	if fields, ok := q["fields"].([]any); ok {
		for i, f := range fields {
			if x, ok := f.(string); ok {
				fields[i] = publicToInternal(x)
			}
		}
		q["fields"] = fields
	}
	if sort, ok := q["sort"].([]any); ok {
		for i, entry := range sort {
			if s, ok := entry.(string); ok {
				sort[i] = publicToInternal(s)
				continue
			}
			if m, ok := entry.(map[string]any); ok {
				mapped := make(map[string]any, len(m))
				for key, value := range m {
					mapped[publicToInternal(key)] = value
				}
				sort[i] = mapped
			}
		}
	}
	return q
}

func publicToInternal(s string) string {
	if s == "id" {
		return "_id"
	}
	if s == "revision" {
		return "_rev"
	}
	return s
}

func rewriteSelector(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for key, value := range m {
		switch key {
		case "$and", "$or", "$nor":
			if xs, ok := value.([]any); ok {
				for i, x := range xs {
					if child, ok := x.(map[string]any); ok {
						xs[i] = rewriteSelector(child)
					}
				}
				value = xs
			}
		case "$not":
			if child, ok := value.(map[string]any); ok {
				value = rewriteSelector(child)
			}
		default:
			if !strings.HasPrefix(key, "$") {
				key = publicToInternal(key)
			}
		}
		out[key] = value
	}
	return out
}

func publicFindResult(result Document) {
	docs, _ := result["docs"].([]any)
	for i, raw := range docs {
		if d, ok := raw.(map[string]any); ok {
			docs[i] = publicDocument(d)
		}
	}
}

func publicDocument(doc Document) Document {
	out := cloneDocument(doc)
	if id, ok := out["_id"]; ok {
		out["id"] = id
	}
	if rev, ok := out["_rev"]; ok {
		out["revision"] = rev
	}
	delete(out, "_id")
	delete(out, "_rev")
	delete(out, "stuff_kind")
	delete(out, "stuff_attachment_meta")
	if raw, ok := out["_attachments"].(map[string]any); ok {
		metas, _ := doc["stuff_attachment_meta"].(map[string]any)
		names := make([]string, 0, len(raw))
		for name := range raw {
			names = append(names, name)
		}
		sort.Strings(names)
		list := make([]any, 0, len(raw))
		for _, name := range names {
			am, _ := raw[name].(map[string]any)
			d := map[string]any{"name": name, "media_type": am["content_type"], "bytes": am["length"]}
			if meta, ok := metas[name].(map[string]any); ok {
				for k, v := range meta {
					d[k] = v
				}
			}
			list = append(list, d)
		}
		out["attachments"] = list
	}
	delete(out, "_attachments")
	return out
}

func cloneDocument(in Document) Document {
	b, _ := json.Marshal(in)
	var out Document
	_ = json.Unmarshal(b, &out)
	return out
}

package stuff

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const readPageSize = 20

var readTemplates = template.Must(template.New("read").Parse(readTemplateText))

type readItem struct {
	ID, Name, CreatedAt, UpdatedAt, ActivityAt, ActivitySource, Path, Metadata string
}

type readAttachment struct {
	Name, MediaType, Path string
	Bytes                 any
}

type readNote struct {
	ID, CreatedAt, UpdatedAt, Metadata string
	Body                               template.HTML
	Attachments                        []readAttachment
}

type readIndexData struct {
	Items                           []readItem
	Page, Pages                     int
	Previous, Next                  int
	HasPrevious, HasNext, Truncated bool
}

type readDetailData struct {
	Item      readItem
	Notes     []readNote
	Truncated bool
}

func (s *Server) serveReadRoute(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == "/":
		http.Redirect(w, r, "/read", http.StatusSeeOther)
		return true
	case r.URL.Path == "/read":
		s.serveReadIndex(w, r)
		return true
	case strings.HasPrefix(r.URL.Path, "/read/items/"):
		id, ok := onePathPart(strings.TrimPrefix(r.URL.Path, "/read/items/"))
		if !ok {
			readHTTPError(w, http.StatusNotFound, "Item not found")
			return true
		}
		s.serveReadItem(w, r, id)
		return true
	case strings.HasPrefix(r.URL.Path, "/read/notes/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/read/notes/"), "/")
		if len(parts) != 3 || parts[1] != "attachments" {
			readHTTPError(w, http.StatusNotFound, "Attachment not found")
			return true
		}
		id, errID := url.PathUnescape(parts[0])
		name, errName := url.PathUnescape(parts[2])
		if errID != nil || errName != nil || id == "" || name == "" {
			readHTTPError(w, http.StatusNotFound, "Attachment not found")
			return true
		}
		if err := s.attachment(w, r, id, name); err != nil {
			s.readError(w, err)
		}
		return true
	default:
		return false
	}
}

func onePathPart(raw string) (string, bool) {
	if raw == "" || strings.Contains(raw, "/") {
		return "", false
	}
	part, err := url.PathUnescape(raw)
	return part, err == nil && part != ""
}

func (s *Server) serveReadIndex(w http.ResponseWriter, r *http.Request) {
	page, err := readPage(r)
	if err != nil {
		readHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, itemFull, err := s.readDocuments(r, "item", Document{"selector": Document{}, "limit": MaxPageSize})
	if err != nil {
		s.readError(w, err)
		return
	}
	notes, noteFull, err := s.readDocuments(r, "note", Document{"selector": Document{}, "limit": MaxPageSize})
	if err != nil {
		s.readError(w, err)
		return
	}

	byID := make(map[string]*readItem, len(items))
	list := make([]readItem, 0, len(items))
	for _, doc := range items {
		item := documentReadItem(doc)
		list = append(list, item)
		byID[item.ID] = &list[len(list)-1]
	}
	for _, note := range notes {
		item := byID[stringValue(note["item_id"])]
		if item == nil {
			continue
		}
		candidate := newestTimestamp(stringValue(note["updated_at"]), stringValue(note["created_at"]))
		if timestampAfter(candidate, item.ActivityAt) {
			item.ActivityAt = candidate
			item.ActivitySource = "note activity"
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].ActivityAt != list[j].ActivityAt {
			return timestampAfter(list[i].ActivityAt, list[j].ActivityAt)
		}
		return list[i].ID < list[j].ID
	})

	pages := (len(list) + readPageSize - 1) / readPageSize
	if pages == 0 {
		pages = 1
	}
	if page > pages {
		readHTTPError(w, http.StatusNotFound, "Page not found")
		return
	}
	start := (page - 1) * readPageSize
	end := start + readPageSize
	if end > len(list) {
		end = len(list)
	}
	data := readIndexData{
		Items: list[start:end], Page: page, Pages: pages,
		Previous: page - 1, Next: page + 1,
		HasPrevious: page > 1, HasNext: page < pages,
		Truncated: itemFull || noteFull,
	}
	s.renderRead(w, "index", data)
}

func (s *Server) serveReadItem(w http.ResponseWriter, r *http.Request, id string) {
	doc, err := s.store.Get(r.Context(), id)
	if err != nil {
		s.readError(w, err)
		return
	}
	if doc["stuff_kind"] != "item" {
		readHTTPError(w, http.StatusNotFound, "Item not found")
		return
	}
	item := documentReadItem(publicDocument(doc))
	docs, full, err := s.readDocuments(r, "note", Document{"selector": Document{"item_id": id}, "limit": MaxPageSize})
	if err != nil {
		s.readError(w, err)
		return
	}
	notes := make([]readNote, 0, len(docs))
	for _, note := range docs {
		if stringValue(note["item_id"]) != id {
			continue
		}
		rn := readNote{
			ID: stringValue(note["id"]), CreatedAt: stringValue(note["created_at"]),
			UpdatedAt: stringValue(note["updated_at"]), Metadata: prettyJSON(note["metadata"]),
			Body: renderConservativeMarkdown(stringValue(note["text"])),
		}
		if raw, ok := note["attachments"].([]any); ok {
			for _, value := range raw {
				a, _ := value.(map[string]any)
				name := stringValue(a["name"])
				rn.Attachments = append(rn.Attachments, readAttachment{
					Name: name, MediaType: stringValue(a["media_type"]), Bytes: a["bytes"],
					Path: "/read/notes/" + url.PathEscape(rn.ID) + "/attachments/" + url.PathEscape(name),
				})
			}
		}
		notes = append(notes, rn)
	}
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].CreatedAt != notes[j].CreatedAt {
			return timestampAfter(notes[j].CreatedAt, notes[i].CreatedAt)
		}
		return notes[i].ID < notes[j].ID
	})
	s.renderRead(w, "detail", readDetailData{Item: item, Notes: notes, Truncated: full})
}

func (s *Server) readDocuments(r *http.Request, kind string, query Document) ([]Document, bool, error) {
	out, err := s.store.Find(r.Context(), kind, query)
	if err != nil {
		return nil, false, err
	}
	raw, _ := out["docs"].([]any)
	docs := make([]Document, 0, len(raw))
	for _, value := range raw {
		switch doc := value.(type) {
		case Document:
			docs = append(docs, doc)
		case map[string]any:
			docs = append(docs, Document(doc))
		}
	}
	return docs, len(docs) >= MaxPageSize, nil
}

func documentReadItem(doc Document) readItem {
	updated := stringValue(doc["updated_at"])
	return readItem{
		ID: stringValue(doc["id"]), Name: stringValue(doc["name"]),
		CreatedAt: stringValue(doc["created_at"]), UpdatedAt: updated,
		ActivityAt:     newestTimestamp(updated, stringValue(doc["created_at"])),
		ActivitySource: "item activity", Metadata: prettyJSON(doc["metadata"]),
		Path: "/read/items/" + url.PathEscape(stringValue(doc["id"])),
	}
}

func readPage(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("page")
	if raw == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page < 1 {
		return 0, fmt.Errorf("page must be a positive integer")
	}
	return page, nil
}

func newestTimestamp(values ...string) string {
	best := ""
	for _, value := range values {
		if timestampAfter(value, best) {
			best = value
		}
	}
	return best
}

func timestampAfter(a, b string) bool {
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	if ea == nil && eb == nil {
		return ta.After(tb)
	}
	if ea == nil {
		return true
	}
	if eb == nil {
		return false
	}
	return a > b
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

func prettyJSON(value any) string {
	if value == nil {
		return "null"
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

// renderConservativeMarkdown supports common block structure while treating all
// source text, including raw HTML, as text. Only fixed tags introduced here are
// trusted before the result reaches html/template.
func renderConservativeMarkdown(source string) template.HTML {
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	var out strings.Builder
	inCode, inList, inParagraph := false, false, false
	closeParagraph := func() {
		if inParagraph {
			out.WriteString("</p>")
			inParagraph = false
		}
	}
	closeList := func() {
		if inList {
			out.WriteString("</ul>")
			inList = false
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			closeParagraph()
			closeList()
			if inCode {
				out.WriteString("</code></pre>")
			} else {
				out.WriteString("<pre><code>")
			}
			inCode = !inCode
			continue
		}
		if inCode {
			out.WriteString(html.EscapeString(line))
			out.WriteByte('\n')
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			closeParagraph()
			closeList()
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			closeParagraph()
			if !inList {
				out.WriteString("<ul>")
				inList = true
			}
			out.WriteString("<li>" + html.EscapeString(strings.TrimSpace(trimmed[2:])) + "</li>")
			continue
		}
		closeList()
		level := 0
		for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
			level++
		}
		if level > 0 && len(trimmed) > level && trimmed[level] == ' ' {
			closeParagraph()
			fmt.Fprintf(&out, "<h%d>%s</h%d>", level, html.EscapeString(strings.TrimSpace(trimmed[level+1:])), level)
			continue
		}
		if !inParagraph {
			out.WriteString("<p>")
			inParagraph = true
		} else {
			out.WriteString("<br>")
		}
		out.WriteString(html.EscapeString(line))
	}
	if inCode {
		out.WriteString("</code></pre>")
	}
	closeParagraph()
	closeList()
	return template.HTML(out.String()) // #nosec G203 -- every source fragment is escaped above.
}

func (s *Server) renderRead(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := readTemplates.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering read surface", "template", name, "error", err)
	}
}

func (s *Server) readError(w http.ResponseWriter, err error) {
	var storeErr *StoreError
	if errors.As(err, &storeErr) && storeErr.Status == http.StatusNotFound {
		readHTTPError(w, http.StatusNotFound, "Not found")
		return
	}
	s.log.Error("read surface request failed", "error", err)
	readHTTPError(w, http.StatusInternalServerError, "Unable to read Stuff")
}

func readHTTPError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self'")
	http.Error(w, message, status)
}

const readTemplateText = `
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Stuff</title><style>
:root{color-scheme:light;--ink:#25231f;--muted:#716d64;--line:#dedbd3;--paper:#faf9f6;--card:#fff;--accent:#385d54}*{box-sizing:border-box}body{margin:0;background:var(--paper);color:var(--ink);font:16px/1.55 ui-sans-serif,system-ui,-apple-system,sans-serif}main{width:min(880px,calc(100% - 2rem));margin:3rem auto 6rem}header{margin-bottom:2rem}h1,h2,h3{line-height:1.2}h1{font-size:2rem;margin:.2rem 0}a{color:var(--accent)}.subtle,.stamp{color:var(--muted);font-size:.9rem}.card,.note{display:block;background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1rem 1.15rem;margin:.8rem 0;text-decoration:none;color:inherit}.card:hover{border-color:var(--accent)}.card h2,.note h2{font-size:1.12rem;margin:0 0 .35rem}.warning{border-left:4px solid #b58134;background:#fff7e9;padding:.7rem 1rem}.pager{display:flex;justify-content:space-between;margin-top:1.5rem}.meta{white-space:pre-wrap;overflow-wrap:anywhere;background:#f1f0eb;padding:.8rem;border-radius:6px;font:13px/1.45 ui-monospace,monospace}.markdown pre{overflow:auto;background:#f1f0eb;padding:.8rem;border-radius:6px}.markdown code{font-family:ui-monospace,monospace}.markdown h1{font-size:1.35rem}.markdown h2{font-size:1.2rem}.markdown h3{font-size:1.08rem}.attachments{padding-left:1.2rem}.back{display:inline-block;margin-bottom:1rem}@media(max-width:520px){main{margin-top:1.5rem}.stamp{display:block}}</style></head><body><main>{{end}}
{{define "foot"}}</main></body></html>{{end}}
{{define "index"}}{{template "head" .}}<header><div class="subtle">Durable Items and Notes</div><h1>Stuff</h1><div class="subtle">Recently active · page {{.Page}} of {{.Pages}}</div></header>{{if .Truncated}}<p class="warning">This view is bounded to the newest available sample of 200 Items and 200 Notes; older activity may be omitted.</p>{{end}}{{if .Items}}{{range .Items}}<a class="card" href="{{.Path}}"><h2>{{.Name}}</h2><div class="stamp">{{.ActivityAt}} · {{.ActivitySource}}</div><div class="subtle">{{.ID}}</div></a>{{end}}{{else}}<p>No Items yet.</p>{{end}}<nav class="pager"><span>{{if .HasPrevious}}<a href="/read?page={{.Previous}}">← Newer</a>{{end}}</span><span>{{if .HasNext}}<a href="/read?page={{.Next}}">Older →</a>{{end}}</span></nav>{{template "foot" .}}{{end}}
{{define "detail"}}{{template "head" .}}<a class="back" href="/read">← All Items</a><header><h1>{{.Item.Name}}</h1><div class="subtle">{{.Item.ID}}</div><div class="stamp">Created {{.Item.CreatedAt}} · Item updated {{.Item.UpdatedAt}}</div></header><h2>Metadata</h2><pre class="meta">{{.Item.Metadata}}</pre><h2>Notes</h2>{{if .Truncated}}<p class="warning">Only the first 200 matching Notes are shown.</p>{{end}}{{if .Notes}}{{range .Notes}}<article class="note"><h2>{{.CreatedAt}}</h2><div class="subtle">{{.ID}}</div>{{if .Body}}<div class="markdown">{{.Body}}</div>{{end}}<details><summary>Metadata</summary><pre class="meta">{{.Metadata}}</pre></details>{{if .Attachments}}<h3>Attachments</h3><ul class="attachments">{{range .Attachments}}<li><a href="{{.Path}}">{{.Name}}</a> <span class="subtle">{{.MediaType}} · {{.Bytes}} bytes · download</span></li>{{end}}</ul>{{end}}</article>{{end}}{{else}}<p>No Notes yet.</p>{{end}}{{template "foot" .}}{{end}}
`

package stuff

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	readPageSize         = 20
	maxReadNoteTextBytes = MaxJSONBytes
	maxReadNoteFormBytes = 3*maxReadNoteTextBytes + 1024 // URL encoding may expand every UTF-8 byte to %XX.
)

var readTemplates = template.Must(template.New("read").Parse(readTemplateText))

type readItem struct {
	ID, ShortID, Name, CreatedAt, UpdatedAt, ActivityAt, ActivitySource, Path, Metadata string
	NoteCount                                                                           int
}

type readAttachment struct {
	Name, MediaType, Path, ViewPath string
	Bytes                           any
	CanView                         bool
}

type readNote struct {
	ID, ShortID, CreatedAt, UpdatedAt, Metadata string
	Body                                        template.HTML
	Attachments                                 []readAttachment
}

type readIndexData struct {
	Items                           []readItem
	Page, Pages                     int
	Previous, Next                  int
	HasPrevious, HasNext, Truncated bool
}

type readDetailData struct {
	Item       readItem
	Notes      []readNote
	NoteAction string
	Warning    string
	Truncated  bool
}

func (s *Server) serveReadRoute(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == "/":
		s.serveReadHome(w, r)
		return true
	case r.URL.Path == "/read":
		s.serveReadIndex(w, r)
		return true
	case r.URL.Path == "/read/activity.js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(readActivityScript))
		return true
	case r.URL.Path == "/read/view-host.js":
		serveReadViewHostScript(w)
		return true
	case strings.HasPrefix(r.URL.Path, "/read/items/"):
		raw := strings.TrimPrefix(r.URL.Path, "/read/items/")
		parts := strings.Split(raw, "/")
		if len(parts) == 2 && (parts[1] == "view" || parts[1] == "snapshot") {
			id, err := url.PathUnescape(parts[0])
			if err != nil || id == "" || strings.Contains(id, "/") {
				readHTTPError(w, http.StatusNotFound, "Item not found")
				return true
			}
			if parts[1] == "view" {
				s.serveReadViewDocument(w, r, id)
			} else {
				s.serveReadViewSnapshot(w, r, id)
			}
			return true
		}
		id, ok := onePathPart(raw)
		if !ok {
			readHTTPError(w, http.StatusNotFound, "Item not found")
			return true
		}
		s.serveReadItem(w, r, id)
		return true
	case strings.HasPrefix(r.URL.Path, "/read/notes/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/read/notes/"), "/")
		if (len(parts) != 3 && len(parts) != 4) || parts[1] != "attachments" || (len(parts) == 4 && parts[3] != "view") {
			readHTTPError(w, http.StatusNotFound, "Attachment not found")
			return true
		}
		id, errID := url.PathUnescape(parts[0])
		name, errName := url.PathUnescape(parts[2])
		if errID != nil || errName != nil || id == "" || name == "" {
			readHTTPError(w, http.StatusNotFound, "Attachment not found")
			return true
		}
		var err error
		if len(parts) == 4 {
			err = s.viewHTMLAttachment(w, r, id, name)
		} else {
			err = s.attachment(w, r, id, name)
		}
		if err != nil {
			s.readError(w, err)
		}
		return true
	default:
		return false
	}
}

func readItemNoteTarget(path string) (string, bool) {
	raw := strings.TrimPrefix(path, "/read/items/")
	if raw == path {
		return "", false
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || parts[1] != "notes" {
		return "", false
	}
	id, err := url.PathUnescape(parts[0])
	return id, err == nil && id != "" && !strings.Contains(id, "/")
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
		item.NoteCount++
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

func (s *Server) createReadNote(w http.ResponseWriter, r *http.Request, itemID string) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		readHTTPError(w, http.StatusForbidden, "Cross-site Note creation is not allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		readHTTPError(w, http.StatusUnsupportedMediaType, "Expected an HTML form submission")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxReadNoteFormBytes)+1))
	if err != nil {
		readHTTPError(w, http.StatusBadRequest, "Unable to read Note text")
		return
	}
	if len(body) > maxReadNoteFormBytes {
		readHTTPError(w, http.StatusRequestEntityTooLarge, "Note text is too large")
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		readHTTPError(w, http.StatusBadRequest, "Invalid Note form submission")
		return
	}
	if len(values) != 1 || len(values["text"]) != 1 {
		readHTTPError(w, http.StatusBadRequest, "The Note form must contain exactly one text field")
		return
	}
	text := values.Get("text")
	if !utf8.ValidString(text) {
		readHTTPError(w, http.StatusBadRequest, "Note text must be valid UTF-8")
		return
	}
	if strings.TrimSpace(text) == "" {
		readHTTPError(w, http.StatusBadRequest, "Note text cannot be empty")
		return
	}
	if len([]byte(text)) > maxReadNoteTextBytes {
		readHTTPError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("Note text exceeds the %d-byte limit", maxReadNoteTextBytes))
		return
	}

	item, err := s.store.Get(r.Context(), itemID)
	if err != nil {
		s.readError(w, err)
		return
	}
	if item["stuff_kind"] != "item" {
		readHTTPError(w, http.StatusNotFound, "Item not found")
		return
	}
	id, err := newID("note_")
	if err != nil {
		s.readError(w, err)
		return
	}
	doc := s.newNoteDocument(itemID, &text, Document{})
	if _, err := s.store.Create(r.Context(), id, doc); err != nil {
		s.readError(w, err)
		return
	}
	location := "/read/items/" + url.PathEscape(itemID)
	if stringValue(item["view_id"]) != "" {
		location += "?plain=1"
	}
	w.Header().Del("Content-Type")
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusSeeOther)
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
	warning := ""
	if r.URL.Query().Get("plain") != "1" {
		served, fallbackWarning, err := s.maybeServeReadViewHost(w, r, doc)
		if err != nil {
			s.readError(w, err)
			return
		}
		if served {
			return
		}
		warning = fallbackWarning
	}
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
			ID: stringValue(note["id"]), ShortID: shortID(stringValue(note["id"])), CreatedAt: stringValue(note["created_at"]),
			UpdatedAt: stringValue(note["updated_at"]), Metadata: prettyJSON(note["metadata"]),
			Body: renderConservativeMarkdown(stringValue(note["text"])),
		}
		if raw, ok := note["attachments"].([]any); ok {
			for _, value := range raw {
				a, _ := value.(map[string]any)
				name := stringValue(a["name"])
				mediaType := stringValue(a["media_type"])
				path := "/read/notes/" + url.PathEscape(rn.ID) + "/attachments/" + url.PathEscape(name)
				rn.Attachments = append(rn.Attachments, readAttachment{
					Name: name, MediaType: mediaType, Bytes: a["bytes"], Path: path,
					CanView:  strings.EqualFold(strings.TrimSpace(strings.Split(mediaType, ";")[0]), "text/html"),
					ViewPath: path + "/view",
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
	s.renderRead(w, "detail", readDetailData{Item: item, Notes: notes, NoteAction: "/read/items/" + url.PathEscape(id) + "/notes", Warning: warning, Truncated: full})
}

func (s *Server) viewHTMLAttachment(w http.ResponseWriter, r *http.Request, id, name string) error {
	doc, err := s.store.Get(r.Context(), id)
	if err != nil {
		return err
	}
	if doc["stuff_kind"] != "note" {
		return &StoreError{Status: http.StatusNotFound, Reason: "Note not found"}
	}
	body, headers, err := s.store.Attachment(r.Context(), id, name)
	if err != nil {
		return err
	}
	defer body.Close()
	mediaType, _, _ := mime.ParseMediaType(headers.Get("Content-Type"))
	if !strings.EqualFold(mediaType, "text/html") {
		return &StoreError{Status: http.StatusNotFound, Reason: "HTML attachment not found"}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", headers.Get("Content-Length"))
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:; base-uri 'none'; form-action 'none'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, body)
	return err
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
	id := stringValue(doc["id"])
	return readItem{
		ID: id, ShortID: shortID(id), Name: stringValue(doc["name"]),
		CreatedAt: stringValue(doc["created_at"]), UpdatedAt: updated,
		ActivityAt:     newestTimestamp(updated, stringValue(doc["created_at"])),
		ActivitySource: "item activity", Metadata: prettyJSON(doc["metadata"]),
		Path: "/read/items/" + url.PathEscape(stringValue(doc["id"])),
	}
}

func shortID(id string) string {
	prefixEnd := strings.IndexByte(id, '_')
	if prefixEnd < 0 || len(id)-prefixEnd-1 <= 8 {
		return id
	}
	return id[:prefixEnd+1] + id[prefixEnd+1:prefixEnd+9]
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'self'")
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

const readActivityScript = `(() => {
  const relative = new Intl.RelativeTimeFormat(undefined, {numeric: "auto"});
  const now = Date.now();
  for (const element of document.querySelectorAll("time[data-time]")) {
    const timestamp = Date.parse(element.dateTime);
    if (!Number.isFinite(timestamp)) continue;
    const seconds = (timestamp - now) / 1000;
    let divisor = 1, unit = "second";
    if (Math.abs(seconds) >= 86400) { divisor = 86400; unit = "day"; }
    else if (Math.abs(seconds) >= 3600) { divisor = 3600; unit = "hour"; }
    else if (Math.abs(seconds) >= 60) { divisor = 60; unit = "minute"; }
    element.title = element.dateTime;
    element.textContent = relative.format(Math.round(seconds / divisor), unit);
  }

  for (const button of document.querySelectorAll("[data-copy-id]")) {
    button.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(button.dataset.copyId);
        const previous = button.textContent;
        button.textContent = "Copied";
        setTimeout(() => { button.textContent = previous; }, 900);
      } catch (_) {}
    });
  }

  const noteList = document.getElementById("notes-list");
  const orderButton = document.getElementById("notes-order");
  if (noteList && orderButton) {
    let newestFirst = false;
    try { newestFirst = localStorage.getItem("stuff.read.notesNewestFirst") === "true"; } catch (_) {}
    const applyOrder = (next) => {
      if (next !== (noteList.dataset.order === "newest")) {
        for (const note of Array.from(noteList.children).reverse()) noteList.append(note);
      }
      noteList.dataset.order = next ? "newest" : "oldest";
      orderButton.textContent = next ? "Oldest first" : "Newest first";
      orderButton.setAttribute("aria-pressed", String(next));
      newestFirst = next;
      try { localStorage.setItem("stuff.read.notesNewestFirst", String(next)); } catch (_) {}
    };
    applyOrder(newestFirst);
    orderButton.addEventListener("click", () => applyOrder(!newestFirst));
  }

  try {
    const key = "stuff.read.lastSeen";
    const previous = localStorage.getItem(key);
    const cards = Array.from(document.querySelectorAll("[data-activity]"));
    const previousTime = previous ? Date.parse(previous) : NaN;
    if (Number.isFinite(previousTime)) {
      const firstSeen = cards.find((card) => Date.parse(card.dataset.activity) <= previousTime);
      if (firstSeen) {
        const divider = document.createElement("div");
        divider.className = "seen-divider";
        divider.textContent = "Seen before your last visit";
        firstSeen.before(divider);
      }
    }
    if (cards.length) {
      const newest = cards[0].dataset.activity;
      const newestTime = Date.parse(newest);
      if (Number.isFinite(newestTime) && (!Number.isFinite(previousTime) || newestTime > previousTime)) {
        localStorage.setItem(key, newest);
      }
    }
  } catch (_) {
    // Reading Stuff must keep working when storage is disabled.
  }
})();
`

const readTemplateText = `
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Stuff</title><style>
:root{color-scheme:light;--ink:#25231f;--muted:#716d64;--line:#dedbd3;--paper:#faf9f6;--card:#fff;--accent:#385d54}*{box-sizing:border-box}body{margin:0;background:var(--paper);color:var(--ink);font:16px/1.55 ui-sans-serif,system-ui,-apple-system,sans-serif}main{width:min(880px,calc(100% - 2rem));margin:3rem auto 6rem}header{margin-bottom:2rem}h1,h2,h3{line-height:1.2}h1{font-size:2rem;margin:.2rem 0}a{color:var(--accent)}.subtle,.stamp{color:var(--muted);font-size:.9rem}.card,.note{display:block;background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1rem 1.15rem;margin:.8rem 0;text-decoration:none;color:inherit}.card:hover{border-color:var(--accent)}.card h2,.note h2{font-size:1.12rem;margin:0 0 .35rem}.warning{border-left:4px solid #b58134;background:#fff7e9;padding:.7rem 1rem}.pager{display:flex;justify-content:space-between;margin-top:1.5rem}.meta{white-space:pre-wrap;overflow-wrap:anywhere;background:#f1f0eb;padding:.8rem;border-radius:6px;font:13px/1.45 ui-monospace,monospace}.markdown pre{overflow:auto;background:#f1f0eb;padding:.8rem;border-radius:6px}.markdown code{font-family:ui-monospace,monospace}.markdown h1{font-size:1.35rem}.markdown h2{font-size:1.2rem}.markdown h3{font-size:1.08rem}.attachments{padding-left:1.2rem}.back{display:inline-block;margin-bottom:1rem}.id-button,.order-button{appearance:none;border:1px solid var(--line);border-radius:6px;background:#fff;color:var(--accent);padding:.2rem .45rem;font:inherit;cursor:pointer}.id-button{font:13px/1.35 ui-monospace,monospace}.notes-head{display:flex;align-items:center;justify-content:space-between;gap:1rem}.notes-head h2{margin-bottom:.4rem}.note-form{margin:1rem 0 1.8rem}.note-form label{display:block;font-weight:600;margin-bottom:.35rem}.note-form textarea{display:block;width:100%;min-height:8rem;resize:vertical;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--ink);padding:.7rem;font:inherit}.note-form button{margin-top:.6rem;border:0;border-radius:7px;background:var(--accent);color:#fff;padding:.55rem .9rem;font:inherit;font-weight:600;cursor:pointer}.seen-divider{display:flex;align-items:center;gap:.7rem;color:var(--muted);font-size:.82rem;margin:1.4rem 0}.seen-divider:before,.seen-divider:after{content:"";height:1px;background:var(--line);flex:1}@media(max-width:520px){main{margin-top:1.5rem}.stamp{display:block}}</style></head><body><main>{{end}}
{{define "foot"}}</main></body></html>{{end}}
{{define "index"}}{{template "head" .}}<header><div class="subtle">Durable Items and Notes</div><h1>Stuff</h1><div class="subtle">Recently active · page {{.Page}} of {{.Pages}}</div></header>{{if .Truncated}}<p class="warning">This view is bounded to a sample of 200 Items and 200 Notes; activity outside that sample may be omitted.</p>{{end}}{{if .Items}}{{range .Items}}<a class="card" data-activity="{{.ActivityAt}}" href="{{.Path}}"><h2>{{.Name}}</h2><div class="stamp"><time data-time datetime="{{.ActivityAt}}">{{.ActivityAt}}</time> · {{.ActivitySource}} · {{.NoteCount}} {{if eq .NoteCount 1}}Note{{else}}Notes{{end}}</div><div class="subtle" title="{{.ID}}">{{.ShortID}}</div></a>{{end}}{{else}}<p>No Items yet.</p>{{end}}<nav class="pager"><span>{{if .HasPrevious}}<a href="/read?page={{.Previous}}">← Newer</a>{{end}}</span><span>{{if .HasNext}}<a href="/read?page={{.Next}}">Older →</a>{{end}}</span></nav><script src="/read/activity.js" defer></script>{{template "foot" .}}{{end}}
{{define "detail"}}{{template "head" .}}<a class="back" href="/read">← All Items</a>{{if .Warning}}<p class="warning">{{.Warning}}</p>{{end}}<header><h1>{{.Item.Name}}</h1><button class="id-button" type="button" data-copy-id="{{.Item.ID}}" title="Copy {{.Item.ID}}">{{.Item.ShortID}}</button><div class="stamp">Created <time data-time datetime="{{.Item.CreatedAt}}">{{.Item.CreatedAt}}</time> · Item updated <time data-time datetime="{{.Item.UpdatedAt}}">{{.Item.UpdatedAt}}</time></div></header><h2>Metadata</h2><pre class="meta">{{.Item.Metadata}}</pre><form class="note-form" method="post" action="{{.NoteAction}}"><label for="new-note-text">Add a Note</label><textarea id="new-note-text" name="text" maxlength="1048576" required aria-describedby="new-note-help"></textarea><div id="new-note-help" class="subtle">Plain text or Markdown · up to 1 MiB</div><button type="submit">Add Note</button></form><div class="notes-head"><h2>Notes <span class="subtle">({{len .Notes}})</span></h2>{{if .Notes}}<button id="notes-order" class="order-button" type="button" aria-pressed="false">Newest first</button>{{end}}</div>{{if .Truncated}}<p class="warning">Only the first 200 matching Notes are shown.</p>{{end}}{{if .Notes}}<div id="notes-list" data-order="oldest">{{range .Notes}}<article class="note"><h2><time data-time datetime="{{.CreatedAt}}">{{.CreatedAt}}</time></h2><button class="id-button" type="button" data-copy-id="{{.ID}}" title="Copy {{.ID}}">{{.ShortID}}</button>{{if .Body}}<div class="markdown">{{.Body}}</div>{{end}}<details><summary>Metadata</summary><pre class="meta">{{.Metadata}}</pre></details>{{if .Attachments}}<h3>Attachments</h3><ul class="attachments">{{range .Attachments}}<li>{{if .CanView}}<a href="{{.ViewPath}}">View {{.Name}}</a> · {{end}}<a href="{{.Path}}">Download</a> <span class="subtle">{{.MediaType}} · {{.Bytes}} bytes</span></li>{{end}}</ul>{{end}}</article>{{end}}</div>{{else}}<p>No Notes yet.</p>{{end}}<script src="/read/activity.js" defer></script>{{template "foot" .}}{{end}}
`

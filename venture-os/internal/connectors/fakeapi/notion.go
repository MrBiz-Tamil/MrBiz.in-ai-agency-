package fakeapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Notion fakes the Notion API (2022-06-28): create page in a database, append block children, and search.
type Notion struct {
	base
	Token      string
	DatabaseID string
	// Title is the database's title column; Columns are its other (text) columns.
	Title   string
	Columns map[string]bool

	nmu   sync.Mutex
	pages map[string]*notionPage
	seq   int
}

type notionPage struct {
	ID, URL, Title string
	Props          map[string]string
	Blocks         []string
}

// NewNotion starts the fake. Columns lists the text columns the database has besides its title.
func NewNotion(token, databaseID, title string, columns ...string) *Notion {
	n := &Notion{Token: token, DatabaseID: databaseID, Title: title, Columns: map[string]bool{}, pages: map[string]*notionPage{}}
	for _, c := range columns {
		n.Columns[c] = true
	}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	return n
}

// Pages returns the stored pages.
func (n *Notion) Pages() []notionPage {
	n.nmu.Lock()
	defer n.nmu.Unlock()
	var out []notionPage
	for _, p := range n.pages {
		out = append(out, *p)
	}
	return out
}

// Page returns one stored page.
func (n *Notion) Page(id string) *notionPage {
	n.nmu.Lock()
	defer n.nmu.Unlock()
	return n.pages[id]
}

func notionErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"object": "error", "status": status, "code": code, "message": msg})
}

func (n *Notion) serve(w http.ResponseWriter, r *http.Request) {
	c, injected := n.record(r)
	if injected != 0 {
		code := map[int]string{429: "rate_limited", 409: "conflict_error", 500: "internal_server_error", 503: "service_unavailable"}[injected]
		if injected == 429 {
			w.Header().Set("Retry-After", "1")
		}
		notionErr(w, injected, code, "injected failure")
		return
	}
	if r.Header.Get("Notion-Version") != "2022-06-28" {
		notionErr(w, 400, "invalid_request", "Notion-Version header is required")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+n.Token {
		notionErr(w, 401, "unauthorized", "API token is invalid.")
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/pages":
		n.create(w, c)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/blocks/") && strings.HasSuffix(r.URL.Path, "/children"):
		n.appendChildren(w, c, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/search":
		n.search(w, c)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/users/me":
		writeJSON(w, 200, map[string]any{"object": "user", "id": "bot-1", "type": "bot"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/databases/"):
		if strings.TrimPrefix(r.URL.Path, "/v1/databases/") != n.DatabaseID {
			notionErr(w, 404, "object_not_found", "Could not find database. Make sure the relevant pages and databases are shared with your integration.")
			return
		}
		props := map[string]any{n.Title: map[string]string{"type": "title"}}
		for col := range n.Columns {
			props[col] = map[string]string{"type": "rich_text"}
		}
		writeJSON(w, 200, map[string]any{"object": "database", "id": n.DatabaseID, "properties": props})
	default:
		notionErr(w, 404, "invalid_request_url", "Invalid request URL.")
	}
}

type blockIn struct {
	Type      string `json:"type"`
	Paragraph struct {
		RichText []struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"rich_text"`
	} `json:"paragraph"`
}

// checkBlocks enforces Notion's documented limits and returns the paragraph texts.
func checkBlocks(w http.ResponseWriter, raw []json.RawMessage) ([]string, bool) {
	if len(raw) > 100 {
		notionErr(w, 400, "validation_error", "body.children.length should be ≤ 100")
		return nil, false
	}
	var out []string
	for _, r := range raw {
		var b blockIn
		if json.Unmarshal(r, &b) != nil || b.Type != "paragraph" || len(b.Paragraph.RichText) == 0 {
			notionErr(w, 400, "validation_error", "body.children[0].paragraph.rich_text should be defined")
			return nil, false
		}
		for _, t := range b.Paragraph.RichText {
			if len([]rune(t.Text.Content)) > 2000 {
				notionErr(w, 400, "validation_error", "body.children[0].paragraph.rich_text[0].text.content.length should be ≤ 2000")
				return nil, false
			}
			out = append(out, t.Text.Content)
		}
	}
	return out, true
}

func (n *Notion) create(w http.ResponseWriter, c Call) {
	var in struct {
		Parent     map[string]string         `json:"parent"`
		Properties map[string]map[string]any `json:"properties"`
		Children   []json.RawMessage         `json:"children"`
	}
	if json.Unmarshal(c.Body, &in) != nil {
		notionErr(w, 400, "invalid_json", "Error parsing JSON body.")
		return
	}
	if in.Parent["database_id"] != n.DatabaseID {
		notionErr(w, 404, "object_not_found", "Could not find database. Make sure the relevant pages and databases are shared with your integration.")
		return
	}
	p := &notionPage{Props: map[string]string{}}
	for name, v := range in.Properties {
		switch {
		case name == n.Title:
			items, _ := v["title"].([]any)
			if len(items) == 0 {
				notionErr(w, 400, "validation_error", "properties.title.title should be defined")
				return
			}
			p.Title = fmt.Sprint(items[0].(map[string]any)["text"].(map[string]any)["content"])
		case n.Columns[name]:
			items, _ := v["rich_text"].([]any)
			if len(items) == 0 {
				notionErr(w, 400, "validation_error", name+" is expected to be rich_text.")
				return
			}
			content := fmt.Sprint(items[0].(map[string]any)["text"].(map[string]any)["content"])
			if len([]rune(content)) > 2000 {
				notionErr(w, 400, "validation_error", name+".rich_text[0].text.content.length should be ≤ 2000")
				return
			}
			p.Props[name] = content
		default:
			notionErr(w, 400, "validation_error", name+" is not a property that exists.")
			return
		}
	}
	if p.Title == "" {
		notionErr(w, 400, "validation_error", "title is required")
		return
	}
	texts, ok := checkBlocks(w, in.Children)
	if !ok {
		return
	}
	p.Blocks = texts
	n.nmu.Lock()
	n.seq++
	p.ID = fmt.Sprintf("0b6f1a20-0000-4000-8000-%012d", n.seq)
	p.URL = "https://www.notion.so/" + strings.ReplaceAll(p.ID, "-", "")
	n.pages[p.ID] = p
	n.nmu.Unlock()
	writeJSON(w, 200, n.render(p))
}

func (n *Notion) render(p *notionPage) map[string]any {
	return map[string]any{"object": "page", "id": p.ID, "url": p.URL, "properties": map[string]any{
		n.Title: map[string]any{"type": "title", "title": []map[string]string{{"plain_text": p.Title}}},
	}}
}

func (n *Notion) appendChildren(w http.ResponseWriter, c Call, id string) {
	var in struct {
		Children []json.RawMessage `json:"children"`
	}
	if json.Unmarshal(c.Body, &in) != nil || len(in.Children) == 0 {
		notionErr(w, 400, "validation_error", "body.children should be defined")
		return
	}
	n.nmu.Lock()
	p, ok := n.pages[id]
	n.nmu.Unlock()
	if !ok {
		notionErr(w, 404, "object_not_found", "Could not find block with ID: "+id)
		return
	}
	texts, ok := checkBlocks(w, in.Children)
	if !ok {
		return
	}
	n.nmu.Lock()
	p.Blocks = append(p.Blocks, texts...)
	n.nmu.Unlock()
	writeJSON(w, 200, map[string]any{"object": "list", "results": []any{}})
}

func (n *Notion) search(w http.ResponseWriter, c Call) {
	var in struct {
		Query    string            `json:"query"`
		PageSize int               `json:"page_size"`
		Filter   map[string]string `json:"filter"`
	}
	if json.Unmarshal(c.Body, &in) != nil || in.PageSize < 1 || in.PageSize > 100 {
		notionErr(w, 400, "validation_error", "body.page_size should be between 1 and 100")
		return
	}
	n.nmu.Lock()
	defer n.nmu.Unlock()
	var results []map[string]any
	for _, p := range n.pages {
		if strings.Contains(strings.ToLower(p.Title), strings.ToLower(in.Query)) && len(results) < in.PageSize {
			results = append(results, n.render(p))
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "results": results, "has_more": false})
}

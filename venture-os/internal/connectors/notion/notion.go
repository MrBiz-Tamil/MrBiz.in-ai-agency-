// Package notion implements collab.Notes on the Notion API (version 2022-06-28) with an internal integration token.
//
// The business creates an internal integration in Notion, shares one database with it, and gives us the token and the
// database id. Pages are created as rows of that database; Search is limited by Notion itself to pages the integration
// has been shared with, so the adapter can only ever see what the business chose to share.
//
// Notion limits that shape the code: a rich-text block holds at most 2,000 characters and one request at most 100
// blocks, so long bodies are split into paragraphs and chunked. There is no idempotency key on page creation, so a
// create is not retried after a server error (a retry could produce a duplicate page); rate limits (429) are retried
// because Notion did not process those. Extra Properties must be TEXT columns of the database.
package notion

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/httpx"
)

const (
	defaultAPI     = "https://api.notion.com/v1"
	apiVersion     = "2022-06-28"
	maxRichText    = 2000
	maxBlocksPerOp = 100
)

// Config holds one business's Notion connection.
type Config struct {
	Token         string
	DatabaseID    string
	TitleProperty string // the database's title column; default "Name"

	APIBase string // override, for tests
	Tune    func(*httpx.Config)
}

// Client is a connected Notion workspace. Safe for concurrent use.
type Client struct {
	cfg Config
	api *httpx.Client
}

var _ collab.Notes = (*Client)(nil)

var idRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

// New builds a client; it does not call Notion.
func New(cfg Config) (*Client, error) {
	if cfg.Token == "" || !idRe.MatchString(cfg.DatabaseID) {
		return nil, fmt.Errorf("%w: notion needs an integration token and a database id", collab.ErrInvalid)
	}
	if cfg.TitleProperty == "" {
		cfg.TitleProperty = "Name"
	}
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPI
	}
	hc := httpx.Config{Provider: "notion", BaseURL: cfg.APIBase, RatePerSecond: 3, Burst: 3} // Notion allows ~3 requests/second
	if cfg.Tune != nil {
		cfg.Tune(&hc)
	}
	api, err := httpx.New(hc)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, api: api}, nil
}

func (c *Client) call(ctx context.Context, r httpx.Request) (*httpx.Response, error) {
	r.Header = http.Header{"Authorization": {"Bearer " + c.cfg.Token}, "Notion-Version": {apiVersion}}
	resp, err := c.api.Do(ctx, r)
	return resp, mapErr(err)
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *httpx.APIError
	switch {
	case errors.Is(err, httpx.ErrCircuitOpen), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", collab.ErrUnavailable, err)
	case !errors.As(err, &ae):
		return fmt.Errorf("%w: %v", collab.ErrUnavailable, err)
	}
	switch {
	case ae.Status == http.StatusUnauthorized:
		return fmt.Errorf("%w: the Notion integration token was rejected; reconnect Notion: %w", collab.ErrAuth, ae)
	case ae.Status == http.StatusForbidden:
		return fmt.Errorf("%w: the Notion integration has no access to that: %w", collab.ErrAuth, ae)
	case ae.Status == http.StatusNotFound:
		return fmt.Errorf("%w: not found; make sure the page or database is shared with the integration: %w", collab.ErrNotFound, ae)
	case ae.Status == http.StatusConflict || ae.Status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", collab.ErrUnavailable, ae) // conflict_error is transient in Notion
	case ae.Status >= 400 && ae.Status < 500:
		return fmt.Errorf("%w: %w", collab.ErrInvalid, ae)
	}
	return fmt.Errorf("%w: %w", collab.ErrUnavailable, ae)
}

// Check verifies the token and that the database is shared with the integration and has the expected title column.
func (c *Client) Check(ctx context.Context) error {
	if _, err := c.call(ctx, httpx.Request{Path: "/users/me"}); err != nil {
		return err
	}
	resp, err := c.call(ctx, httpx.Request{Path: "/databases/" + c.cfg.DatabaseID})
	if err != nil {
		return err
	}
	var db struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := resp.JSON(&db); err != nil {
		return fmt.Errorf("%w: unreadable database", collab.ErrUnavailable)
	}
	if p, ok := db.Properties[c.cfg.TitleProperty]; !ok || p.Type != "title" {
		return fmt.Errorf("%w: the database has no title column named %q", collab.ErrInvalid, c.cfg.TitleProperty)
	}
	return nil
}

type richText struct {
	Type string `json:"type"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
}

func rt(s string) []richText {
	var t richText
	t.Type, t.Text.Content = "text", s
	return []richText{t}
}

func paragraph(s string) map[string]any {
	return map[string]any{"object": "block", "type": "paragraph", "paragraph": map[string]any{"rich_text": rt(s)}}
}

func blocks(text string) []map[string]any {
	var out []map[string]any
	for _, p := range collab.Paragraphs(text, maxRichText) {
		out = append(out, paragraph(p))
	}
	return out
}

type page struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	Properties map[string]struct {
		Type  string `json:"type"`
		Title []struct {
			PlainText string `json:"plain_text"`
		} `json:"title"`
	} `json:"properties"`
}

func (p page) note() collab.Note {
	n := collab.Note{ID: p.ID, URL: p.URL}
	for _, pr := range p.Properties {
		if pr.Type == "title" {
			for _, t := range pr.Title {
				n.Title += t.PlainText
			}
		}
	}
	return n
}

// CreatePage adds a row to the database with the body as paragraphs.
func (c *Client) CreatePage(ctx context.Context, in collab.NoteInput) (collab.Note, error) {
	in, err := collab.NormalizeNote(in)
	if err != nil {
		return collab.Note{}, err
	}
	props := map[string]any{c.cfg.TitleProperty: map[string]any{"title": rt(in.Title)}}
	for k, v := range in.Properties {
		if k == c.cfg.TitleProperty {
			return collab.Note{}, fmt.Errorf("%w: %q is the title; pass it as the title", collab.ErrInvalid, k)
		}
		props[k] = map[string]any{"rich_text": rt(v)}
	}
	all := blocks(in.Body)
	first := all
	if len(first) > maxBlocksPerOp {
		first = all[:maxBlocksPerOp]
	}
	body := map[string]any{"parent": map[string]string{"database_id": c.cfg.DatabaseID}, "properties": props}
	if len(first) > 0 {
		body["children"] = first
	}
	resp, err := c.call(ctx, httpx.Request{Method: http.MethodPost, Path: "/pages", JSON: body})
	if err != nil {
		return collab.Note{}, err
	}
	var p page
	if err := resp.JSON(&p); err != nil || p.ID == "" {
		return collab.Note{}, fmt.Errorf("%w: Notion returned an unreadable page", collab.ErrUnavailable)
	}
	for rest := all[len(first):]; len(rest) > 0; {
		n := min(len(rest), maxBlocksPerOp)
		if err := c.appendBlocks(ctx, p.ID, rest[:n]); err != nil {
			return p.note(), fmt.Errorf("the page %s was created but only part of its text was added: %w", p.ID, err)
		}
		rest = rest[n:]
	}
	return p.note(), nil
}

func (c *Client) appendBlocks(ctx context.Context, pageID string, bs []map[string]any) error {
	_, err := c.call(ctx, httpx.Request{Method: http.MethodPatch, Path: "/blocks/" + pageID + "/children", JSON: map[string]any{"children": bs}})
	return err
}

// Append adds text to the end of a page.
func (c *Client) Append(ctx context.Context, pageID, text string) error {
	if !idRe.MatchString(pageID) {
		return fmt.Errorf("%w: %q is not a Notion page id", collab.ErrInvalid, pageID)
	}
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > collab.MaxBodyLen {
		return fmt.Errorf("%w: text must be 1 to %d characters", collab.ErrInvalid, collab.MaxBodyLen)
	}
	all := blocks(text)
	for len(all) > 0 {
		n := min(len(all), maxBlocksPerOp)
		if err := c.appendBlocks(ctx, pageID, all[:n]); err != nil {
			return err
		}
		all = all[n:]
	}
	return nil
}

// Search finds pages by title among those shared with the integration.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]collab.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > collab.MaxSearchLimit {
		return nil, fmt.Errorf("%w: limit must be at most %d", collab.ErrInvalid, collab.MaxSearchLimit)
	}
	if len([]rune(query)) > 200 {
		return nil, fmt.Errorf("%w: query is too long", collab.ErrInvalid)
	}
	resp, err := c.call(ctx, httpx.Request{Method: http.MethodPost, Path: "/search", Idempotent: true, JSON: map[string]any{
		"query": strings.TrimSpace(query), "page_size": limit, "filter": map[string]string{"property": "object", "value": "page"},
	}})
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []page `json:"results"`
	}
	if err := resp.JSON(&out); err != nil {
		return nil, fmt.Errorf("%w: unreadable search result", collab.ErrUnavailable)
	}
	notes := make([]collab.Note, 0, len(out.Results))
	for _, p := range out.Results {
		notes = append(notes, p.note())
	}
	return notes, nil
}

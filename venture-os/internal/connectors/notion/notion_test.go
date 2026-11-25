package notion_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/connectors/fakeapi"
	"github.com/mrbiz/venture-os/internal/connectors/notion"
	"github.com/mrbiz/venture-os/internal/httpx"
)

var ctx = context.Background()

const dbID = "a1b2c3d4-0000-4000-8000-000000000001"

func setup(t *testing.T) (*notion.Client, *fakeapi.Notion) {
	t.Helper()
	f := fakeapi.NewNotion("secret_tok", dbID, "Name", "Status", "Deal")
	t.Cleanup(f.Close)
	c, err := notion.New(notion.Config{Token: "secret_tok", DatabaseID: dbID, APIBase: f.URL() + "/v1",
		Tune: func(h *httpx.Config) {
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			h.Rand = func() float64 { return 1 }
		}})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func TestCreatePageWritesTitlePropertiesAndParagraphs(t *testing.T) {
	c, f := setup(t)
	n, err := c.CreatePage(ctx, collab.NoteInput{Title: " Call with Berlin ", Body: "First point.\n\nSecond point.", Properties: map[string]string{"Status": "Open", "Deal": "Pepper"}})
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "Call with Berlin" || !strings.HasPrefix(n.URL, "https://www.notion.so/") || n.ID == "" {
		t.Fatalf("note = %+v", n)
	}
	p := f.Page(n.ID)
	if p.Title != "Call with Berlin" || p.Props["Status"] != "Open" || p.Props["Deal"] != "Pepper" || strings.Join(p.Blocks, "|") != "First point.|Second point." {
		t.Fatalf("stored = %+v", p)
	}
	call := f.Calls()[0]
	if call.Header.Get("Notion-Version") != "2022-06-28" || call.Header.Get("Authorization") != "Bearer secret_tok" {
		t.Fatalf("headers = %v", call.Header)
	}
}

func TestLongBodiesAreSplitToNotionsLimits(t *testing.T) {
	c, f := setup(t)
	long := strings.Repeat("word ", 3000)   // 15,000 characters in one paragraph: needs many 2,000-char blocks
	many := strings.Repeat("para\n\n", 250) // 250 paragraphs: needs more than one 100-block request
	n, err := c.CreatePage(ctx, collab.NoteInput{Title: "Big", Body: long + "\n\n" + many})
	if err != nil {
		t.Fatalf("a long body must be split, not rejected: %v", err)
	}
	p := f.Page(n.ID)
	if len(p.Blocks) < 250 {
		t.Fatalf("only %d blocks stored", len(p.Blocks))
	}
	for _, b := range p.Blocks {
		if len([]rune(b)) > 2000 {
			t.Fatal("a block exceeded 2,000 characters")
		}
	}
	patches := 0
	for _, call := range f.Calls() {
		if call.Method == "PATCH" {
			patches++
		}
	}
	if patches < 2 {
		t.Fatalf("%d append requests; more than 100 blocks must be sent in chunks", patches)
	}
	hindi := strings.Repeat("मसाला ", 2000) // 2,000-character limit is in characters, not bytes
	if _, err := c.CreatePage(ctx, collab.NoteInput{Title: "Hindi", Body: hindi}); err != nil {
		t.Fatalf("non-English text: %v", err)
	}
}

func TestAppendAndSearch(t *testing.T) {
	c, f := setup(t)
	a, _ := c.CreatePage(ctx, collab.NoteInput{Title: "Pepper supplier notes"})
	b, _ := c.CreatePage(ctx, collab.NoteInput{Title: "Cardamom pricing"})
	if err := c.Append(ctx, a.ID, "Called the co-op.\n\nThey will quote Friday."); err != nil {
		t.Fatal(err)
	}
	if got := f.Page(a.ID).Blocks; len(got) != 2 || got[1] != "They will quote Friday." {
		t.Fatalf("blocks = %v", got)
	}
	res, err := c.Search(ctx, "pepper", 5)
	if err != nil || len(res) != 1 || res[0].ID != a.ID || res[0].Title != "Pepper supplier notes" {
		t.Fatalf("search = %+v %v", res, err)
	}
	if all, _ := c.Search(ctx, "", 5); len(all) != 2 {
		t.Fatalf("an empty query lists pages: %d", len(all))
	}
	_ = b
	if err := c.Append(ctx, "00000000-0000-4000-8000-00000000dead", "x"); !errors.Is(err, collab.ErrNotFound) || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("unknown page: %v", err)
	}
}

func TestErrorsAreMappedAndInputNeverReachesNotionWhenInvalid(t *testing.T) {
	c, f := setup(t)
	bad, _ := notion.New(notion.Config{Token: "wrong", DatabaseID: dbID, APIBase: f.URL() + "/v1"})
	if _, err := bad.CreatePage(ctx, collab.NoteInput{Title: "x"}); !errors.Is(err, collab.ErrAuth) || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("bad token: %v", err)
	}
	wrongDB, _ := notion.New(notion.Config{Token: "secret_tok", DatabaseID: "ffffffff-0000-4000-8000-000000000001", APIBase: f.URL() + "/v1"})
	if _, err := wrongDB.CreatePage(ctx, collab.NoteInput{Title: "x"}); !errors.Is(err, collab.ErrNotFound) || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("unshared database: %v", err)
	}
	before := len(f.Calls())
	for name, in := range map[string]collab.NoteInput{
		"no title":      {},
		"unknown prop":  {Title: "t", Properties: map[string]string{"Nope": "v"}},
		"title as prop": {Title: "t", Properties: map[string]string{"Name": "v"}},
	} {
		if _, err := c.CreatePage(ctx, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if n := len(f.Calls()) - before; n != 1 { // only the unknown-property one is Notion's to refuse
		t.Fatalf("%d calls reached Notion for locally invalid input", n)
	}
	if err := c.Append(ctx, "not-a-uuid", "x"); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("bad page id: %v", err)
	}
	if err := c.Append(ctx, dbID, "   "); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("empty text: %v", err)
	}
	if _, err := c.Search(ctx, "x", 21); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("limit: %v", err)
	}
	if _, err := notion.New(notion.Config{Token: "", DatabaseID: dbID}); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := notion.New(notion.Config{Token: "t", DatabaseID: "nope"}); !errors.Is(err, collab.ErrInvalid) {
		t.Fatalf("bad database id: %v", err)
	}
}

func TestRateLimitsAreRetriedButACreateIsNeverRepeatedAfterAServerError(t *testing.T) {
	c, f := setup(t)
	f.FailNext(429) // Notion did not process a 429, so repeating is safe
	if _, err := c.CreatePage(ctx, collab.NoteInput{Title: "after the limit"}); err != nil {
		t.Fatalf("a 429 must be retried: %v", err)
	}
	if len(f.Pages()) != 1 {
		t.Fatalf("%d pages", len(f.Pages()))
	}
	f.FailNext(503)
	if _, err := c.CreatePage(ctx, collab.NoteInput{Title: "lost response"}); !errors.Is(err, collab.ErrUnavailable) {
		t.Fatalf("a 5xx on create must surface: %v", err)
	}
	if len(f.Pages()) != 1 {
		t.Fatal("a create after a 5xx was repeated; that could duplicate a page")
	}
	f.FailNext(409)
	if _, err := c.Search(ctx, "x", 3); !errors.Is(err, collab.ErrUnavailable) {
		t.Fatalf("conflict_error is transient: %v", err)
	}
}

func TestSendsOnlyTheDocumentedShape(t *testing.T) {
	c, f := setup(t)
	_, _ = c.CreatePage(ctx, collab.NoteInput{Title: "Shape", Body: "x"})
	var body map[string]any
	_ = json.Unmarshal(f.Calls()[0].Body, &body)
	if body["parent"].(map[string]any)["database_id"] != dbID {
		t.Fatalf("parent = %v", body["parent"])
	}
	if _, has := body["children"]; !has {
		t.Fatal("children missing")
	}
}

func TestCheckVerifiesTokenDatabaseAndTitleColumn(t *testing.T) {
	c, f := setup(t)
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}
	wrongTitle, _ := notion.New(notion.Config{Token: "secret_tok", DatabaseID: dbID, TitleProperty: "Title", APIBase: f.URL() + "/v1"})
	if err := wrongTitle.Check(ctx); !errors.Is(err, collab.ErrInvalid) || !strings.Contains(err.Error(), `"Title"`) {
		t.Fatalf("a database without that title column: %v", err)
	}
	badTok, _ := notion.New(notion.Config{Token: "nope", DatabaseID: dbID, APIBase: f.URL() + "/v1"})
	if err := badTok.Check(ctx); !errors.Is(err, collab.ErrAuth) {
		t.Fatalf("bad token: %v", err)
	}
	unshared, _ := notion.New(notion.Config{Token: "secret_tok", DatabaseID: "ffffffff-0000-4000-8000-000000000001", APIBase: f.URL() + "/v1"})
	if err := unshared.Check(ctx); !errors.Is(err, collab.ErrNotFound) {
		t.Fatalf("a database that is not shared: %v", err)
	}
}

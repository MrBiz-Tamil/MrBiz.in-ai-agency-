package notion_test

// notes_append takes page_id from an AGENT. The adapter puts it into "/blocks/<id>/children", so it must never be able to change
// the path. Append checks it against a strict UUID pattern first; this test pins that down against every shape of path trick,
// using a server that records the raw request line (the fake normalises paths, which would hide a traversal).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/connectors/notion"
	"github.com/mrbiz/venture-os/internal/httpx"
)

func TestPageIDCannotChangeTheRequestPath(t *testing.T) {
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.Method+" "+r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := notion.New(notion.Config{Token: "t", DatabaseID: dbID, APIBase: srv.URL + "/v1",
		Tune: func(h *httpx.Config) {
			h.Sleep = func(context.Context, time.Duration) error { return nil }
			h.Rand = func() float64 { return 1 }
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../../users/me", "x/../../pages/p", "..", "a/b", "a?b=c", "a#frag", "id with space", "a%2f..%2fb", ""} {
		if err := c.Append(ctx, id, "text"); !errors.Is(err, collab.ErrInvalid) {
			t.Errorf("page_id %q: want ErrInvalid, got %v", id, err)
		}
	}
	if len(uris) != 0 {
		t.Fatalf("a refused id must not reach the provider: %v", uris)
	}
	// A real page id (with or without dashes) is sent, as one segment.
	for _, id := range []string{dbID, "a1b2c3d400004000800000000000000f"} {
		if err := c.Append(ctx, id, "text"); err != nil {
			t.Fatalf("page %q: %v", id, err)
		}
	}
	ok := regexp.MustCompile(`^PATCH /v1/blocks/[0-9a-fA-F-]+/children$`)
	if len(uris) != 2 || !ok.MatchString(uris[0]) || !ok.MatchString(uris[1]) {
		t.Fatalf("requests = %v", uris)
	}
}

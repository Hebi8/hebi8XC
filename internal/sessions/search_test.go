package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSearchCopies404AndTimeout(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "reason before change", "edited parser", "after")
	m := &desk.src.messages["codex:example"][1]
	m.Kind = "text"
	m.Calls = []Call{{Call: "edit-1", Tool: "edit", Input: "needle", Files: []File{{Path: "/home/alice/project/sessions/store.go", Op: "change"}}}}
	for i := range desk.src.messages["codex:example"] {
		desk.src.messages["codex:example"][i].Kind = "text"
	}
	net.sync()
	if _, _, e := laptop.readAll("desk", "codex:example", 50); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	laptop.Fetch = func(ctx context.Context, p Peer, path string, out any) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
		resp, e := server.Client().Do(req)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatal(resp.StatusCode)
		}
		return errors.New("HTTP 404")
	}
	q := SearchQuery{File: "sessions/store.go", Node: "desk", Since: 500, BySession: true, Limit: 50}
	r, e := laptop.Search(ctx, q)
	if e != nil || len(r.Sessions) != 1 || r.Sessions[0].Hits != 1 || r.Nodes[0].Searched != "copies" {
		t.Fatal(r, e)
	}
	if r.Sessions[0].Ref != "desk/codex:example" || r.Sessions[0].Rev == 0 || r.Sessions[0].First == 0 {
		t.Fatal("missing anchor", r)
	}
	// Inject an expired peer deadline. No wall time or sleep is needed.
	laptop.PeerSearchContext = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d != 5*time.Second {
			t.Fatal(d)
		}
		return context.WithDeadline(ctx, time.Unix(0, 0))
	}
	laptop.Fetch = func(ctx context.Context, p Peer, path string, out any) error { <-ctx.Done(); return ctx.Err() }
	r, e = laptop.Search(ctx, q)
	if e != nil || len(r.Sessions) != 1 || r.Nodes[0].Searched != "copies" {
		t.Fatal(r, e)
	}
	desk.online = false
	reading, e := laptop.Around(ctx, "desk", "codex:example", 2, 1, 1, new(int64))
	if e != nil || len(reading.Messages) != 3 || !reading.Moved || reading.Rev == 0 {
		t.Fatal(reading, e)
	}
}
func TestSearchCopyFormatsAndMaintenance(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:plain", "Plain", 1000, "quotes AND NEAR -- 中文 js")
	net.sync()
	if _, _, e := laptop.readAll("desk", "codex:plain", 50); e != nil {
		t.Fatal(e)
	}
	desk.online = false
	for _, term := range []string{"AND", "中文", "js"} {
		r, e := laptop.Search(ctx, SearchQuery{Terms: []string{term}, Node: "desk", Limit: 50})
		if e != nil || len(r.Hits) != 1 || !r.Nodes[0].Partial {
			t.Fatal(r, e)
		}
	}
	r, e := laptop.Search(ctx, SearchQuery{Kind: "text", Node: "desk", Limit: 50})
	if e != nil || len(r.Hits) != 0 || !r.Nodes[0].Partial {
		t.Fatal(r, e)
	}
	// An old writer replacing or deleting content must update the new indexes.
	if _, e := laptop.Store.db.Exec(`UPDATE messages SET text='replacement' WHERE key='codex:plain'`); e != nil {
		t.Fatal(e)
	}
	r, e = laptop.Search(ctx, SearchQuery{Terms: []string{"replacement"}, Node: "desk", Limit: 50})
	if e != nil || len(r.Hits) != 1 {
		t.Fatal(r, e)
	}
	if _, e := laptop.Store.db.Exec(`DELETE FROM messages WHERE key='codex:plain'`); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := laptop.Store.db.QueryRow(`SELECT count(*) FROM search_fts WHERE search_fts MATCH 'replacement'`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestSearchBackfillAndSchema2Rollback(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:plain", "Plain", 1000, "backfill needle")
	net.sync()
	laptop.readAll("desk", "codex:plain", 50)
	for _, sql := range []string{"DROP TRIGGER search_messages_insert", "DROP TRIGGER search_messages_replace", "DROP TRIGGER search_messages_update", "DROP TRIGGER search_messages_delete", "DROP TABLE search_fts", "DROP TABLE search_docs", "DROP TABLE search_files"} {
		if _, e := laptop.Store.db.Exec(sql); e != nil {
			t.Fatal(e)
		}
	}
	if e := laptop.Store.migrateSearch(); e != nil {
		t.Fatal(e)
	}
	// These migration functions are unchanged from Phase 2 and reject any
	// unrecognized meta.schema. They must still open the additive migration.
	if e := laptop.Store.migrateStructure(); e != nil {
		t.Fatal(e)
	}
	if e := laptop.Store.migrateExport(); e != nil {
		t.Fatal(e)
	}
	r, held, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{Terms: []string{"needle"}, Limit: 50})
	if e != nil || !held || len(r.Hits) != 1 {
		t.Fatal(r, held, e)
	}
	if e := laptop.Store.migrateSearch(); e != nil {
		t.Fatal(e)
	}
	r, _, e = laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{Terms: []string{"needle"}, Limit: 50})
	if e != nil || len(r.Hits) != 1 {
		t.Fatal(r, e)
	}
}
func TestSearchAPIAndAround(t *testing.T) {
	desk, _, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "before", "needle", "after")
	net.sync()
	mux := http.NewServeMux()
	desk.ControlRoutes(mux)
	for _, path := range []string{"/v1/sessions/search?q=needle&node=desk", "/v1/sessions/messages?node=desk&key=codex:example&around=2&before=1&after=1&rev=0"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var raw map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &raw); e != nil {
			t.Fatal(e)
		}
		if strings.Contains(path, "around") && raw["moved"] != true {
			t.Fatal(raw)
		}
	}
	for _, query := range []string{"q=" + strings.Repeat("x&q=", 100), "op=bad", "since=tomorrow"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/search?"+query, nil))
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestGroupCountsBeforeLimit(t *testing.T) {
	src := newSource()
	texts := make([]string, 60)
	for i := range texts {
		texts[i] = "needle"
	}
	src.put("codex:many", "Many", 1000, texts...)
	r, e := ScanSearch(ctx, src, SearchQuery{Terms: []string{"needle"}, BySession: true, Limit: 1})
	if e != nil || len(r.Sessions) != 1 || r.Sessions[0].Hits != 60 || r.Sessions[0].First != 1 || r.Sessions[0].Last != 60 {
		t.Fatal(r, e)
	}
}

// Model a source stuck behind another reader. The HTTP boundary must still
// answer at ten seconds, even before that reader releases its lock.
type blockedSearchSource struct {
	Source
	release chan struct{}
}

func (s blockedSearchSource) Sessions(ctx context.Context) ([]Session, error) {
	<-s.release
	return s.Source.Sessions(ctx)
}
func TestSearchHTTPDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		desk, _, _ := pair(t)
		release := make(chan struct{})
		desk.Source = blockedSearchSource{desk.Source, release}
		mux := http.NewServeMux()
		desk.ReadRoutes(mux)
		w := httptest.NewRecorder()
		start := time.Now()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/search?node=desk", nil))
		close(release)
		if w.Code != http.StatusServiceUnavailable || time.Since(start) != 10*time.Second {
			t.Fatal(w.Code, time.Since(start))
		}
	})
}

func TestPeerSearchFiveSecondClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		desk, laptop, net := pair(t)
		desk.src.put("codex:example", "Example", 1000, "needle")
		net.sync()
		if _, _, e := laptop.readAll("desk", "codex:example", 50); e != nil {
			t.Fatal(e)
		}
		laptop.Fetch = func(ctx context.Context, _ Peer, _ string, _ any) error { <-ctx.Done(); return ctx.Err() }
		start := time.Now()
		r, e := laptop.Search(ctx, SearchQuery{Node: "desk", Terms: []string{"needle"}, Limit: 50})
		if e != nil || len(r.Hits) != 1 || r.Nodes[0].Searched != "copies" || time.Since(start) != 5*time.Second {
			t.Fatal(r, e, time.Since(start))
		}
	})
}

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

func TestAroundUsesReadRevisionAndHasBefore(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "before", "needle", "after")
	net.sync()
	if _, _, e := laptop.readAll("desk", "codex:example", 50); e != nil {
		t.Fatal(e)
	}
	r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{Terms: []string{"needle"}, Limit: 50})
	if e != nil || len(r.Hits) != 1 {
		t.Fatal(r, e)
	}
	anchor := r.Hits[0]
	desk.src.add("codex:example", "new text")
	net.sync()
	desk.online = false
	pg, e := laptop.Around(ctx, "desk", "codex:example", anchor.ID, 1, 1, &anchor.Rev)
	if e != nil || pg.Moved || pg.Rev != anchor.Rev || pg.CopyRev != anchor.Rev || pg.HasBefore {
		t.Fatal(pg, e)
	}
	pg, e = laptop.Around(ctx, "desk", "codex:example", anchor.ID, 0, 0, &anchor.Rev)
	if e != nil || !pg.HasBefore {
		t.Fatal(pg, e)
	}
	// The peer has advanced again without the reader pulling its newer feed.
	desk.online = true
	desk.src.add("codex:example", "latest")
	desk.SyncLocal(ctx)
	_, rev, _, _ := desk.Store.Lookup(ctx, desk.Self.ID, "codex:example")
	pg, e = laptop.Around(ctx, "desk", "codex:example", anchor.ID, 1, 1, &rev)
	if e != nil || pg.Moved || pg.Rev != rev {
		t.Fatal(pg, e)
	}
}
func TestAroundMissingAndPartial(t *testing.T) {
	desk, _, _ := pair(t)
	desk.src.put("codex:example", "Example", 1000, "first", "last")
	desk.SyncLocal(ctx)
	pg, e := desk.Around(ctx, "desk", "codex:example", 999, 0, 0, nil)
	if e != nil || len(pg.Messages) != 1 || pg.Messages[0].Text != "last" || !pg.HasBefore {
		t.Fatal(pg, e)
	}
	desk.src.put("codex:empty", "Empty", 1000)
	desk.SyncLocal(ctx)
	pg, e = desk.Around(ctx, "desk", "codex:empty", 1, 0, 0, nil)
	if e != nil || pg.Messages == nil {
		t.Fatal(pg, e)
	}
	many := make([]string, SearchCandidates+5)
	for i := range many {
		many[i] = "text"
	}
	desk.src.put("codex:many", "Many", 1000, many...)
	desk.SyncLocal(ctx)
	pg, e = desk.Around(ctx, "desk", "codex:many", 999999, 2, 1, nil)
	if e != nil || !pg.Partial || len(pg.Messages) == 0 || strings.Contains(pg.Note, "use --after") {
		t.Fatal(pg.Partial, len(pg.Messages), pg.Note, e)
	}
}
func TestShowKeyResolution(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "needle")
	net.sync()
	r, e := laptop.Read(ctx, "", "codex:example", 0, 50)
	if e != nil || r.Session.Ref != "desk/codex:example" {
		t.Fatal(r, e)
	}
	laptop.src.put("codex:example", "Other", 1000, "second")
	laptop.SyncLocal(ctx)
	_, e = laptop.Read(ctx, "", "codex:example", 0, 50)
	if e == nil || !strings.Contains(e.Error(), "desk/codex:example") || !strings.Contains(e.Error(), "laptop/codex:example") {
		t.Fatal(e)
	}
	_, e = laptop.Read(ctx, "", "codex:missing", 0, 50)
	if !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}

type cancelSearchSource struct {
	Source
	cancel context.CancelFunc
	calls  int
}

func (s *cancelSearchSource) Messages(ctx context.Context, key string, after int64, limit, budget int) ([]Message, bool, error) {
	s.calls++
	if s.calls == 1 {
		return []Message{{ID: 1, Text: "needle", Time: 1000}}, true, nil
	}
	s.cancel()
	return nil, false, ctx.Err()
}
func TestSearchKeepsHitsOnCancellation(t *testing.T) {
	src := newSource()
	src.put("codex:example", "Example", 1000, "needle", "other")
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	r, e := ScanSearch(c, &cancelSearchSource{Source: src, cancel: cancel}, SearchQuery{Terms: []string{"needle"}, Limit: 50})
	if e != nil || len(r.Hits) != 1 || !r.Partial || !strings.Contains(r.Note, "narrow") || strings.Contains(r.Note, "deadline exceeded") {
		t.Fatal(r, e)
	}
}
func TestPartialOnlyInRelevantWindow(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:old", "Old", 1000, "text")
	net.sync()
	laptop.readAll("desk", "codex:old", 50)
	for _, q := range []SearchQuery{{Terms: []string{"text"}, Limit: 50}, {File: "store.go", Since: 2000, Limit: 50}, {Kind: "text", Agent: "other", Limit: 50}} {
		r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, q)
		if e != nil || r.Partial {
			t.Fatal(r, e)
		}
	}
	r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{Kind: "text", Limit: 50})
	if e != nil || !r.Partial {
		t.Fatal(r, e)
	}
	src := newSource()
	src.put("codex:empty", "Empty", 3000)
	src.put("codex:old", "Old", 1000, "text")
	r, e = ScanSearch(ctx, src, SearchQuery{Kind: "text", Since: 2000, Limit: 50})
	if e != nil || r.Partial || src.read != 0 {
		t.Fatal(r, e, src.read)
	}
}
func TestLocalSearchFiltersBeforeLimit(t *testing.T) {
	desk, _, _ := pair(t)
	desk.src.put("codex:kept", "Kept", 1000, "needle")
	desk.SyncLocal(ctx)
	desk.src.put("codex:unpublished", "Unpublished", 2000, "needle")
	r, e := desk.LocalSearch(ctx, SearchQuery{Terms: []string{"needle"}, Limit: 1})
	if e != nil || len(r.Hits) != 1 || r.Hits[0].Key != "codex:kept" {
		t.Fatal(r, e)
	}
}
func TestEmptySearchDocsAndSchemaMigration(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "needle")
	net.sync()
	laptop.readAll("desk", "codex:example", 50)
	var empty int
	e := laptop.Store.db.QueryRow("SELECT count(*) FROM search_docs WHERE text='' OR text IS NULL").Scan(&empty)
	if e != nil || empty != 0 {
		t.Fatal(empty, e)
	}
	// Simulate the prior additive search schema with redundant rows.
	laptop.Store.db.Exec("DELETE FROM meta WHERE k='search_schema'")
	laptop.Store.db.Exec("INSERT INTO search_docs(node,key,gen,id,field,call,text) VALUES('d1','codex:example',1,1,'thinking',-1,'')")
	if e = laptop.Store.migrateSearch(); e != nil {
		t.Fatal(e)
	}
	laptop.Store.db.QueryRow("SELECT count(*) FROM search_docs WHERE text=''").Scan(&empty)
	if empty != 0 {
		t.Fatal(empty)
	}
}
func TestSearchPathsValuesAndErrors(t *testing.T) {
	q := SearchQuery{File: `././C:\project\store.go`, Errors: true}
	if e := q.Validate(); e != nil || q.File != "c:/project/store.go" || !q.Failed {
		t.Fatal(q, e)
	}
	if !FileMatch(`C:\project\store.go`, `././c:/project/store.go`) || FileMatch("/p/xstore.go", "././store.go") {
		t.Fatal("path boundary")
	}
	if len((SearchQuery{}).Values()) != 0 {
		t.Fatal((SearchQuery{}).Values())
	}
}
func TestSourcesHaveIndependentBudgets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready := newSource()
		ready.put("antigravity-acp:example", "Example", 1000, "needle")
		c := &Combined{Wake: deadlineSource{Source: newSource()}, ACP: ready}
		start := time.Now()
		r, e := c.Search(ctx, SearchQuery{Terms: []string{"needle"}, Limit: 50})
		if e != nil || !r.Partial || len(r.Hits) != 2 || time.Since(start) != 8*time.Second {
			t.Fatal(r, e, time.Since(start))
		}
	})
}

type deadlineSource struct{ Source }

func (s deadlineSource) Search(ctx context.Context, q SearchQuery) (LocalSearchResult, error) {
	<-ctx.Done()
	return LocalSearchResult{Hits: []SearchHit{{Key: "codex:retained", ID: 1}}}, ctx.Err()
}

func TestReadRouteHasNoTenSecondDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		desk, _, _ := pair(t)
		desk.src.put("codex:example", "Example", 1000, "text")
		desk.SyncLocal(ctx)
		desk.Source = slowReadSource{desk.Source}
		mux := http.NewServeMux()
		desk.ReadRoutes(mux)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/messages?node=desk&key=codex:example&around=1", nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var r Reading
		if e := json.Unmarshal(w.Body.Bytes(), &r); e != nil || len(r.Messages) != 1 {
			t.Fatal(r, e)
		}
	})
}

type slowReadSource struct{ Source }

func (s slowReadSource) Messages(ctx context.Context, key string, after int64, limit, budget int) ([]Message, bool, error) {
	<-time.After(11 * time.Second)
	return s.Source.Messages(ctx, key, after, limit, budget)
}

func TestCopySQLFiltersPrecedeCandidateLimit(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "text")
	net.sync()
	gen, e := laptop.Store.NewCopy(ctx, desk.Self.ID, "codex:example")
	if e != nil {
		t.Fatal(e)
	}
	msgs := make([]Message, 10020)
	for i := range msgs {
		msgs[i] = Message{ID: int64(i + 1), Text: "needle", Kind: "meta", Time: 1000}
	}
	msgs[len(msgs)-1].Kind = "text"
	msgs[len(msgs)-1].Calls = []Call{{Tool: "Bash", Input: "needle", Error: true, Files: []File{{Path: `C:\project\store.go`, Op: "change"}}}}
	if e = laptop.Store.AddToCopy(ctx, desk.Self.ID, "codex:example", gen, msgs); e != nil {
		t.Fatal(e)
	}
	feed, _ := laptop.Store.Feed(ctx, desk.Self.ID)
	if e = laptop.Store.UseCopy(ctx, desk.Self.ID, "codex:example", feed.Epoch, gen, 1, true, 1); e != nil {
		t.Fatal(e)
	}
	for _, group := range []bool{false, true} {
		q := SearchQuery{Terms: []string{"needle"}, Tool: "Bash", Kind: "text", Errors: true, File: "././c:/project/store.go", Agent: "codex", Project: "proj", Since: 999, Until: 1001, Limit: 50, BySession: group}
		q.Validate()
		r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, q)
		if e != nil || r.Partial {
			t.Fatal(r, e)
		}
		if group {
			if len(r.Sessions) != 1 || r.Sessions[0].Hits != 1 {
				t.Fatal(r)
			}
		} else if len(r.Hits) != 1 || r.Hits[0].Agent != "codex" || r.Hits[0].ProjectPath == "" {
			t.Fatal(r)
		}
	}
	r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{BySession: true, Limit: 50})
	if e != nil || !r.Partial || len(r.Sessions) != 1 || r.Sessions[0].Hits != 10000 {
		t.Fatal(r, e)
	}
}

func TestCopySearchDoesNotReserveStoreConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		desk, laptop, net := pair(t)
		desk.src.put("codex:example", "Example", 1000, "needle")
		net.sync()
		laptop.readAll("desk", "codex:example", 50)
		conn, e := laptop.Store.db.Conn(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close()
		r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{Terms: []string{"needle"}, Limit: 50})
		if e != nil || r.Partial || len(r.Hits) != 1 {
			t.Fatal(r, e)
		}
	})
}

func TestSearchMetadataCancellationIsPartial(t *testing.T) {
	desk, _, _ := pair(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	local, e := desk.LocalSearch(c, SearchQuery{Limit: 50})
	if e != nil || !local.Partial || !strings.Contains(local.Note, "narrow") {
		t.Fatal(local, e)
	}
	r, e := desk.Search(c, SearchQuery{Limit: 50})
	if e != nil || len(r.Nodes) != 1 || !r.Nodes[0].Partial || strings.Contains(r.Nodes[0].Note, "context canceled") {
		t.Fatal(r, e)
	}
}

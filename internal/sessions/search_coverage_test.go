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

func TestCopyFileDeclarationAndOldWriterInvalidation(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "text")
	sess := desk.src.sessions["codex:example"]
	sess.Structure = 2
	desk.src.sessions[sess.Key] = sess
	m := &desk.src.messages[sess.Key][0]
	m.Role = "assistant"
	m.Kind = "text"
	m.Structure = 2
	m.Calls = []Call{{Tool: "edit", Files: []File{{Path: "/home/alice/project/store.go", Op: "change"}}}}
	net.sync()
	if _, _, e := laptop.readAll("desk", sess.Key, 50); e != nil {
		t.Fatal(e)
	}
	c, ok, e := laptop.Store.Cached(ctx, desk.Self.ID, sess.Key)
	if e != nil || !ok || c.StructureLevel == nil || *c.StructureLevel != 2 {
		t.Fatal(c, e)
	}
	q := SearchQuery{File: "store.go", Limit: 50}
	r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, q)
	if e != nil || r.Partial || len(r.Hits) != 1 {
		t.Fatal(r, e)
	}
	// Phase 2 writes content without a declaration; even a reused generation
	// must not retain a declaration that belongs to the previous writer.
	if _, e = laptop.Store.db.Exec("UPDATE content SET fetched_at=fetched_at"); e != nil {
		t.Fatal(e)
	}
	c, _, e = laptop.Store.Cached(ctx, desk.Self.ID, sess.Key)
	if e != nil || c.StructureLevel != nil {
		t.Fatal(c, e)
	}
	for _, query := range []SearchQuery{q, {Op: "read", Limit: 50}} {
		r, _, e = laptop.Store.SearchCopies(ctx, desk.Self.ID, query)
		if e != nil || !r.Partial || !strings.Contains(r.Note, "1 matching session(s)") || !strings.Contains(r.Note, "file records") {
			t.Fatal(r, e)
		}
	}
	// A new peer refreshes an old copy's missing declaration once.
	if _, _, e = laptop.readAll("desk", sess.Key, 50); e != nil {
		t.Fatal(e)
	}
	c, _, e = laptop.Store.Cached(ctx, desk.Self.ID, sess.Key)
	if e != nil || c.StructureLevel == nil || *c.StructureLevel != 2 {
		t.Fatal(c, e)
	}
}

func TestStructureNotesCountSessionsAndIgnoreUsers(t *testing.T) {
	src := newSource()
	src.put("codex:user", "User", 3000, "question")
	src.put("codex:assistant", "Assistant", 3000, "answer", "another")
	for i := range src.messages["codex:assistant"] {
		src.messages["codex:assistant"][i].Role = "assistant"
	}
	r, e := ScanSearch(ctx, src, SearchQuery{Tool: "Bash", Limit: 50})
	if e != nil || !r.Partial || !strings.Contains(r.Note, "1 matching session(s)") {
		t.Fatal(r, e)
	}
	r, e = ScanSearch(ctx, src, SearchQuery{Kind: "text", Limit: 50})
	if e != nil || !strings.Contains(r.Note, "2 matching session(s)") {
		t.Fatal(r, e)
	}
	r, e = ScanSearch(ctx, src, SearchQuery{Tool: "Bash", Since: 4000, Limit: 50})
	if e != nil || r.Partial || r.Note != "" {
		t.Fatal(r, e)
	}
}

func TestACPUntilUsesCreatedAt(t *testing.T) {
	src := newSource()
	src.put("antigravity-acp:long", "Long", 3000, "early", "late")
	s := src.sessions["antigravity-acp:long"]
	s.CreatedAt = 1000
	src.sessions[s.Key] = s
	src.messages[s.Key][0].Time = 1500
	src.messages[s.Key][1].Time = 2500
	r, e := ScanSearch(ctx, src, SearchQuery{Until: 2000, Limit: 50})
	if e != nil || len(r.Hits) != 1 || r.Hits[0].ID != 1 {
		t.Fatal(r, e)
	}
}

func TestAroundBudgetClippingReportsEarlierMessages(t *testing.T) {
	desk, _, _ := pair(t)
	texts := make([]string, 12)
	for i := range texts {
		texts[i] = strings.Repeat("x", 190000)
	}
	desk.src.put("codex:large", "Large", 1000, texts...)
	desk.SyncLocal(ctx)
	r, e := desk.Around(ctx, "desk", "codex:large", 10, 8, 2, nil)
	if e != nil || !r.HasBefore || len(r.Messages) != 5 || r.Messages[0].ID != 8 || r.Messages[2].ID != 10 {
		t.Fatal(r.HasBefore, len(r.Messages), e)
	}
}

func TestPeerSearchReturnsPartialBeforeCallerTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		desk, _, _ := pair(t)
		desk.src.put("codex:retained", "Retained", 1000, "needle")
		desk.SyncLocal(ctx)
		desk.Source = deadlineSource{desk.Source}
		mux := http.NewServeMux()
		desk.PeerRoutes(mux)
		w := httptest.NewRecorder()
		start := time.Now()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/search?q=needle", nil))
		var r LocalSearchResult
		e := json.Unmarshal(w.Body.Bytes(), &r)
		if e != nil || w.Code != 200 || time.Since(start) != 4*time.Second || len(r.Hits) != 1 || !r.Partial || !strings.Contains(r.Note, "time budget") {
			t.Fatal(w.Code, r, e, time.Since(start))
		}
	})
}

func TestSearchAppendsFallbackAndCoverageNotes(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "text")
	m := &desk.src.messages["codex:example"][0]
	m.Role = "assistant"
	m.Kind = "text"
	m.Calls = []Call{{Tool: "edit", Files: []File{{Path: "/home/alice/store.go", Op: "change"}}}}
	net.sync()
	laptop.readAll("desk", "codex:example", 50)
	laptop.Fetch = func(context.Context, Peer, string, any) error { return errors.New("unavailable") }
	r, e := laptop.Search(ctx, SearchQuery{Node: "desk", File: "store.go", Limit: 50})
	if e != nil || len(r.Nodes) != 1 {
		t.Fatal(r, e)
	}
	for _, phrase := range []string{"peer search unavailable", "searched held copies", "file records", "1 matching session(s)"} {
		if !strings.Contains(r.Nodes[0].Note, phrase) {
			t.Fatal(r.Nodes[0])
		}
	}
}

func TestCoverageIsSeparateFromIncompleteSearch(t *testing.T) {
	r := LocalSearchResult{}
	r.SideCoverage(1700000000123)
	if r.Partial || r.ScannedSince != 1700000000123 || !strings.Contains(r.Note, "message text searched completely") || !strings.Contains(r.Note, "--since 2023-11-14T22:13:20.123Z") {
		t.Fatal(r)
	}
	timeBound := LocalSearchResult{}
	timeBound.Bound(0, true)
	countBound := LocalSearchResult{}
	countBound.Bound(0, false)
	if !timeBound.Partial || !countBound.Partial || !strings.Contains(timeBound.Note, "time budget") || !strings.Contains(countBound.Note, "candidate limit") {
		t.Fatal(timeBound, countBound)
	}
}

func TestPageDeclarationUsesCurrentRows(t *testing.T) {
	desk, _, _ := pair(t)
	desk.src.put("codex:example", "Example", 1000, "text")
	s := desk.src.sessions["codex:example"]
	s.Structure = 1
	desk.src.sessions[s.Key] = s
	desk.SyncLocal(ctx)
	m := &desk.src.messages[s.Key][0]
	m.Role = "assistant"
	m.Kind = "text"
	m.Structure = 2
	page, e := desk.LocalMessages(ctx, s.Key, 0, 50)
	if e != nil || page.StructureLevel == nil || *page.StructureLevel != 2 {
		t.Fatal(page.StructureLevel, e)
	}
	s.Structure = 2
	s.Stamp += ":format2"
	desk.src.sessions[s.Key] = s
	desk.SyncLocal(ctx)
	m.Structure = 1
	page, e = desk.LocalMessages(ctx, s.Key, 0, 50)
	if e != nil || page.StructureLevel == nil || *page.StructureLevel != 1 {
		t.Fatal(page.StructureLevel, e)
	}
}

func TestIncompleteUserPrefixStillNeedsPartial(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:prefix", "Prefix", 3000, "question")
	desk.src.messages["codex:prefix"][0].Time = 1000
	net.sync()
	laptop.readAll("desk", "codex:prefix", 50)
	if _, e := laptop.Store.db.Exec("UPDATE content SET complete=0"); e != nil {
		t.Fatal(e)
	}
	for _, q := range []SearchQuery{{Tool: "Bash", Since: 2000, Limit: 50}, {Terms: []string{"needle"}, Since: 2000, Limit: 50}} {
		r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, q)
		if e != nil || !r.Partial || !strings.Contains(r.Note, "1 matching session(s)") {
			t.Fatal(r, e)
		}
	}
}

func TestDroppedCallsNeedStructuralPartial(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:dropped", "Dropped", 1000, "text")
	m := &desk.src.messages["codex:dropped"][0]
	m.Role = "assistant"
	m.Kind = "text"
	m.Structure = 2
	m.CallsDropped = 1
	s := desk.src.sessions["codex:dropped"]
	s.Structure = 2
	desk.src.sessions[s.Key] = s
	net.sync()
	laptop.readAll("desk", s.Key, 50)
	r, _, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{Tool: "Bash", Limit: 50})
	if e != nil || !r.Partial || !strings.Contains(r.Note, "1 matching session(s)") {
		t.Fatal(r, e)
	}
}

type coveredSource struct{ Source }

func (s coveredSource) Search(context.Context, SearchQuery) (LocalSearchResult, error) {
	r := LocalSearchResult{Hits: []SearchHit{{Key: "codex:covered", ID: 1}}}
	r.SideCoverage(1700000000123)
	return r, nil
}
func TestCoverageReachesNodeResponse(t *testing.T) {
	desk, _, _ := pair(t)
	desk.src.put("codex:covered", "Covered", 1000, "text")
	desk.SyncLocal(ctx)
	desk.Source = coveredSource{desk.Source}
	r, e := desk.Search(ctx, SearchQuery{Node: "desk", Limit: 50})
	if e != nil || len(r.Nodes) != 1 || r.Nodes[0].Partial || r.Nodes[0].ScannedSince != 1700000000123 || !strings.Contains(r.Nodes[0].Note, "--since") {
		t.Fatal(r, e)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"scanned_since":1700000000123`) {
		t.Fatal(string(raw))
	}
}

package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type partlyUnreadable struct{ Source }

func (s partlyUnreadable) Messages(ctx context.Context, key string, after int64, limit, budget int) ([]Message, bool, error) {
	if key == "codex:bad" {
		return nil, false, errors.New("private-path-marker")
	}
	return s.Source.Messages(ctx, key, after, limit, budget)
}
func TestUnreadableSessionDoesNotDiscardSourceOrLeak(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	src := newSource()
	src.put("codex:bad", "Bad", 2000, "needle")
	src.put("codex:good", "Good", 1000, "needle")
	r, e := ScanSearch(ctx, partlyUnreadable{src}, SearchQuery{Terms: []string{"needle"}, Limit: 50})
	if e != nil || len(r.Hits) != 1 || r.Hits[0].Key != "codex:good" || !r.Partial || !strings.Contains(r.Note, "session unreadable in 1 matching session(s), newest ") || strings.Contains(r.Note, "private-path-marker") || !strings.Contains(logs.String(), "private-path-marker") {
		t.Fatal(r, e, logs.String())
	}
}

type failingSearch struct{ Source }

func (s failingSearch) Search(context.Context, SearchQuery) (LocalSearchResult, error) {
	return LocalSearchResult{}, errors.New("private-source-marker")
}
func TestSourceErrorsStayInLocalLogs(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	desk, _, _ := pair(t)
	desk.src.put("codex:example", "Example", 1000, "text")
	desk.SyncLocal(ctx)
	desk.Source = failingSearch{desk.Source}
	for _, peer := range []bool{false, true} {
		mux := http.NewServeMux()
		if peer {
			desk.PeerRoutes(mux)
		} else {
			desk.ReadRoutes(mux)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/search?node=desk", nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "private-source-marker") || !strings.Contains(w.Body.String(), searchSourceUnavailableNote) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	combined := Combined{Wake: desk.Source}
	r, e := combined.Search(ctx, SearchQuery{Limit: 50})
	if e != nil || strings.Contains(r.Note, "private-source-marker") || !strings.Contains(logs.String(), "private-source-marker") {
		t.Fatal(r, e)
	}
}
func TestPartialRemovesCompleteTextClaim(t *testing.T) {
	r := LocalSearchResult{}
	r.SideCoverage(1000)
	r.Missing(2, 2000, MissingReason(SearchQuery{File: "store.go"}, false), false)
	r = FinishSearch(r, SearchQuery{Limit: 50})
	if !r.Partial || strings.Contains(r.Note, "message text searched completely") || !strings.Contains(r.Note, "thinking and full tool inputs") || !strings.Contains(r.Note, "file records unavailable in 2 matching session(s), newest ") || strings.Contains(r.Note, "refresh") {
		t.Fatal(r)
	}
	r = LocalSearchResult{}
	r.Missing(1, 2000, MissingReason(SearchQuery{}, true), true)
	if !strings.Contains(r.Note, "incomplete copies") || !strings.Contains(r.Note, "refresh the copies") {
		t.Fatal(r)
	}
}
func TestBusySearchStillReportsCopies(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("codex:example", "Example", 1000, "text")
	net.sync()
	laptop.readAll("desk", "codex:example", 50)
	writer, e := sql.Open("sqlite", laptop.Store.path)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, e = writer.Exec("BEGIN EXCLUSIVE"); e != nil {
		t.Fatal(e)
	}
	defer writer.Exec("ROLLBACK")
	laptop.Store.searchBusyTimeout = time.Millisecond
	r, held, e := laptop.Store.SearchCopies(ctx, desk.Self.ID, SearchQuery{Terms: []string{"text"}, Limit: 50})
	if e != nil || !held || !r.Partial || !strings.Contains(r.Note, "waited for a writer") || strings.Contains(r.Note, "database is locked") {
		raw, _ := json.Marshal(r)
		t.Fatal(held, e, string(raw))
	}
	if _, e = writer.Exec("ROLLBACK"); e != nil {
		t.Fatal(e)
	}
	peers := laptop.Peers
	calls := 0
	laptop.Peers = func() []Peer {
		calls++
		if calls == 2 {
			if _, e := writer.Exec("BEGIN EXCLUSIVE"); e != nil {
				t.Fatal(e)
			}
		}
		return peers()
	}
	laptop.Fetch = func(context.Context, Peer, string, any) error { return errors.New("unavailable") }
	answer, e := laptop.Search(ctx, SearchQuery{Node: "desk", Terms: []string{"text"}, Limit: 50})
	if e != nil || len(answer.Nodes) != 1 || answer.Nodes[0].Searched != "copies" || !answer.Nodes[0].Partial || !strings.Contains(answer.Nodes[0].Note, "waited for a writer") {
		t.Fatal(answer, e)
	}

}

// A machine without a session index says so, as a fixed fact, without a warning.
func TestNoIndexKeepsItsNote(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	desk, _, _ := pair(t)
	desk.Source = nil
	mux := http.NewServeMux()
	desk.ReadRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/search?node=desk", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), ErrNoIndex.Error()) || strings.Contains(w.Body.String(), searchSourceUnavailableNote) || strings.Contains(logs.String(), "search source failed") {
		t.Fatal(w.Code, w.Body.String(), logs.String())
	}
}

package wake_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

func TestSearchLiteralFTSAndFiles(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	literal := `quotes "hello" AND NEAR * : ^ -- DROP TABLE messages; 中文测试 js`
	d.Put(waketest.Session{Key: "codex:example", Agent: "codex", Project: "parser", UpdatedAt: 100}, waketest.Message{Role: "assistant", Text: literal, Time: 100})
	d.Structure("codex:example", 1, sessions.Message{Thinking: "hidden rationale", Calls: []sessions.Call{
		{Call: "a", Tool: "edit", Input: "complete needle", Files: []sessions.File{{Path: "/home/alice/project/sessions/store.go", Op: "change"}}},
		{Call: "b", Tool: "read", Input: "reading", Files: []sessions.File{{Path: "/home/alice/project/store.go", Op: "read"}}},
		{Call: "c", Tool: "edit", Input: "failed", Error: true, Files: []sessions.File{{Path: "/home/alice/project/store.go", Op: "delete"}}},
	}})
	x := wake.Open(d.Path)
	t.Cleanup(x.Close)
	for _, term := range []string{`"hello"`, "AND", "NEAR", "*", ":", "^", "--", "中文", "中文测试", "js", "rationale", "needle"} {
		q := sessions.SearchQuery{Terms: []string{term}, Limit: 50}
		r, e := x.Search(context.Background(), q)
		if e != nil || len(r.Hits) != 1 {
			t.Fatalf("term %q: hits=%d err=%v", term, len(r.Hits), e)
		}
		if term == "AND" && r.Hits[0].Snippet != literal {
			t.Fatal("literal text changed")
		}
	}
	for _, tt := range []struct {
		file, op string
		failed   bool
		want     int
	}{{"sessions/store.go", "", false, 1}, {"store.go", "", false, 1}, {"xstore.go", "any", true, 0}, {"store.go", "read", false, 1}, {"store.go", "any", false, 2}, {"store.go", "any", true, 3}, {"store.go", "", true, 2}, {"/else/store.go", "any", true, 0}} {
		r, e := x.Search(context.Background(), sessions.SearchQuery{File: tt.file, Op: tt.op, Failed: tt.failed, Limit: 50})
		if e != nil || len(r.Hits) != tt.want || r.Partial {
			t.Fatalf("%+v: %+v %v", tt, r, e)
		}
	}
	for _, tt := range []struct {
		q    sessions.SearchQuery
		want int
	}{
		{sessions.SearchQuery{Agent: "other"}, 0}, {sessions.SearchQuery{Project: "missing"}, 0},
		{sessions.SearchQuery{Since: 101}, 0}, {sessions.SearchQuery{Until: 99}, 0},
		{sessions.SearchQuery{Kind: "meta"}, 0}, {sessions.SearchQuery{Tool: "read"}, 1},
		{sessions.SearchQuery{File: "store.go", Errors: true, Failed: true}, 1},
	} {
		tt.q.Limit = 50
		r, e := x.Search(context.Background(), tt.q)
		if e != nil || len(r.Hits) != tt.want {
			t.Fatalf("filters %+v: %+v %v", tt, r, e)
		}
	}
	// FTS must stay in step through append, replacement and deletion.
	d.Append("codex:example", waketest.Message{Text: "appendix 中文"})
	r, e := x.Search(context.Background(), sessions.SearchQuery{Terms: []string{"appendix"}, Limit: 50})
	if e != nil || len(r.Hits) != 1 {
		t.Fatal(r, e)
	}
	d.Put(waketest.Session{Key: "codex:example"}, waketest.Message{Text: "replacement"})
	r, e = x.Search(context.Background(), sessions.SearchQuery{Terms: []string{"appendix"}, Limit: 50})
	if e != nil || len(r.Hits) != 0 {
		t.Fatal(r, e)
	}
	d.Delete("codex:example")
	r, e = x.Search(context.Background(), sessions.SearchQuery{Terms: []string{"中文"}, Limit: 50})
	if e != nil || len(r.Hits) != 0 {
		t.Fatal(r, e)
	}
}
func TestSearchFormatAndBounds(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	d.Put(waketest.Session{Key: "codex:old"}, waketest.Message{Text: "hello world"})
	d.StructureV1("codex:old", 1, sessions.Message{})
	x := wake.Open(d.Path)
	defer x.Close()
	r, e := x.Search(context.Background(), sessions.SearchQuery{File: "store.go", Limit: 50})
	if e != nil || !r.Partial || len(r.Hits) != 0 {
		t.Fatal(r, e)
	}
	d.Exec(`DROP TABLE messages_fts`)
	_, e = x.Search(context.Background(), sessions.SearchQuery{Terms: []string{"hello"}, Limit: 50})
	if e == nil || !strings.Contains(e.Error(), "trigram") {
		t.Fatal(e)
	}
	q := sessions.SearchQuery{Terms: strings.Fields(strings.Repeat("term ", 100))}
	if q.Validate() == nil {
		t.Fatal("100 terms allowed")
	}
}

func TestSearchCJKBackfillAndBoundedShortScan(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	d.Put(waketest.Session{Key: "codex:example", Agent: "codex", UpdatedAt: 1000}, waketest.Message{Text: "测试 js"})
	d.Exec("INSERT INTO schema_meta VALUES('cjk_backfill','1')")
	d.Exec("DELETE FROM messages_cjk")
	x := wake.Open(d.Path)
	defer x.Close()
	r, e := x.Search(context.Background(), sessions.SearchQuery{Terms: []string{"测试"}, Limit: 50})
	if e != nil || len(r.Hits) != 1 {
		t.Fatal(r, e)
	}
	d.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<2050) INSERT INTO messages(session_key,seq,role,ts,text) SELECT 'codex:example',x+1,'user',1000+x,'js' FROM n`)
	r, e = x.Search(context.Background(), sessions.SearchQuery{Terms: []string{"js"}, Limit: 50})
	if e != nil || len(r.Hits) != 50 || !r.Partial || !strings.Contains(r.Note, "through") {
		t.Fatal(len(r.Hits), r.Partial, r.Note, e)
	}
	r, e = x.Search(context.Background(), sessions.SearchQuery{File: "store.go", Since: 10000, Limit: 50})
	if e != nil || r.Partial {
		t.Fatal(r, e)
	}
}

func TestSQLFileFiltersAndErrors(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	d.Put(waketest.Session{Key: "codex:example", Agent: "codex", UpdatedAt: 1000}, waketest.Message{Text: "test", Time: 1000})
	d.Structure("codex:example", 1, sessions.Message{Calls: []sessions.Call{{Tool: "Bash", Error: true, Files: []sessions.File{{Path: `C:\project\store.go`, Op: "change"}}}, {Tool: "read", Files: []sessions.File{{Path: "/project/xstore.go", Op: "read"}}}}})
	x := wake.Open(d.Path)
	defer x.Close()
	for _, q := range []sessions.SearchQuery{{File: "store.go", Errors: true, Tool: "Bash", Kind: "text", Agent: "codex", Since: 999, Until: 1001, Limit: 50}, {File: `././c:\project\store.go`, Failed: true, Limit: 50}} {
		if e := q.Validate(); e != nil {
			t.Fatal(e)
		}
		r, e := x.Search(context.Background(), q)
		if e != nil || len(r.Hits) != 1 {
			t.Fatal(r, e)
		}
	}
	r, e := x.Search(context.Background(), sessions.SearchQuery{File: "store.go", Op: "read", Limit: 50})
	if e != nil || len(r.Hits) != 0 {
		t.Fatal(r, e)
	}
}

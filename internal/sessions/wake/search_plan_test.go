package wake

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

func TestPublishedAllowListKeepsFTSDrivingPlan(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	d.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<600)
 INSERT INTO sessions(key,agent_id,native_id,file_path,updated_at) SELECT 'codex:'||x,'codex','example-'||x,'/sessions/example-'||x,1000 FROM n`)
	d.Exec(`INSERT INTO messages(session_key,seq,role,ts,text) SELECT key,1,'assistant',1000,'error parser' FROM sessions`)
	d.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<175)
 INSERT INTO sessions(key,agent_id,native_id,file_path,updated_at) SELECT 'antigravity:'||x,'antigravity','acp-'||x,'/sessions/acp-'||x,1000 FROM n`)
	d.Exec(waketest.StructureSchema)
	d.Exec(`INSERT INTO structure_sessions SELECT key,file_mtime,file_size,(SELECT value FROM schema_meta WHERE key='fts_format'),(SELECT count(*) FROM messages m WHERE m.session_key=s.key),'2' FROM sessions s`)
	q := sessions.SearchQuery{Terms: []string{"error"}, Limit: 50, AllowedKeys: map[string]int64{}}
	for i := 1; i <= 600; i++ {
		q.AllowedKeys[fmt.Sprintf("codex:%d", i)] = 1
	}
	for i := 1; i <= 400; i++ {
		q.AllowedKeys[fmt.Sprintf("antigravity-acp:%d", i)] = 1
	}
	cte, args := searchScope(q, "2")
	if len(args) != 1 || !strings.Contains(cte, "json_each(?)") || strings.Contains(cte, "<>") {
		t.Fatal(cte, len(args))
	}
	query := bodyCandidateQuery(cte, searchMessageJoin, []string{"m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)"}, true)
	args = append(args, sessions.FTSPhrase(q.Terms), 10001)
	x := Open(d.Path)
	defer x.Close()
	db, e := x.open(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	rows, e := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	driven := false
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if e = rows.Scan(&id, &parent, &unused, &detail); e != nil {
			t.Fatal(e)
		}
		plan = append(plan, detail)
		if parent == 0 && strings.HasPrefix(detail, "SCAN s") {
			t.Fatal("scope became the outer loop", plan)
		}
		if parent == 0 && strings.Contains(detail, "SEARCH m USING INTEGER PRIMARY KEY") {
			driven = true
		}
	}
	if e = rows.Err(); e != nil || !driven {
		t.Fatal(plan, e)
	}
	r, e := x.Search(context.Background(), q)
	if e != nil || len(r.Hits) != 50 {
		t.Fatal(len(r.Hits), r.Note, e)
	}
}

func TestAllowListAboveSQLiteVariableLimit(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	d.Put(waketest.Session{Key: "codex:kept"}, waketest.Message{Text: "needle"})
	q := sessions.SearchQuery{Terms: []string{"needle"}, Limit: 50, AllowedKeys: map[string]int64{"codex:kept": 1}}
	for i := 0; i < 40000; i++ {
		q.AllowedKeys[fmt.Sprintf("antigravity-acp:%d", i)] = 1
	}
	x := Open(d.Path)
	defer x.Close()
	r, e := x.Search(context.Background(), q)
	if e != nil || len(r.Hits) != 1 {
		t.Fatal(len(r.Hits), r.Note, e)
	}
}

func TestSideCoverageDoesNotMarkCompleteBodyPartial(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	d.Put(waketest.Session{Key: "codex:example", Agent: "codex", UpdatedAt: 1000}, waketest.Message{Role: "assistant", Text: "needle"})
	d.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<20050) INSERT INTO messages(session_key,seq,role,ts,text) SELECT 'codex:example',x+1,'assistant',1000+x,'needle' FROM n`)
	d.Structure("codex:example", 1, sessions.Message{Thinking: "reason"})
	x := Open(d.Path)
	defer x.Close()
	r, e := x.Search(context.Background(), sessions.SearchQuery{Terms: []string{"needle"}, Limit: 10})
	if e != nil || r.Partial || r.ScannedSince == 0 || len(r.Hits) != 10 || !strings.Contains(r.Note, "message text searched completely") {
		t.Fatal(r.Partial, r.ScannedSince, len(r.Hits), r.Note, e)
	}
}

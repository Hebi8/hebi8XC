package wake

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
	"modernc.org/sqlite"
)

var cancelFunctionID atomic.Int64

func cancelOnText(t *testing.T, cancel context.CancelFunc) string {
	t.Helper()
	name := fmt.Sprintf("cancel_search_%d", cancelFunctionID.Add(1))
	if e := sqlite.RegisterScalarFunction(name, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if s, ok := args[0].(string); ok && strings.Contains(s, "cancel") {
			cancel()
		}
		return args[0], nil
	}); e != nil {
		t.Fatal(e)
	}
	return name
}
func TestFilteredCancellationRetainsCompletedSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fn := cancelOnText(t, cancel)
	d := waketest.New(t, t.TempDir())
	for i := 0; i < 16; i++ {
		key := fmt.Sprintf("codex:%02d", i)
		text := "ok"
		if i == 8 {
			text = "cancel"
		}
		d.Put(waketest.Session{Key: key, UpdatedAt: int64(2000 - i)}, waketest.Message{Role: "assistant", Text: text, Time: int64(2000 - i)})
		d.Structure(key, 1, sessions.Message{Calls: []sessions.Call{{Tool: "Bash", Input: "command"}}})
	}
	d.Exec(`ALTER TABLE messages RENAME TO raw_messages`)
	d.Exec(`CREATE VIEW messages AS SELECT id,session_key,sidechain_id,seq,role,ts,` + fn + `(text) text FROM raw_messages`)
	x := Open(d.Path)
	defer x.Close()
	r, e := x.Search(ctx, sessions.SearchQuery{Tool: "Bash", Limit: 50})
	if e != nil || !r.Partial || len(r.Hits) != 8 || !strings.Contains(r.Note, "messages examined through "+time.UnixMilli(1994).UTC().Format(time.RFC3339Nano)) {
		t.Fatal(len(r.Hits), r.Partial, r.Note, e)
	}
}
func TestFilteredOrderingUsesSessionUpperBound(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	for i := 0; i < 16; i++ {
		key := fmt.Sprintf("codex:%02d", i)
		at := int64(50)
		if i == 8 {
			at = 1900
		}
		d.Put(waketest.Session{Key: key, UpdatedAt: int64(2000 - i)}, waketest.Message{Role: "assistant", Text: "text", Time: at})
		d.Structure(key, 1, sessions.Message{Calls: []sessions.Call{{Tool: "Bash"}}})
	}
	x := Open(d.Path)
	defer x.Close()
	r, e := x.Search(context.Background(), sessions.SearchQuery{Tool: "Bash", Limit: 1})
	if e != nil || r.Partial || len(r.Hits) != 1 || r.Hits[0].Key != "codex:08" {
		t.Fatal(r, e)
	}
}
func TestSideCancellationKeepsCompleteIndexedBody(t *testing.T) {
	for _, term := range []string{"needle", "js"} {
		t.Run(term, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fn := cancelOnText(t, cancel)
			d := waketest.New(t, t.TempDir())
			d.Put(waketest.Session{Key: "codex:example", UpdatedAt: 2000}, waketest.Message{Role: "assistant", Text: term, Time: 1000}, waketest.Message{Role: "assistant", Text: "other", Time: 1900})
			d.Structure("codex:example", 1, sessions.Message{})
			d.Structure("codex:example", 2, sessions.Message{Thinking: "cancel " + term})
			d.Exec(`ALTER TABLE message_extras RENAME TO raw_extras`)
			d.Exec(`CREATE VIEW message_extras AS SELECT session_key,seq,kind,` + fn + `(thinking) thinking,model,full_text,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens FROM raw_extras`)
			x := Open(d.Path)
			defer x.Close()
			r, e := x.Search(ctx, sessions.SearchQuery{Terms: []string{term}, Limit: 50})
			if e != nil {
				t.Fatal(e)
			}
			if term == "needle" {
				if r.Partial || len(r.Hits) != 1 || r.ScannedSince == 0 {
					t.Fatal(r)
				}
			} else if !r.Partial {
				t.Fatal("short-word body was incomplete", r)
			}
		})
	}
}

// Grouped filter batches take every remaining candidate, so they stay at
// eight sessions: 24 sessions of 500 calls each fill ten groups in two such
// batches (8,000 candidates), while a doubled second batch of 16 sessions would
// need 8,000 of the 6,000 left and stop at the candidate limit.
func TestGroupedFilterBatchesStayWithinCandidates(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	for i := 0; i < 24; i++ {
		d.Put(waketest.Session{Key: fmt.Sprintf("codex:%02d", i), UpdatedAt: int64(100000 - i)})
	}
	d.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<500)
 INSERT INTO messages(session_key, seq, role, ts, text) SELECT s.key, n.i, 'assistant', s.updated_at-501+n.i, 'call' FROM sessions s, n`)
	d.Exec(`UPDATE sessions SET message_count=(SELECT count(*) FROM messages WHERE session_key=sessions.key)`)
	d.Exec(waketest.StructureSchema)
	d.Exec(`INSERT INTO tool_calls SELECT session_key, seq, 0, 'c'||id, 'Bash', NULL, NULL, 0, NULL FROM messages`)
	for i := 0; i < 24; i++ {
		d.SealStructure(fmt.Sprintf("codex:%02d", i))
	}
	x := Open(d.Path)
	defer x.Close()
	r, e := x.Search(context.Background(), sessions.SearchQuery{Tool: "Bash", BySession: true, Limit: 10})
	if e != nil || r.Partial || len(r.Sessions) != 10 || r.Sessions[0].Key != "codex:00" {
		t.Fatal(len(r.Sessions), r.Partial, r.Note, e)
	}
}

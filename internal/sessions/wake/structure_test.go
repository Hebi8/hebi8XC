package wake_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

func TestStructureAndFullText(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	prefix := strings.Repeat("x", 32<<10)
	d.Put(waketest.Session{Key: "codex:example", Agent: "codex"}, waketest.Message{Role: "assistant", Text: prefix})
	x := wake.Open(d.Path)
	defer x.Close()
	var log bytes.Buffer
	x.Log = slog.New(slog.NewTextHandler(&log, nil))
	for range 2 {
		m, _, err := x.Messages(ctx, "codex:example", 0, 10, sessions.PageBudget)
		if err != nil || len(m) != 1 || m[0].Kind != "" {
			t.Fatal("stock", err)
		}
	}
	if strings.Count(log.String(), "no supported structure") != 1 {
		t.Fatal(log.String())
	}
	before, _ := x.Sessions(ctx)
	full := prefix + strings.Repeat("after cutoff", 30000)
	d.Structure("codex:example", 1, sessions.Message{Text: full, Thinking: "consider the boundary", Model: "model-x", Calls: []sessions.Call{
		{Call: "c1", Tool: "shell", Input: "go test ./...", Output: "failed", Error: true},
		{Call: "c2", Tool: "spawn_agent", Agent: "child"},
	}})
	read := func() sessions.Message {
		t.Helper()
		m, more, err := x.Messages(ctx, "codex:example", 0, 10, sessions.PageBudget)
		if err != nil || more || len(m) != 1 {
			t.Fatalf("read: %d %v %v", len(m), more, err)
		}
		return m[0]
	}
	m := read()
	if m.Text != full || m.Truncated || m.Kind != "text" || m.Thinking == "" || m.Model != "model-x" || len(m.Calls) != 2 || !m.Calls[0].Error || m.Calls[1].Agent != "codex:child" {
		t.Fatal("structure or full text lost")
	}
	current, err := x.Sessions(ctx)
	if err != nil || current[0].Stamp == before[0].Stamp || *current[0].ToolCalls != 2 || *current[0].ToolErrors != 1 {
		t.Fatal("backfill stamp/counts", err)
	}
	d.Exec(`UPDATE message_extras SET full_text=full_text || 'changed'`)
	d.Exec(`UPDATE sessions SET file_mtime=file_mtime+1`)
	d.SealStructure("codex:example")
	after, err := x.Sessions(ctx)
	if err != nil || after[0].Stamp == current[0].Stamp || read().Text != full+"changed" {
		t.Fatal("change beyond cutoff", err)
	}
	// A stock parser changes the rows without changing the file or FTS format.
	d.Exec(`INSERT INTO messages(session_key,seq,role,text) VALUES ('codex:example',2,'assistant','new')`)
	for range 2 {
		msgs, _, err := x.Messages(ctx, "codex:example", 0, 10, sessions.PageBudget)
		if err != nil || len(msgs) != 2 || msgs[0].Kind != "" || msgs[0].Text != prefix || len(msgs[0].Calls) != 0 {
			t.Fatal("stale rows not discarded", err)
		}
	}
	if !strings.Contains(log.String(), "session=codex:example") {
		t.Fatal("stale log does not identify the session", log.String())
	}
	if _, _, err := x.Messages(ctx, "codex:missing", 0, 10, sessions.PageBudget); err != nil {
		t.Fatal(err)
	}
	if strings.Count(log.String(), "structure is stale") != 1 {
		t.Fatal(log.String())
	}
}

func TestStructureStampChecks(t *testing.T) {
	for _, change := range []string{
		`UPDATE sessions SET file_size=file_size+1`,
		`UPDATE sessions SET file_mtime=file_mtime+1`,
		`UPDATE schema_meta SET value='20' WHERE key='fts_format'`,
		`UPDATE structure_sessions SET format='3'`,
		`DELETE FROM structure_sessions`,
		`UPDATE schema_meta SET value='3' WHERE key='structure_format'`,
		`DROP TABLE tool_calls`,
		`DROP TABLE tool_files`,
		`ALTER TABLE schema_meta DROP COLUMN value`,
		`ALTER TABLE message_extras DROP COLUMN input_tokens`,
	} {
		t.Run(change, func(t *testing.T) {
			d := waketest.New(t, t.TempDir())
			d.Put(waketest.Session{Key: "agent:one", Agent: "agent"}, waketest.Message{Role: "user", Text: "plain"})
			d.Structure("agent:one", 1, sessions.Message{Thinking: "thought"})
			d.Exec(change)
			x := wake.Open(d.Path)
			defer x.Close()
			m, _, err := x.Messages(ctx, "agent:one", 0, 10, sessions.PageBudget)
			if err != nil || len(m) != 1 || m[0].Kind != "" || m[0].Thinking != "" || m[0].Text != "plain" {
				t.Fatal("fallback", err)
			}
		})
	}
}

func TestUsageTotalsAndNulls(t *testing.T) {
	for _, format := range []string{"stock", "1", "2"} {
		t.Run(format, func(t *testing.T) {
			d := waketest.New(t, t.TempDir())
			total := int64(900)
			for _, key := range []string{"agent:usage", "agent:total", "agent:unknown", "agent:zero", "agent:partial"} {
				var tokens *int64
				if key == "agent:usage" || key == "agent:total" {
					tokens = &total
				}
				d.Put(waketest.Session{Key: key, Agent: "agent", Tokens: tokens},
					waketest.Message{Role: "assistant", Text: "one"}, waketest.Message{Role: "assistant", Text: "two"})
				if format == "1" {
					d.StructureV1(key, 1, sessions.Message{Thinking: "thought"})
				}
				if format == "2" {
					d.Structure(key, 1, sessions.Message{})
				}
			}
			if format == "2" {
				d.Structure("agent:usage", 1, sessions.Message{Usage: &sessions.Usage{Input: 10, Output: 2, CacheRead: 3, CacheWrite: 4}})
				d.Structure("agent:usage", 2, sessions.Message{Usage: &sessions.Usage{Input: 20, Output: 5, CacheRead: 7, CacheWrite: 9}})
				d.Structure("agent:zero", 1, sessions.Message{Usage: &sessions.Usage{}})
				d.Exec(`UPDATE message_extras SET output_tokens=6 WHERE session_key='agent:partial'`)
				// A side row without an indexed message must not enter the totals.
				d.Exec(`INSERT INTO message_extras(session_key,seq,kind,input_tokens) VALUES ('agent:usage',99,'text',999)`)
			}
			x := wake.Open(d.Path)
			defer x.Close()
			list, err := x.Sessions(ctx)
			if err != nil || len(list) != 5 {
				t.Fatalf("listing: %d %v", len(list), err)
			}
			for _, s := range list {
				raw, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case s.Key == "agent:usage" && format == "2":
					if s.Tokens == nil || s.Tokens.Total != nil || *s.Tokens.Input != 30 || *s.Tokens.Output != 7 || *s.Tokens.CacheRead != 10 || *s.Tokens.CacheWrite != 13 {
						t.Fatalf("sum: %s", raw)
					}
				case s.Key == "agent:total" || s.Key == "agent:usage":
					if !bytes.Contains(raw, []byte(`"tokens":{"total":900}`)) {
						t.Fatalf("total fallback: %s", raw)
					}
				case s.Key == "agent:zero" && format == "2":
					if !bytes.Contains(raw, []byte(`"tokens":{"input":0,"output":0,"cache_read":0,"cache_write":0}`)) {
						t.Fatalf("known zero: %s", raw)
					}
				case s.Key == "agent:partial" && format == "2":
					if s.Tokens == nil || *s.Tokens.Output != 6 {
						t.Fatalf("partial: %s", raw)
					}
				default:
					if s.Tokens != nil || bytes.Contains(raw, []byte(`"tokens"`)) {
						t.Fatalf("unknown: %s", raw)
					}
				}
			}
			for _, tc := range []struct {
				key  string
				want *sessions.Usage
			}{
				{"agent:unknown", nil}, {"agent:zero", &sessions.Usage{}}, {"agent:partial", &sessions.Usage{Output: 6}},
			} {
				msgs, _, err := x.Messages(ctx, tc.key, 0, 10, sessions.PageBudget)
				want := tc.want
				if format != "2" {
					want = nil
				}
				if err != nil || len(msgs) != 2 || !reflect.DeepEqual(msgs[0].Usage, want) || msgs[1].Usage != nil {
					t.Fatalf("message usage %s: %+v %v", tc.key, msgs, err)
				}
			}
		})
	}
}

func TestFormatUpgradeAndBackfillMoveStamp(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	const key = "agent:one"
	d.Put(waketest.Session{Key: key, Agent: "agent"}, waketest.Message{Role: "assistant", Text: "indexed"})
	x := wake.Open(d.Path)
	defer x.Close()
	read := func() sessions.Session {
		t.Helper()
		list, err := x.Sessions(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("list: %v %v", list, err)
		}
		return list[0]
	}
	stock := read()
	d.StructureV1(key, 1, sessions.Message{Thinking: "thought", Calls: []sessions.Call{{Call: "c", Tool: "edit", Output: "done"}}})
	one := read()
	msgs, _, err := x.Messages(ctx, key, 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 1 || msgs[0].Thinking != "thought" || len(msgs[0].Calls) != 1 || msgs[0].Usage != nil || one.Stamp == stock.Stamp {
		t.Fatalf("format1: %+v %v", msgs, err)
	}
	d.Structure(key, 1, sessions.Message{Usage: &sessions.Usage{Input: 7}, Calls: []sessions.Call{{Call: "c", Tool: "edit", Output: "done", Files: []sessions.File{{Path: "/project/a.go", Op: "change"}}}}})
	two := read()
	if two.Stamp == one.Stamp || two.Tokens == nil || *two.Tokens.Input != 7 || !reflect.DeepEqual(two.Files, []string{"/project/a.go"}) {
		t.Fatalf("upgrade: %+v", two)
	}
	// The schema can be upgraded before a session is reparsed. Never read that
	// session's old side rows as if they were format 2.
	d.Exec(`UPDATE structure_sessions SET format='1'`)
	stale := read()
	msgs, _, err = x.Messages(ctx, key, 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 1 || msgs[0].Kind != "" || stale.Tokens != nil || len(stale.Files) != 0 {
		t.Fatalf("stale format: %+v %+v %v", stale, msgs, err)
	}
	d.SealStructure(key)
	backfill := read()
	if backfill.Stamp == stale.Stamp || backfill.Tokens == nil {
		t.Fatalf("backfill: %+v", backfill)
	}
	if read().Stamp != backfill.Stamp {
		t.Fatal("unchanged stamp moved")
	}
}

func TestToolFilesAndSessionLimit(t *testing.T) {
	d := waketest.New(t, t.TempDir())
	const key = "agent:one"
	d.Put(waketest.Session{Key: key, Agent: "agent"}, waketest.Message{Role: "assistant", Text: "one"}, waketest.Message{Role: "assistant", Text: "two"})
	calls := []sessions.Call{
		{Call: "edit", Tool: "edit", Output: "done", Files: []sessions.File{{Path: "/project/a", Op: "change"}, {Path: "/project/a", Op: "change"}, {Path: "/project/a", Op: "read"}, {Path: "/project/b", Op: "delete"}}},
		{Call: "failed", Tool: "edit", Output: "error", Error: true, Files: []sessions.File{{Path: "/project/failed", Op: "change"}}},
		{Call: "read", Tool: "read", Output: "text", Files: []sessions.File{{Path: "/project/read", Op: "read"}}},
		{Call: "pending", Tool: "write", Files: []sessions.File{{Path: "/project/pending", Op: "change"}}},
	}
	d.Structure(key, 1, sessions.Message{Calls: calls})
	d.Structure(key, 2, sessions.Message{Calls: []sessions.Call{{Call: "same-id", Tool: "edit", Output: "done", Files: []sessions.File{{Path: "/project/a", Op: "change"}}}}})
	x := wake.Open(d.Path)
	defer x.Close()
	list, err := x.Sessions(ctx)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].Files, []string{"/project/a", "/project/b"}) {
		t.Fatalf("files: %+v %v", list, err)
	}
	msgs, _, err := x.Messages(ctx, key, 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 2 || len(msgs[0].Calls) != 4 || len(msgs[0].Calls[0].Files) != 3 || len(msgs[0].Calls[1].Files) != 1 || len(msgs[0].Calls[2].Files) != 1 || len(msgs[1].Calls[0].Files) != 1 {
		t.Fatalf("call files: %+v %v", msgs, err)
	}
	// Same path with different operations survives the all-column primary key.
	if msgs[0].Calls[0].Files[0].Op != "change" || msgs[0].Calls[0].Files[1].Op != "read" {
		t.Fatal("operation lost")
	}
	many := []sessions.File{{Path: "/project/a", Op: "change"}}
	for i := 0; i < 510; i++ {
		many = append(many, sessions.File{Path: fmt.Sprintf("/project/file-%03d", i), Op: "change"})
	}
	d.Structure(key, 2, sessions.Message{Calls: []sessions.Call{{Call: "patch", Tool: "patch", Output: "done", Files: many}}})
	list, err = x.Sessions(ctx)
	if err != nil || len(list) != 1 || len(list[0].Files) != 500 || list[0].Files[499] != "/project/file-497" {
		t.Fatalf("file cap: %v %v", list, err)
	}
	// Message file lists are preserved; only the session summary is capped.
	msgs, _, err = x.Messages(ctx, key, 0, 10, sessions.PageBudget)
	if err != nil || len(msgs[1].Calls[0].Files) != 511 {
		t.Fatalf("full call files: %v", err)
	}
}

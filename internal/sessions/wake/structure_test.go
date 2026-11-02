package wake_test

import (
	"bytes"
	"log/slog"
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
	if strings.Count(log.String(), "structure is stale") != 1 {
		t.Fatal(log.String())
	}
}

func TestStructureStampChecks(t *testing.T) {
	for _, change := range []string{
		`UPDATE sessions SET file_size=file_size+1`,
		`UPDATE sessions SET file_mtime=file_mtime+1`,
		`UPDATE schema_meta SET value='20' WHERE key='fts_format'`,
		`UPDATE structure_sessions SET format='2'`,
		`DELETE FROM structure_sessions`,
		`UPDATE schema_meta SET value='2' WHERE key='structure_format'`,
		`DROP TABLE tool_calls`,
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

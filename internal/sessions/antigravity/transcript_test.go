package antigravity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/antigravity/acptest"
)

func TestTranscriptStructureKeepsDatabaseText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conversations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	d := acptest.New(t, dir, "example")
	full := strings.Repeat("full database text ", 20000)
	d.Put(0, "user", "question", "", "", 1700000000000)
	d.Put(1, "assistant", full, "", "model-x", 1700000001000)
	d.Put(2, "assistant", "", "", "model-x", 1700000002000)
	d.Exec("INSERT INTO steps VALUES (3,21,3,X'',X'')")
	x := Open(dir)
	plain, err := x.Sessions(ctx)
	if err != nil || plain[0].Messages != 2 {
		t.Fatal(err)
	}
	write := func(result string) {
		d.Transcript(
			map[string]any{"step_index": 0, "type": "USER_INPUT", "content": "wrapped question"},
			map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "content": "truncated transcript", "thinking": "reason", "input_tokens": 7, "output_tokens": 5, "cache_read_tokens": 3},
			map[string]any{"step_index": 2, "type": "PLANNER_RESPONSE", "tool_calls": []any{map[string]any{"name": "shell", "args": map[string]any{"command": "false"}}}},
			map[string]any{"step_index": 3, "type": "RUN_COMMAND", "content": result, "status": "ERROR", "exit_code": 1, "error": "failed"},
		)
	}
	write("failed")
	list, err := x.Sessions(ctx)
	if err != nil || list[0].Messages != 3 || list[0].Stamp == plain[0].Stamp || *list[0].ToolCalls != 1 || *list[0].ToolErrors != 1 {
		t.Fatal("metadata", err)
	}
	msgs, more, err := x.Messages(ctx, "antigravity-acp:example", 0, 10, sessions.PageBudget)
	if err != nil || more || len(msgs) != 3 {
		t.Fatalf("read %d %v %v", len(msgs), more, err)
	}
	if msgs[1].Text != full || msgs[1].Truncated || msgs[1].Thinking != "reason" || msgs[1].Usage == nil || *msgs[1].Usage != (sessions.Usage{Input: 7, Output: 5, CacheRead: 3}) {
		t.Fatal("text/thinking/usage")
	}
	if msgs[2].Text != "" || msgs[2].ID != 3 || msgs[2].Seq != 2 || len(msgs[2].Calls) != 1 || !msgs[2].Calls[0].Error || msgs[2].Calls[0].Output != "failed" {
		t.Fatal("empty host/results")
	}
	write("completed with a longer error")
	next, err := x.Sessions(ctx)
	if err != nil || next[0].Stamp == list[0].Stamp {
		t.Fatal("transcript change did not invalidate stamp", err)
	}
	if err := os.Remove(transcriptPath(d.Path)); err != nil {
		t.Fatal(err)
	}
	msgs, _, err = x.Messages(ctx, "antigravity-acp:example", 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 2 || msgs[1].Kind != "" || msgs[1].Usage != nil || len(msgs[1].Calls) != 0 {
		t.Fatal("no transcript fallback", err)
	}
}

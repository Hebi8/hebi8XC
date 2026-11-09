package antigravity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/antigravity/acptest"
)

func TestSearchACPTextThinkingAndFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conversations")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	d := acptest.New(t, dir, "example")
	d.Put(0, "user", `"quoted" AND NEAR * : ^ -- 中文测试 js`, "", "", 1000)
	d.Put(1, "assistant", "answer", "", "", 2000)
	d.Transcript(map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "thinking": "hidden rationale", "tool_calls": []any{map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": "/home/alice/project/sessions/store.go", "CodeContent": "needle"}}}}, map[string]any{"step_index": 2, "type": "TOOL_RESULT", "content": "done", "status": "DONE"})
	x := Open(dir)
	for _, term := range []string{"quoted", "AND", "NEAR", "*", ":", "^", "--", "中文", "中文测试", "js", "rationale", "needle"} {
		r, e := sessions.ScanSearch(ctx, x, sessions.SearchQuery{Terms: []string{term}, Limit: 50})
		if e != nil || len(r.Hits) < 1 {
			t.Fatal(term, len(r.Hits), e)
		}
	}
	r, e := sessions.ScanSearch(ctx, x, sessions.SearchQuery{File: "sessions/store.go", Limit: 50})
	if e != nil || len(r.Hits) != 1 || r.Hits[0].Call == nil {
		t.Fatal(r, e)
	}
	r, e = sessions.ScanSearch(ctx, x, sessions.SearchQuery{File: "xstore.go", Limit: 50})
	if e != nil || len(r.Hits) != 0 {
		t.Fatal(r, e)
	}
}

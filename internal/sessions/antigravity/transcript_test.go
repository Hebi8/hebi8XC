package antigravity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestBadTranscriptLinesKeepSession(t *testing.T) {
	for name, bad := range map[string]string{
		"partial tail":     `{"step_index":2,"type":`,
		"empty line":       "\n",
		"string exit code": "{\"step_index\":2,\"type\":\"RUN_COMMAND\",\"exit_code\":\"1\"}\n",
		"object content":   "{\"step_index\":2,\"type\":\"RUN_COMMAND\",\"content\":{}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "conversations")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			d := acptest.New(t, dir, "example")
			d.Put(0, "user", "database question", "", "", 1700000000000)
			d.Put(1, "assistant", "database answer", "", "", 1700000001000)
			d.Transcript(map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "thinking": "valid structure"})
			store, err := sessions.OpenStore(filepath.Join(t.TempDir(), "sessions.db"), func() time.Time { return time.Unix(1700000000, 0) })
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			x := Open(dir)
			list, err := x.Sessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetLocal(ctx, "desk", list); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(transcriptPath(d.Path), os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(bad); err != nil {
				t.Fatal(err)
			}
			f.Close()
			// The cached reader and a fresh reader after restart must both retain it.
			for _, reader := range []*Index{x, Open(dir)} {
				list, err := reader.Sessions(ctx)
				if err != nil || len(list) != 1 {
					t.Fatalf("sessions: %v %v", list, err)
				}
				if _, err := store.SetLocal(ctx, "desk", list); err != nil {
					t.Fatal(err)
				}
				_, _, ok, err := store.Lookup(ctx, "desk", "antigravity-acp:example")
				if err != nil || !ok {
					t.Fatal("session deleted after bad transcript", err)
				}
				msgs, more, err := reader.Messages(ctx, "antigravity-acp:example", 0, 10, sessions.PageBudget)
				if err != nil || more || len(msgs) != 2 || msgs[0].Text != "database question" || msgs[1].Text != "database answer" || msgs[1].Thinking != "valid structure" {
					t.Fatalf("messages: %+v %v %v", msgs, more, err)
				}
			}
		})
	}
}

func TestTranscriptCountsOnlyDatabaseSteps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conversations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	d := acptest.New(t, dir, "example")
	d.Put(0, "assistant", "database answer", "", "", 1700000000000)
	d.Exec("INSERT INTO steps VALUES (2,21,3,X'',X'')")
	call := []any{map[string]any{"name": "shell", "args": "false"}}
	d.Transcript(
		map[string]any{"step_index": 0, "type": "PLANNER_RESPONSE", "tool_calls": call, "status": "ERROR"},
		map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "tool_calls": call, "status": "ERROR"},
		map[string]any{"step_index": 2, "type": "PLANNER_RESPONSE", "tool_calls": call, "status": "ERROR"},
	)
	x := Open(dir)
	list, err := x.Sessions(ctx)
	if err != nil || len(list) != 1 || *list[0].ToolCalls != 1 || *list[0].ToolErrors != 1 {
		t.Fatalf("counts: %+v %v", list, err)
	}
	msgs, _, err := x.Messages(ctx, "antigravity-acp:example", 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 1 || len(msgs[0].Calls) != 1 || !msgs[0].Calls[0].Error {
		t.Fatalf("messages: %+v %v", msgs, err)
	}
}

// appendTranscript adds raw lines, which d.Transcript would have encoded.
func appendTranscript(t *testing.T, path, lines string) {
	t.Helper()
	f, err := os.OpenFile(transcriptPath(path), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(lines); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedTranscriptStepKeepsItsPlace(t *testing.T) {
	const (
		twoCalls = `{"step_index":1,"type":"PLANNER_RESPONSE","tool_calls":[{"name":"first","args":"a"},{"name":"second","args":"b"}]}` + "\n"
		oneCall  = `{"step_index":1,"type":"PLANNER_RESPONSE","tool_calls":[{"name":"earlier","args":"a"}]}` + "\n"
	)
	for name, tc := range map[string]struct {
		lines string
		want  []sessions.Call
	}{
		"mistyped result": {
			twoCalls + `{"step_index":2,"type":"RUN_COMMAND","exit_code":"1","content":"first output"}` + "\n" +
				`{"step_index":3,"type":"RUN_COMMAND","content":"second output"}` + "\n",
			[]sessions.Call{{Call: "step-1-0", Tool: "first", Input: "a"}, {Call: "step-1-1", Tool: "second", Input: "b", Output: "second output"}},
		},
		"mistyped planner": {
			oneCall + `{"step_index":2,"type":"PLANNER_RESPONSE","content":{}}` + "\n" +
				`{"step_index":3,"type":"RUN_COMMAND","status":"ERROR","content":"belongs to the lost step"}` + "\n",
			[]sessions.Call{{Call: "step-1-0", Tool: "earlier", Input: "a"}},
		},
		"valid steps after a line that is not JSON": {
			twoCalls + "{\"step_index\":2,\"type\":\n" + `{"step_index":3,"type":"RUN_COMMAND","content":"first output"}` + "\n",
			[]sessions.Call{{Call: "step-1-0", Tool: "first", Input: "a", Output: "first output"}, {Call: "step-1-1", Tool: "second", Input: "b"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "conversations")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			d := acptest.New(t, dir, "example")
			d.Put(0, "user", "question", "", "", 1700000000000)
			d.Put(1, "assistant", "answer", "", "", 1700000001000)
			d.Transcript()
			appendTranscript(t, d.Path, tc.lines)
			msgs, _, err := Open(dir).Messages(ctx, "antigravity-acp:example", 0, 10, sessions.PageBudget)
			if err != nil || len(msgs) != 2 || !reflect.DeepEqual(msgs[1].Calls, tc.want) {
				t.Fatalf("messages: %+v %v, want calls %+v", msgs, err, tc.want)
			}
		})
	}
}

func TestUnreadableTranscriptLeavesDatabaseText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conversations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	d := acptest.New(t, dir, "example")
	d.Put(0, "user", "question", "", "", 1700000000000)
	// A directory opens and then fails to read, on every platform.
	if err := os.MkdirAll(transcriptPath(d.Path), 0700); err != nil {
		t.Fatal(err)
	}
	x := Open(dir)
	list, err := x.Sessions(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions: %+v %v", list, err)
	}
	msgs, _, err := x.Messages(ctx, "antigravity-acp:example", 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "question" {
		t.Fatalf("messages: %+v %v", msgs, err)
	}
}

func TestExplicitToolFiles(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "project")
	absolute := filepath.Join(cwd, "absolute.go")
	for _, tc := range []struct{ tool, field, op string }{
		{"client_create_file", "target_file", "change"},
		{"client_edit_file", "target_file", "change"},
		{"write_to_file", "TargetFile", "change"},
		{"replace_file_content", "TargetFile", "change"},
		{"client_view_file", "absolute_path", "read"},
		{"view_file", "AbsolutePath", "read"},
		{"view_file", "absolute_path", "read"},
	} {
		t.Run(tc.tool+tc.field, func(t *testing.T) {
			for _, path := range []string{"src/../main.go", absolute} {
				raw, _ := json.Marshal(map[string]string{tc.field: path})
				want := path
				if !filepath.IsAbs(path) {
					want = filepath.Join(cwd, path)
				}
				got := toolFiles(tc.tool, string(raw), cwd)
				if !reflect.DeepEqual(got, []sessions.File{{Path: want, Op: tc.op}}) {
					t.Fatalf("files: %+v", got)
				}
			}
		})
	}
	for _, tc := range []struct{ tool, input, cwd string }{
		{"run_command", `{"command":"rm main.go","TargetFile":"main.go"}`, cwd},
		{"call_mcp_tool", `{"TargetFile":"main.go"}`, cwd},
		{"unknown_edit", `{"TargetFile":"main.go"}`, cwd},
		{"write_to_file", `{"TargetFile":123}`, cwd},
		{"write_to_file", `{"TargetFile":""}`, cwd},
		{"write_to_file", `not json`, cwd},
	} {
		if got := toolFiles(tc.tool, tc.input, tc.cwd); len(got) != 0 {
			t.Fatalf("unexpected files: %+v", got)
		}
	}
}

func TestTranscriptSessionTokensAndFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conversations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	d := acptest.New(t, dir, "example")
	cwd := filepath.Join(t.TempDir(), "project")
	d.CWD(cwd)
	d.Put(0, "assistant", "answer", "", "", 1700000000000)
	d.Put(7, "assistant", "another answer", "", "", 1700000007000)
	d.Exec("INSERT INTO steps VALUES (8,21,3,X'',X'')")
	call := func(name, field, path string) map[string]any {
		return map[string]any{"name": name, "args": map[string]any{field: path}}
	}
	d.Transcript(
		map[string]any{"step_index": 0, "type": "PLANNER_RESPONSE", "input_tokens": 7, "output_tokens": 5, "cache_read_tokens": 3, "tool_calls": []any{
			call("client_create_file", "target_file", "a.go"),
			call("client_edit_file", "target_file", "a.go"),
			call("replace_file_content", "TargetFile", "failed.go"),
			call("view_file", "AbsolutePath", "read.go"),
			call("run_command", "command", "touch shell.go"),
			call("write_to_file", "TargetFile", "pending.go"),
		}},
		map[string]any{"step_index": 1, "type": "CODE_ACTION", "content": "created", "status": "DONE"},
		map[string]any{"step_index": 2, "type": "CODE_ACTION", "content": "edited", "status": "DONE"},
		map[string]any{"step_index": 3, "type": "CODE_ACTION", "content": "failure", "exit_code": 1},
		map[string]any{"step_index": 4, "type": "VIEW_FILE", "content": "text"},
		map[string]any{"step_index": 5, "type": "RUN_COMMAND", "content": "done"},
		// The pending call has no result before the next planner step.
		map[string]any{"step_index": 7, "type": "PLANNER_RESPONSE", "input_tokens": 11, "output_tokens": 2, "cache_read_tokens": 4, "tool_calls": []any{
			map[string]any{"name": "write_to_file", "args": `{"TargetFile":"b.go"}`},
		}},
		map[string]any{"step_index": 8, "type": "CODE_ACTION", "content": "written"},
		// Orphan and non-message transcript rows do not contribute usage.
		map[string]any{"step_index": 9, "type": "PLANNER_RESPONSE", "input_tokens": 999},
	)
	x := Open(dir)
	list, err := x.Sessions(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions: %+v %v", list, err)
	}
	s := list[0]
	if s.Tokens == nil || s.Tokens.Total != nil || *s.Tokens.Input != 18 || *s.Tokens.Output != 7 || *s.Tokens.CacheRead != 7 || *s.Tokens.CacheWrite != 0 {
		t.Fatalf("tokens: %+v", s.Tokens)
	}
	if !reflect.DeepEqual(s.Files, []string{filepath.Join(cwd, "a.go"), filepath.Join(cwd, "pending.go"), filepath.Join(cwd, "b.go")}) {
		t.Fatalf("files: %v", s.Files)
	}
	msgs, _, err := x.Messages(ctx, "antigravity-acp:example", 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 2 || len(msgs[0].Calls) != 6 || len(msgs[0].Calls[2].Files) != 1 || !msgs[0].Calls[2].Error || msgs[0].Calls[3].Files[0].Op != "read" || len(msgs[0].Calls[4].Files) != 0 || len(msgs[1].Calls[0].Files) != 1 {
		t.Fatalf("messages: %+v %v", msgs, err)
	}
	// A cwd-only change invalidates both the metadata and relative file paths.
	nextCWD := filepath.Join(cwd, "nested")
	d.CWD(nextCWD)
	list, err = x.Sessions(ctx)
	if err != nil || list[0].Stamp == s.Stamp || list[0].Files[0] != filepath.Join(nextCWD, "a.go") {
		t.Fatalf("cwd update: %+v %v", list, err)
	}
	if err := os.Remove(transcriptPath(d.Path)); err != nil {
		t.Fatal(err)
	}
	list, err = x.Sessions(ctx)
	if err != nil || list[0].Tokens != nil || len(list[0].Files) != 0 {
		t.Fatalf("no transcript: %+v %v", list, err)
	}
}

func TestTranscriptFileCapAndKnownZeroUsage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conversations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	d := acptest.New(t, dir, "example")
	cwd := filepath.Join(t.TempDir(), "project")
	d.CWD(cwd)
	d.Put(0, "assistant", "answer", "", "", 1700000000000)
	var calls []any
	for i := 0; i < 505; i++ {
		calls = append(calls, map[string]any{"name": "client_create_file", "args": map[string]any{"target_file": fmt.Sprintf("file-%03d", i)}})
	}
	steps := []any{map[string]any{"step_index": 0, "type": "PLANNER_RESPONSE", "input_tokens": 0, "tool_calls": calls}}
	for i := 0; i < 505; i++ {
		steps = append(steps, map[string]any{"step_index": i + 1, "type": "CODE_ACTION", "content": "done"})
	}
	d.Transcript(steps...)
	list, err := Open(dir).Sessions(ctx)
	if err != nil || len(list) != 1 || len(list[0].Files) != 500 || list[0].Files[499] != filepath.Join(cwd, "file-499") || list[0].Tokens == nil || *list[0].Tokens.Input != 0 {
		t.Fatalf("cap/zero: %+v %v", list, err)
	}
}

func TestRelativeFileWithoutCWD(t *testing.T) {
	for _, cwd := range []string{"", "relative/project"} {
		got := toolFiles("write_to_file", `{"TargetFile":"../main.go"}`, cwd)
		if !reflect.DeepEqual(got, []sessions.File{{Path: "../main.go", Op: "change"}}) {
			t.Fatalf("files: %+v", got)
		}
	}
}

package demo

import (
	"fmt"
	"strings"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

// Data says what the demo holds, for whoever shows or checks it. All of it
// is made up.
//
// Desk and laptop have Wake indexes with structure: calls, their files and
// thinking. Attic has an older index of text only, so the copy kept of it is
// text only too, and a search over it is marked as incomplete.
type Data struct {
	Names    [3]string // desk, laptop, attic
	IDs      [3]string // their node ids
	Sessions [3]int    // how many sessions each machine has
	Long     string    // on laptop: a session of many pages; some of its answers make calls
	Code     string    // on desk: code blocks, long lines, a folded message, calls with files, thinking
	Review   string    // on laptop: reads the file that Code changed, and changes its test
	Kept     string    // on attic: read while it was online, so a copy is kept
	Unread   string    // on attic: never read, so it cannot be shown; its project has a long name

	Term   string // a word found on all three machines: live on desk and laptop, in the copy of attic
	File   string // the file Code changed and Review only read
	Anchor string // a phrase found in the input of one call of Long, far from its start
	Broad  string // a word found in more places than one answer holds
}

type project struct{ name, path, branch string }

var (
	agents   = []string{"claude", "codex", "gemini", "claude", "opencode", "codex", "claude"}
	models   = map[string]string{"claude": "opus", "codex": "gpt", "gemini": "flash"}
	projects = []project{
		{"atlas", "/home/alice/src/atlas", "main"},
		{"orchard", "/home/alice/src/orchard", "feature/pruning"},
		{"记账本", "/home/alice/src/ledger", "main"},
		{"tide-table", "/home/alice/src/tide-table", ""},
		{"notes", "/home/alice/notes", ""},
		{"garden-api", "/home/alice/work/garden-api", "fix/timeouts"},
	}
	english = [][]string{
		{"Fix", "Refactor", "Add tests for", "Document", "Speed up", "Review", "Explain"},
		{"the pagination of the orders list", "the importer's last-row handling", "the retry policy for uploads",
			"the nightly build", "the cache invalidation path", "the tide parser", "the settings loader",
			"the migration for the invoices table", "the flaky test in the sync loop"},
	}
	chinese = [][]string{
		{"修复", "重构", "补测试：", "梳理", "优化", "评审", "解释一下"},
		{"订单列表的分页", "导入时最后一行丢失的问题", "上传的重试策略", "夜间构建变慢的原因",
			"缓存失效的路径", "潮汐表的解析器", "配置的惰性加载", "发票表的迁移脚本", "同步循环里偶发的测试失败"},
	}
)

// fill puts the demo's sessions into the three indexes.
func fill(desk, laptop, attic *waketest.DB, now time.Time) Data {
	d := Data{Sessions: [3]int{59, 45, 12}, Term: "rows", File: "importer/rows.go", Anchor: "--batch 100", Broad: "batch"}
	ms := now.UnixMilli()
	const minute = 60_000
	n := 0 // counts all sessions, so that no two look the same
	ordinary := func(db *waketest.DB, machine string, count int, age func(i int) int64) {
		for i := range count {
			n++
			agent := agents[n%len(agents)]
			p := projects[n*5%len(projects)]
			title, zh := english[0][n%7]+" "+english[1][n*4%9], false
			if n%5 < 2 {
				title, zh = chinese[0][n%7]+chinese[1][n*4%9], true
			}
			if n == 17 {
				title = "" // a session without a title shows its key
			}
			at := ms - age(i)*minute
			db.Put(waketest.Session{Key: fmt.Sprintf("%s:%s-%03d", agent, machine, i), Agent: agent, Title: title,
				Project: p.name, Path: p.path, Branch: p.branch, Model: models[agent], UpdatedAt: at},
				talk(zh, 2+n%8, at)...)
		}
	}
	// Desk and laptop were used until now, less the further back; attic was
	// last used weeks ago.
	recent := func(i int) int64 { return int64(i*i*7 + i*13 + 5) }
	ordinary(desk, "desk", d.Sessions[0]-2, recent)
	ordinary(laptop, "laptop", d.Sessions[1]-2, func(i int) int64 { return recent(i) + 9 })
	ordinary(attic, "attic", d.Sessions[2]-2, func(i int) int64 { return int64(20+i*2) * 24 * 60 })

	d.Long = "claude:long-migration"
	var long []waketest.Message
	for i := range 260 {
		at := ms - int64(600-i)*minute
		if i%2 == 0 {
			long = append(long, waketest.Message{Role: "user", Time: at,
				Text: fmt.Sprintf("Step %d: move the next batch of accounts to the new schema and tell me what changed.", i/2+1)})
		} else {
			long = append(long, waketest.Message{Role: "assistant", Time: at,
				Text: fmt.Sprintf("Batch %d is moved: 40 accounts, 0 errors. The balances match the old tables.\n第 %d 批已迁移，余额核对一致。", i/2+1, i/2+1)})
		}
	}
	laptop.Put(waketest.Session{Key: d.Long, Agent: "claude", Title: "Migrate the ledger to the new schema, batch by batch",
		Project: "记账本", Path: "/home/alice/src/ledger", Branch: "migration", Model: "opus", UpdatedAt: ms - 3*minute}, long...)
	// Every 25th batch the answer shows the calls that moved it. The answer
	// to batch b is message 2b.
	migrate := func(b int, output string) sessions.Call {
		return sessions.Call{Call: fmt.Sprintf("migrate-%d", b), Tool: "shell",
			Input: fmt.Sprintf("go run ./cmd/migrate --batch %d", b), Output: output}
	}
	laptop.Structure(d.Long, 50, sessions.Message{Calls: []sessions.Call{migrate(25, "moved 40 accounts in 1.8s")}})
	laptop.Structure(d.Long, 100, sessions.Message{Calls: []sessions.Call{
		{Call: "edit-schema", Tool: "edit_file", Input: `{"path":"migrations/0042_accounts.sql","old":"balance NUMERIC(12,2)","new":"balance NUMERIC(14,2)"}`,
			Output: "updated 1 line", Files: []sessions.File{{Path: "/home/alice/src/ledger/migrations/0042_accounts.sql", Op: "change"}}},
		migrate(50, "moved 40 accounts in 1.7s"),
	}})
	failed := migrate(75, "ERROR: deadlock detected while moving account 3011\nHINT: another transaction holds a lock on accounts")
	failed.Error = true
	retried := migrate(75, "moved 40 accounts in 2.4s (1 retry)")
	retried.Call, retried.Input = "migrate-75-retry", retried.Input+" --retry"
	laptop.Structure(d.Long, 150, sessions.Message{
		Thinking: "The deadlock came from the nightly report, which reads the same rows. One retry is enough; no need to stop the report.",
		Calls:    []sessions.Call{failed, retried}})
	laptop.Structure(d.Long, 200, sessions.Message{Calls: []sessions.Call{
		migrate(100, "moved 40 accounts in 1.9s"),
		{Call: "read-balances", Tool: "read_file", Input: `{"path":"reports/balances.csv"}`,
			Output: "account,old,new\n3001,120.00,120.00\n3002,75.50,75.50\n3003,0.00,0.00",
			Files:  []sessions.File{{Path: "/home/alice/src/ledger/reports/balances.csv", Op: "read"}}},
	}})
	laptop.Structure(d.Long, 250, sessions.Message{Calls: []sessions.Call{migrate(125, "moved 40 accounts in 1.8s")}})

	// The fix of Code, looked at again on another machine.
	d.Review = "claude:importer-review"
	laptop.Put(waketest.Session{Key: d.Review, Agent: "claude", Title: "Review the fix for the importer's last row",
		Project: "atlas", Path: "/home/alice/src/atlas", Branch: "fix/last-row", Model: "opus", UpdatedAt: ms - 20*minute},
		waketest.Message{Role: "user", Time: ms - 26*minute,
			Text: "Review the change to importer/rows.go from desk. Are the rows at the end of a file imported now?"},
		waketest.Message{Role: "assistant", Time: ms - 25*minute,
			Text: "Yes. The loop now runs over all rows, and the importer tests pass on this machine as well."},
		waketest.Message{Role: "user", Time: ms - 22*minute, Text: "Add a case for a file that does not end with a newline."},
		waketest.Message{Role: "assistant", Time: ms - 21*minute, Text: "Added to the table of cases in the test; it passes."})
	laptop.Structure(d.Review, 2, sessions.Message{
		Thinking: "The new bound is len(rows). An empty file must not reach rows[len(rows)-1]; check that case too.",
		Calls: []sessions.Call{
			{Call: "read-rows", Tool: "read_file", Input: `{"path":"importer/rows.go"}`, Output: "for i := 0; i < len(rows); i++ {",
				Files: []sessions.File{{Path: "/home/alice/src/atlas/importer/rows.go", Op: "read"}}},
			{Call: "test-rows", Tool: "shell", Input: "go test ./importer -run TestRows -count=1", Output: "ok  \texample.com/atlas/importer\t0.388s"},
		}})
	laptop.Structure(d.Review, 4, sessions.Message{Calls: []sessions.Call{
		{Call: "edit-test", Tool: "edit_file", Input: `{"path":"importer/rows_test.go","old":"{\"trailing row\", 3},","new":"{\"trailing row\", 3},\n\t{\"no final newline\", 3},"}`,
			Output: "updated 1 line", Files: []sessions.File{{Path: "/home/alice/src/atlas/importer/rows_test.go", Op: "change"}}},
	}})

	d.Code = "codex:code-and-long-lines"
	desk.Put(waketest.Session{Key: d.Code, Agent: "codex", Title: "导入器丢最后一行：代码块、长行和很长的输出",
		Project: "atlas", Path: "/home/alice/src/atlas", Branch: "fix/last-row", Model: "gpt", UpdatedAt: ms - 2*minute},
		waketest.Message{Role: "user", Time: ms - 40*minute,
			Text: "The importer drops the last row of every file. The loop is in importer/rows.go. Docs: https://example.com/docs/importer?section=rows."},
		waketest.Message{Role: "assistant", Time: ms - 39*minute,
			Text: "The bound is off by one:\n\n```go\nfor i := 0; i < len(rows)-1; i++ {\n\tif err := imp.write(ctx, rows[i], writeOptions{Flush: i%batch == 0, Retry: defaultRetry, Deadline: time.Now().Add(perRowTimeout)}); err != nil {\n\t\treturn fmt.Errorf(\"row %d: %w\", i, err)\n\t}\n}\n```\n\n改成 `i < len(rows)` 就可以了。下面是修改后的测试输出。"},
		waketest.Message{Role: "tool", Sidechain: "sc-1", Time: ms - 38*minute,
			Text: "$ go test ./importer/\n" + strings.Repeat("=== RUN   TestRows/file_with_a_trailing_row\n--- PASS: TestRows/file_with_a_trailing_row (0.00s)\n", 70) + "PASS\nok  \texample.com/atlas/importer\t0.412s"},
		waketest.Message{Role: "user", Time: ms - 37*minute,
			Text: "这一行没有空格，看看会不会把页面撑开：\n" + strings.Repeat("0123456789abcdef", 60) + "\n还有一行很长但有空格的：\n" + strings.Repeat("the quick brown fox jumps over the lazy dog ", 30)},
		waketest.Message{Role: "assistant", Time: ms - 36*minute,
			Text: "Both wrap. A block that is opened and never closed stays text:\n\n```sh\ngo vet ./... && go test -race ./..."})

	rows := "/home/alice/src/atlas/importer/rows.go"
	desk.Structure(d.Code, 2, sessions.Message{
		Thinking: "Check the last row before changing the loop.\nThe fixture with a trailing row should fail first, so that the fix is seen to matter.",
		Calls: []sessions.Call{
			{Call: "read-rows", Tool: "read_file", Input: `{"path":"importer/rows.go"}`, Output: "for i := 0; i < len(rows)-1; i++ {\n\tif err := imp.write(ctx, rows[i], opts); err != nil {",
				Files: []sessions.File{{Path: rows, Op: "read"}}},
			{Call: "test-rows", Tool: "shell", Input: "go test ./importer -run TestRows",
				Output: "--- FAIL: TestRows/file_with_a_trailing_row (0.00s)\n    rows_test.go:41: got 2 rows, want 3: last row missing\nFAIL", Error: true},
			{Call: "patch-rows", Tool: "apply_patch",
				Input:  "*** Begin Patch\n*** Update File: importer/rows.go\n@@\n-for i := 0; i < len(rows)-1; i++ {\n+for i := 0; i < len(rows); i++ {\n*** End Patch",
				Output: "Success. Updated the following files:\nM importer/rows.go", Files: []sessions.File{{Path: rows, Op: "change"}}},
			{Call: "drop-fixture", Tool: "delete_file", Input: `{"path":"importer/testdata/rows_without_last.csv"}`, Output: "deleted",
				Files: []sessions.File{{Path: "/home/alice/src/atlas/importer/testdata/rows_without_last.csv", Op: "delete"}}},
		}})
	desk.Structure(d.Code, 5, sessions.Message{Calls: []sessions.Call{
		{Call: "vet", Tool: "shell", Input: "go vet ./... && go test -race ./...", Output: "ok  \texample.com/atlas/importer\t0.412s"},
	}})
	d.Kept = "claude:seed-order"
	attic.Put(waketest.Session{Key: d.Kept, Agent: "claude", Title: "Seed order for the spring beds",
		Project: "notes", Path: "/home/alice/notes", UpdatedAt: ms - 19*24*60*minute},
		waketest.Message{Role: "user", Time: ms - 19*24*60*minute, Text: "What did we decide to order for the two beds by the fence?"},
		waketest.Message{Role: "assistant", Time: ms - 19*24*60*minute, Text: "Carrots, chard and two rows of peas; the order goes out on Monday.\n胡萝卜、甜菜和两行豌豆，周一下单。"})

	// Two projects with names that do not fit a row of the list.
	desk.Put(waketest.Session{Key: "gemini:long-project-name", Agent: "gemini", Title: "Move the sign-in page to the new layout",
		Project: "customer-portal-frontend-redesign-2026", Path: "/home/alice/work/customer-portal-frontend-redesign-2026",
		Branch: "layout", Model: "flash", UpdatedAt: ms - 55*minute}, talk(false, 6, ms-55*minute)...)
	d.Unread = "opencode:long-project-name"
	attic.Put(waketest.Session{Key: d.Unread, Agent: "opencode", Title: "整理报销流程的自动化脚本",
		Project: "家庭财务与报销流程自动化工具集", Path: "/home/alice/src/家庭财务与报销流程自动化工具集",
		UpdatedAt: ms - 18*24*60*minute}, talk(true, 4, ms-18*24*60*minute)...)
	// The structure of every session on desk and laptop is current. Attic
	// keeps an index of text only.
	for _, db := range []*waketest.DB{desk, laptop} {
		db.Exec(waketest.StructureSchema)
		db.Exec(`INSERT OR REPLACE INTO structure_sessions
 SELECT key,file_mtime,file_size,(SELECT value FROM schema_meta WHERE key='fts_format'),
 (SELECT count(*) FROM messages WHERE session_key=s.key),(SELECT value FROM schema_meta WHERE key='structure_format') FROM sessions s`)
	}
	return d
}

// talk is a short exchange for an ordinary session.
func talk(zh bool, count int, at int64) []waketest.Message {
	lines := []string{
		"Here is where it goes wrong; can you take a look?",
		"I read the code around it. The cause is one condition that is checked too late.",
		"Good. Change it and run the tests.",
		"Done. All tests pass, and I added one for this case.",
	}
	if zh {
		lines = []string{
			"问题出在这里，帮我看一下。",
			"我读了相关的代码，原因是有一个条件判断得太晚。",
			"好，改掉然后跑一遍测试。",
			"改好了，测试全部通过，并且为这种情况补了一个测试。",
		}
	}
	msgs := make([]waketest.Message, count)
	for i := range msgs {
		msgs[i] = waketest.Message{Role: []string{"user", "assistant"}[i%2], Text: lines[i%len(lines)],
			Time: at - int64(count-i)*20_000}
	}
	return msgs
}

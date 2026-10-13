package demo

import (
	"fmt"
	"strings"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

// Data says what the demo holds, for whoever shows or checks it. All of it
// is made up.
type Data struct {
	Names    [3]string // desk, laptop, attic
	IDs      [3]string // their node ids
	Sessions [3]int    // how many sessions each machine has
	Long     string    // on laptop: a session of many pages
	Code     string    // on desk: code blocks, long lines, a folded message
	Kept     string    // on attic: read while it was online, so a copy is kept
	Unread   string    // on attic: never read, so it cannot be shown; its project has a long name
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
	d := Data{Sessions: [3]int{59, 44, 12}}
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
	ordinary(laptop, "laptop", d.Sessions[1]-1, func(i int) int64 { return recent(i) + 9 })
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

// twoMachines starts the nodes "desk" and "laptop", each with a fake Wake
// index, connected and with each other's sessions read.
func twoMachines(t *testing.T) (desk, laptop string, deskWake *waketest.DB) {
	t.Helper()
	desk, laptop = t.TempDir(), t.TempDir()
	h8c(t, desk, "name", "desk")
	h8c(t, laptop, "name", "laptop")
	keyOf := func(dir string) string {
		_, key := nodeKey(t, dir)
		return key
	}
	h8c(t, desk, "trust", "add", keyOf(laptop))
	h8c(t, laptop, "trust", "add", keyOf(desk))

	deskWake = waketest.New(t, t.TempDir())
	laptopWake := waketest.New(t, t.TempDir())
	wakeDBs[desk], wakeDBs[laptop] = deskWake.Path, laptopWake.Path
	now := time.Now().UnixMilli()
	deskWake.Put(waketest.Session{Key: "claude:a1", Agent: "claude", Title: "Fix the parser", Project: "parser",
		Path: "/home/alice/src/parser", Branch: "main", Model: "model-x", UpdatedAt: now - 60_000},
		waketest.Message{Role: "user", Text: "the parser drops the last token", Time: now - 90_000},
		waketest.Message{Role: "assistant", Text: "found it: an off-by-one in the loop"},
		waketest.Message{Role: "user", Text: "thanks, ship it"})
	deskWake.Put(waketest.Session{Key: "codex:b2", Agent: "codex", Title: "Write\nthe docs", Project: "docs",
		UpdatedAt: now - 10*24*3600_000})
	laptopWake.Put(waketest.Session{Key: "claude:d4", Agent: "claude", Title: "Plan the trip", Project: "notes",
		UpdatedAt: now - 3600_000}, waketest.Message{Role: "user", Text: "tickets"})

	var deskLog, laptopLog bytes.Buffer
	a := startUp(t, desk, &deskLog)
	b := startUp(t, laptop, &laptopLog, listenAddr(t, &deskLog))
	ctx := context.Background()
	b.Round(ctx)
	a.Round(ctx)
	a.Sessions().Round(ctx)
	b.Sessions().Round(ctx)
	a.Sessions().Round(ctx)
	return desk, laptop, deskWake
}

func TestSessionsCommand(t *testing.T) {
	desk, laptop, _ := twoMachines(t)

	out := h8c(t, laptop, "sessions")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 7 || !strings.HasPrefix(lines[0], "SESSION") {
		t.Fatalf("h8c sessions:\n%s", out)
	}
	for i, want := range [][]string{
		{"desk/claude:a1", "desk", "claude", "parser", "3", "Fix the parser"},
		{"laptop/claude:d4", "laptop", "claude", "notes", "1", "Plan the trip"},
		{"desk/codex:b2", "desk", "codex", "docs", "0", "Write the docs"},
	} {
		line := lines[i+1]
		for _, field := range want {
			if !strings.Contains(line, field) {
				t.Errorf("line %d lacks %q: %s", i+1, field, line)
			}
		}
		if !strings.HasPrefix(line, want[0]+" ") {
			t.Errorf("line %d does not start with the session's name: %s", i+1, line)
		}
	}
	if !strings.HasPrefix(lines[5], "laptop: 1 session, this node") ||
		!strings.HasPrefix(lines[6], "desk: 2 sessions, online, read 20") {
		t.Errorf("node lines:\n%s\n%s", lines[5], lines[6])
	}

	// Filters, with the flags wherever the user puts them.
	for args, want := range map[string]string{
		"--node desk":                  "desk/claude:a1 desk/codex:b2",
		"--agent codex":                "desk/codex:b2",
		"--project notes":              "laptop/claude:d4",
		"--since 2h":                   "desk/claude:a1 laptop/claude:d4",
		"--since 2026-01-01":           "desk/claude:a1 laptop/claude:d4 desk/codex:b2",
		"--limit 1":                    "desk/claude:a1",
		"parser":                       "desk/claude:a1",
		"the --node desk":              "desk/claude:a1 desk/codex:b2",
		"--node desk the --limit 1":    "desk/claude:a1",
		"the --node laptop --since 1d": "laptop/claude:d4",
		"nothing-like-this":            "",
	} {
		var got []string
		for _, line := range strings.Split(h8c(t, laptop, append([]string{"sessions"}, strings.Fields(args)...)...), "\n")[1:] {
			if line == "" {
				break
			}
			got = append(got, strings.Fields(line)[0])
		}
		if strings.Join(got, " ") != want {
			t.Errorf("h8c sessions %s: %v, want %s", args, got, want)
		}
	}
	if err := h8cFails(t, laptop, "sessions", "--since", "soon"); !strings.Contains(err.Error(), "--since") {
		t.Errorf("bad --since: %v", err)
	}
	if err := h8cFails(t, laptop, "sessions", "--node", "nobody"); !strings.Contains(err.Error(), `no node "nobody"`) {
		t.Errorf("unknown node: %v", err)
	}
	if got := h8c(t, desk, "sessions", "--node", "laptop"); !strings.Contains(got, "laptop/claude:d4") {
		t.Errorf("desk does not list the laptop's session:\n%s", got)
	}
}

// The JSON is for programs: these are the fields they can count on.
func TestSessionsJSON(t *testing.T) {
	_, laptop, _ := twoMachines(t)
	var got struct {
		Nodes    []map[string]any `json:"nodes"`
		Sessions []map[string]any `json:"sessions"`
	}
	raw := h8c(t, laptop, "sessions", "--json")
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if len(got.Nodes) != 2 || len(got.Sessions) != 3 {
		t.Fatalf("%d nodes, %d sessions:\n%s", len(got.Nodes), len(got.Sessions), raw)
	}
	fields := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	// Fields may be added over time; these must stay.
	has := func(what string, m map[string]any, want ...string) {
		t.Helper()
		for _, field := range want {
			if !slices.Contains(fields(m), field) {
				t.Errorf("%s lacks %q; it has %v", what, field, fields(m))
			}
		}
	}
	has("this node", got.Nodes[0], "id", "name", "online", "self", "sessions", "protocol", "version")
	has("the other node", got.Nodes[1], "id", "name", "online", "sessions", "synced_at", "protocol", "version")
	s := got.Sessions[0]
	has("a session", s, "agent", "branch", "created_at", "key", "messages", "model", "node",
		"node_id", "online", "project", "project_path", "ref", "title", "updated_at")
	if got.Nodes[1]["protocol"] != float64(node.Protocol) || got.Nodes[1]["incompatible"] != nil {
		t.Errorf("the other node = %v", got.Nodes[1])
	}
	for field, want := range map[string]any{"ref": "desk/claude:a1", "node": "desk", "online": true, "key": "claude:a1",
		"agent": "claude", "title": "Fix the parser", "project": "parser", "project_path": "/home/alice/src/parser",
		"branch": "main", "model": "model-x", "messages": float64(3), "node_id": got.Nodes[1]["id"]} {
		if s[field] != want {
			t.Errorf("%s = %v, want %v", field, s[field], want)
		}
	}
	if _, err := time.Parse(time.RFC3339, s["updated_at"].(string)); err != nil {
		t.Errorf("updated_at = %v: %v", s["updated_at"], err)
	}
}

func TestShowCommand(t *testing.T) {
	desk, laptop, deskWake := twoMachines(t)

	for _, dir := range []string{laptop, desk} { // over the link, and on the machine itself
		out := h8c(t, dir, "show", "desk/claude:a1")
		for _, want := range []string{
			"Fix the parser\n", "claude on desk, /home/alice/src/parser, 3 messages, updated 20",
			"--- #1 user  20", "the parser drops the last token",
			"--- #2 assistant\nfound it: an off-by-one in the loop", "--- #3 user\nthanks, ship it",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("h8c show lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "more:") || strings.Contains(out, "note:") {
			t.Errorf("unexpected notes:\n%s", out)
		}
	}

	// In pages, following what the output says.
	first := h8c(t, laptop, "show", "desk/claude:a1", "--limit", "2")
	_, more, ok := strings.Cut(first, "\nmore: h8c show 'desk/claude:a1' ")
	if !ok || strings.Contains(first, "thanks, ship it") {
		t.Fatalf("first page:\n%s", first)
	}
	second := h8c(t, laptop, append([]string{"show", "desk/claude:a1"}, strings.Fields(more)...)...)
	if !strings.Contains(second, "--- #3 user\nthanks, ship it") || strings.Contains(second, "off-by-one") ||
		strings.Contains(second, "more:") {
		t.Errorf("second page:\n%s", second)
	}

	var r struct {
		Session  map[string]any   `json:"session"`
		Messages []map[string]any `json:"messages"`
		Next     int64            `json:"next"`
		From     string           `json:"from"`
		CachedAt string           `json:"cached_at"`
	}
	raw := h8c(t, laptop, "show", "--json", "--limit", "1", "desk/claude:a1")
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if r.From != "cache" || r.CachedAt == "" || r.Next == 0 || len(r.Messages) != 1 || r.Session["ref"] != "desk/claude:a1" ||
		r.Messages[0]["text"] != "the parser drops the last token" || r.Messages[0]["role"] != "user" ||
		r.Messages[0]["seq"] != float64(1) || r.Messages[0]["id"] != float64(r.Next) {
		t.Errorf("show --json:\n%s", raw)
	}

	// The session grows on desk; after the next round the laptop reads the new end.
	deskWake.Append("claude:a1", waketest.Message{Role: "assistant", Text: "shipped"})
	for _, dir := range []string{desk, laptop} {
		running[dir].Sessions().Round(context.Background())
	}
	if out := h8c(t, laptop, "show", "desk/claude:a1"); !strings.Contains(out, "--- #4 assistant\nshipped") ||
		!strings.Contains(out, "4 messages") {
		t.Errorf("after the session grew:\n%s", out)
	}

	for args, want := range map[string]string{
		"nonsense":           "is not NODE/KEY",
		"":                   "want: h8c show NODE/KEY",
		"desk/claude:zzz":    "no such session",
		"nobody/claude:a1":   `no node "nobody"`,
		"desk/claude:a1 x y": "want: h8c show NODE/KEY",
	} {
		if err := h8cFails(t, laptop, append([]string{"show"}, strings.Fields(args)...)...); !strings.Contains(err.Error(), want) {
			t.Errorf("h8c show %s: %v, want %q", args, err, want)
		}
	}
}

func TestSessionCommandsNeedARunningNode(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"sessions"}, {"sessions", "--json"}, {"show", "desk/claude:a1"}} {
		if err := h8cFails(t, dir, args...); !strings.Contains(err.Error(), "not running") ||
			!strings.Contains(err.Error(), "h8c up") {
			t.Errorf("h8c %v: %v", args, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("asking a node that is not there left files: %v", entries)
	}
}

func TestSince(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local)
	for text, want := range map[string]time.Time{
		"36h":        now.Add(-36 * time.Hour),
		"90m":        now.Add(-90 * time.Minute),
		"7d":         now.AddDate(0, 0, -7),
		"2026-10-01": time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
	} {
		if got, err := since(text, now); err != nil || !got.Equal(want) {
			t.Errorf("since(%q) = %v, %v; want %v", text, got, err, want)
		}
	}
	if got, err := since("2026-10-01T08:00:00Z", now); err != nil || !got.Equal(time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("RFC 3339: %v, %v", got, err)
	}
	for _, text := range []string{"soon", "d", "", "7 days"} {
		if _, err := since(text, now); err == nil {
			t.Errorf("since(%q) succeeded", text)
		}
	}
}

// What h8c up makes of the Wake flags.
func TestUpFindsTheIndexAndTheCLI(t *testing.T) {
	ctx := context.Background()

	// No index and no wake-cli: nothing to share, and the node says so.
	// An index that is built later is found without a restart.
	t.Run("neither index nor wake-cli", func(t *testing.T) {
		dir := t.TempDir()
		later := t.TempDir()
		wakeDBs[dir] = filepath.Join(later, "wake.db")
		var logs bytes.Buffer
		n := startUp(t, dir, &logs)
		n.Sessions().Round(ctx)
		if !strings.Contains(logs.String(), "sessions of this machine are not shared") ||
			!strings.Contains(logs.String(), "no Wake index at") {
			t.Errorf("the node did not say that it has no index:\n%s", &logs)
		}
		if got := h8c(t, dir, "sessions"); !strings.Contains(got, ": 0 sessions, this node") {
			t.Errorf("h8c sessions:\n%s", got)
		}

		db := waketest.New(t, later)
		db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", Title: "Indexed later", UpdatedAt: 1})
		n.Sessions().Round(ctx)
		if got := h8c(t, dir, "sessions"); !strings.Contains(got, "Indexed later") ||
			!strings.Contains(got, ": 1 session, this node") {
			t.Errorf("h8c sessions after the index appeared:\n%s", got)
		}
		if runtime.GOOS != "windows" {
			if st, err := os.Stat(filepath.Join(dir, node.SessionsFile)); err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("session store: %v, %v", st, err)
			}
		}
	})

	t.Run("wake-cli builds a missing index", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("the fake wake-cli is a shell script")
		}
		dir := t.TempDir()
		tools := t.TempDir()
		cli, calls := filepath.Join(tools, "wake-cli"), filepath.Join(tools, "calls")
		script := "#!/bin/sh\necho \"$@\" >> " + calls + "\necho 'Indexed 0 sessions.'\n"
		if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(tools, "wake.db")
		wakeDBs[dir] = db
		log := slogTo(new(bytes.Buffer))
		n, release, err := openNode(ctx, dir, upOptions{listen: "127.0.0.1:0", discover: "none", wakeDB: db, wakeCLI: cli}, log)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		defer n.Close()
		n.Start()
		n.Sessions().Round(ctx)
		raw, _ := os.ReadFile(calls)
		if string(raw) != "--db "+db+" index\n" {
			t.Errorf("wake-cli was called with %q", raw)
		}
	})
}

// The hint for the next page is a command to paste. Whatever is in a
// session's name, a shell takes the quoted name as that one argument.
func TestQuotedNameSurvivesAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	for _, name := range []string{
		"desk/claude:a1",
		"living room box/claude:a1",
		"desk/claude:`touch pwned`",
		"desk/claude:`date`",
		"desk/first\nsecond",
		"desk/claude:$(touch pwned)",
		"desk/it's \"quoted\"",
		"desk/two\nlines; rm -rf x",
		"desk/back\\slash and *?[glob]",
	} {
		dir := t.TempDir()
		cmd := exec.Command("sh", "-c", "printf %s "+quoteArg(name))
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil || string(out) != name {
			t.Errorf("%q came out of the shell as %q (%v)", name, out, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%q made the shell run something: %v", name, entries)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	out := h8c(t, t.TempDir(), "version")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// A test binary carries no version; a built one says more in the same places.
	if len(lines) != 3 || lines[0] != "h8c devel" || lines[1] != "commit unknown" || lines[2] != "protocol 1" {
		t.Errorf("h8c version:\n%s", out)
	}
}

func TestPeersShowsWhatEachPeerRuns(t *testing.T) {
	desk, laptop, _ := twoMachines(t)

	out := h8c(t, laptop, "peers")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "BUILD") || !strings.Contains(lines[1], "devel, protocol 1") ||
		strings.Contains(lines[1], "incompatible") || !strings.HasPrefix(lines[1], "desk ") {
		t.Errorf("h8c peers:\n%s", out)
	}

	var got struct {
		Self  map[string]any   `json:"self"`
		Peers []map[string]any `json:"peers"`
	}
	raw := h8c(t, laptop, "peers", "--json")
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if got.Self["name"] != "laptop" || got.Self["protocol"] != float64(1) || got.Self["version"] != "devel" {
		t.Errorf("self = %v", got.Self)
	}
	if len(got.Peers) != 1 {
		t.Fatalf("peers:\n%s", raw)
	}
	p := got.Peers[0]
	for field, want := range map[string]any{"name": "desk", "online": true, "protocol": float64(1),
		"version": "devel", "compatible": true} {
		if p[field] != want {
			t.Errorf("peer %s = %v, want %v\n%s", field, p[field], want, raw)
		}
	}
	for _, field := range []string{"id", "key", "addrs", "sources", "last_seen"} {
		if p[field] == nil {
			t.Errorf("peer lacks %q:\n%s", field, raw)
		}
	}
	_ = desk
}

func TestBuildColumn(t *testing.T) {
	for _, tc := range []struct {
		version      string
		speaks, ours int
		want         string
	}{
		{"v0.3.1", 1, 1, "v0.3.1, protocol 1"},
		{"v0.0.0-20261013104500-0123456789ab+dirty", 1, 1, "0123456, protocol 1"}, // a build from a checkout, by its commit
		{"", 1, 1, "unknown, protocol 1"},                                         // a build from before nodes said what they run
		{"v2.0.0", 2, 1, "v2.0.0, protocol 2 (incompatible: this node speaks 1)"},
	} {
		if got := build(tc.version, tc.speaks, tc.ours); got != tc.want {
			t.Errorf("build(%q, %d, %d) = %q, want %q", tc.version, tc.speaks, tc.ours, got, tc.want)
		}
	}
	// And the same in the lines of h8c sessions.
	for n, want := range map[*sessions.Node]string{
		{Online: true, Incompatible: true, Protocol: 2}: "protocol incompatible (it speaks 2, this node 1), never read",
		{Self: true}: "this node",
	} {
		if got := nodeState(*n, 1); got != want {
			t.Errorf("nodeState(%+v) = %q, want %q", *n, got, want)
		}
	}
}

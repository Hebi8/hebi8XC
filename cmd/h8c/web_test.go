package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

// startWeb starts a node as h8c up --web addr does.
func startWeb(t *testing.T, dir, addr string, logs *bytes.Buffer) *node.Node {
	t.Helper()
	if wakeDBs[dir] == "" {
		wakeDBs[dir] = filepath.Join(dir, "no-wake.db")
	}
	n, release, err := openNode(context.Background(), dir, upOptions{listen: "127.0.0.1:0", discover: "none",
		wakeDB: wakeDBs[dir], web: addr}, slogTo(logs))
	if err != nil {
		t.Fatal(err)
	}
	n.Start()
	t.Cleanup(func() {
		n.Close()
		release()
	})
	return n
}

// h8c web prints a link. Opening it leaves the browser with a cookie and at
// an address without the token; from then on the page and its data answer.
func TestWebCommandOpensTheInterface(t *testing.T) {
	dir := t.TempDir()
	db := waketest.New(t, t.TempDir())
	wakeDBs[dir] = db.Path
	db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", Title: "Fix the parser", UpdatedAt: 1},
		waketest.Message{Role: "user", Text: "hello"})
	var logs bytes.Buffer
	n := startWeb(t, dir, "127.0.0.1:0", &logs)
	n.Sessions().Round(context.Background())

	link := strings.TrimSpace(h8c(t, dir, "web"))
	m := regexp.MustCompile(`^http://127\.0\.0\.1:(\d+)/\?token=([0-9a-f]{64})$`).FindStringSubmatch(link)
	if m == nil {
		t.Fatalf("h8c web printed %q", link)
	}
	token := m[2]
	if again := strings.TrimSpace(h8c(t, dir, "web")); again != link {
		t.Errorf("h8c web printed another link the second time: %q", again)
	}
	if strings.Contains(logs.String(), token) {
		t.Error("the token is in the log")
	}
	if !strings.Contains(logs.String(), "web=127.0.0.1:"+m[1]) {
		t.Errorf("the log does not say where the web interface is:\n%s", &logs)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, node.WebTokenFile))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("token file: %v, %v", st, err)
		}
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || len(resp.Cookies()) != 1 {
		t.Fatalf("opening the link: %d, Location %q, cookies %v", resp.StatusCode, resp.Header.Get("Location"), resp.Cookies())
	}
	base := "http://127.0.0.1:" + m[1]
	fetch := func(path string, withCookie bool) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if withCookie {
			req.AddCookie(resp.Cookies()[0])
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(body)
	}
	if code, body := fetch("/", true); code != 200 || !strings.Contains(body, "/app.js") {
		t.Errorf("the page: %d", code)
	}
	if code, body := fetch("/api/v1/sessions", true); code != 200 || !strings.Contains(body, "Fix the parser") {
		t.Errorf("the sessions: %d %s", code, body)
	}
	if code, body := fetch("/api/v1/sessions", false); code != 401 || strings.Contains(body, "Fix the parser") {
		t.Errorf("the sessions without the cookie: %d %s", code, body)
	}

	// The token outlives a restart: the cookie in the browser keeps working.
	n.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, node.WebTokenFile))
	if strings.TrimSpace(string(raw)) != token {
		t.Error("the token file changed")
	}
}

func TestWebOnlyListensOnThisMachine(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:4893", ":4893", "192.168.1.20:4893", "100.64.0.2:4893", "example.com:4893", "127.0.0.2:4893"} {
		dir := t.TempDir()
		err := h8cFails(t, dir, "up", "--listen", "127.0.0.1:0", "--discover", "none", "--web", addr)
		if !strings.Contains(err.Error(), "--web") {
			t.Errorf("--web %s: %v", addr, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("--web %s: the refused node left files: %v", addr, entries)
		}
	}
}

// The port of the page is taken. The node runs all the same, says that it
// has no web interface, and so does h8c web.
func TestNodeRunsWithoutItsWebPort(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	dir := t.TempDir()
	var logs bytes.Buffer
	n := startWeb(t, dir, taken.Addr().String(), &logs)
	if !strings.Contains(logs.String(), "the web interface is off") || !strings.Contains(logs.String(), "node up") {
		t.Errorf("log:\n%s", &logs)
	}
	n.Sessions().Round(context.Background())
	if got := h8c(t, dir, "sessions"); !strings.Contains(got, "this node") {
		t.Errorf("h8c sessions:\n%s", got)
	}
	if got := h8c(t, dir, "peers"); !strings.HasPrefix(got, "NAME") {
		t.Errorf("h8c peers:\n%s", got)
	}
	if err := h8cFails(t, dir, "web"); !strings.Contains(err.Error(), "web interface of this node is off") {
		t.Errorf("h8c web: %v", err)
	}
}

func TestWebCommandWhenThereIsNothingToOpen(t *testing.T) {
	stopped := t.TempDir()
	if err := h8cFails(t, stopped, "web"); !strings.Contains(err.Error(), "not running") {
		t.Errorf("h8c web without a node: %v", err)
	}

	for _, off := range []string{"none", ""} {
		dir := t.TempDir()
		var logs bytes.Buffer
		startWeb(t, dir, off, &logs)
		if err := h8cFails(t, dir, "web"); !strings.Contains(err.Error(), "--web none") {
			t.Errorf("h8c web with --web %q: %v", off, err)
		}
		if _, err := os.Stat(filepath.Join(dir, node.WebTokenFile)); err == nil {
			t.Errorf("--web %q made a token", off)
		}
	}
}

func TestSessionsCommandContinuesAListing(t *testing.T) {
	_, laptop, _ := twoMachines(t)
	first := h8c(t, laptop, "sessions", "--limit", "2")
	_, cursor, ok := strings.Cut(first, "\nmore: add --after ")
	if !ok {
		t.Fatalf("no hint for the next page:\n%s", first)
	}
	rest := h8c(t, laptop, "sessions", "--limit", "2", "--after", strings.TrimSpace(cursor))
	if strings.Contains(rest, "more:") || !strings.Contains(rest, "desk/codex:b2") || strings.Contains(rest, "desk/claude:a1") {
		t.Errorf("the next page:\n%s", rest)
	}
	if err := h8cFails(t, laptop, "sessions", "--after", "nonsense"); !strings.Contains(err.Error(), "after is not") {
		t.Errorf("a made-up --after: %v", err)
	}
}

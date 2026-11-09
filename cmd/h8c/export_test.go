package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/control"
	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

func TestPhase2Commands(t *testing.T) {
	dir := t.TempDir()
	d := waketest.New(t, t.TempDir())
	const key = "codex:example"
	d.Put(waketest.Session{Key: key, Agent: "codex", UpdatedAt: 1000}, waketest.Message{Role: "assistant", Text: "Done", Time: 1000})
	d.Structure(key, 1, sessions.Message{Usage: &sessions.Usage{Input: 7, Output: 5}, Calls: []sessions.Call{{Call: "edit-1", Tool: "edit", Files: []sessions.File{{Path: "/home/alice/src/parser.go", Op: "change"}}}}})
	x := wake.Open(d.Path)
	defer x.Close()
	store, err := sessions.OpenStore(filepath.Join(dir, "sessions.db"), func() time.Time { return time.UnixMilli(2000) })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := &sessions.Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: store, Source: x, Self: sessions.Peer{ID: "desk", Name: "desk"}, Peers: func() []sessions.Peer { return nil }}
	if err := svc.RefreshLocal(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	svc.ControlRoutes(mux)
	ln, err := control.Listen(filepath.Join(dir, node.ControlSocket))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run(append([]string{"--data-dir", dir}, args...), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	for _, args := range [][]string{{"sessions", "--json"}, {"show", "desk/" + key, "--json"}, {"show", "desk/" + key}} {
		out := run(args...)
		if !strings.Contains(out, "tokens") || !strings.Contains(out, "/home/alice/src/parser.go") {
			t.Fatal("metadata missing", out)
		}
	}
	if out := run("show", "desk/"+key); !strings.Contains(out, "    file: change /home/alice/src/parser.go") {
		t.Fatal(out)
	}
	var page sessions.ExportPage
	if err := json.Unmarshal([]byte(run("export", "--since", "0", "--limit", "1", "--json")), &page); err != nil {
		t.Fatal(err)
	}
	if page.Epoch == "" || len(page.Items) != 1 || page.Items[0].Session.Tokens == nil || len(page.Items[0].Session.Files) != 1 || !page.More {
		t.Fatal(page)
	}
	first := page.Next
	var restarted sessions.ExportPage
	if err := json.Unmarshal([]byte(run("export", "--epoch", "old-epoch", "--after", "999", "--ack", "999", "--json")), &restarted); err != nil || len(restarted.Items) != 2 || restarted.Next != 2 {
		t.Fatal("CLI epoch protection", err, restarted)
	}
	if err := json.Unmarshal([]byte(run("export", "--after", strconv.FormatInt(first, 10), "--ack", strconv.FormatInt(first, 10), "--since", "0", "--json")), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Message.Usage.Input != 7 || len(page.Items[0].Message.Calls[0].Files) != 1 || page.More {
		t.Fatal(page)
	}
	for _, routes := range []func(*http.ServeMux){svc.PeerRoutes, svc.ReadRoutes} {
		m := http.NewServeMux()
		routes(m)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/export", nil))
		if w.Code != http.StatusNotFound {
			t.Fatal("export exposed outside control socket", w.Code)
		}
	}
}

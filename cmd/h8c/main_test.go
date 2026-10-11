package main

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/datadir"
)

func h8c(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(append([]string{"--data-dir", dir}, args...), &out); err != nil {
		t.Fatalf("h8c %v: %v", args, err)
	}
	return out.String()
}

// The pairing flow from the README, minus the running node: the key line
// printed by "id" on one machine is what "trust add" takes on the other.
func TestIDAndTrustCommands(t *testing.T) {
	devnuc, n100 := t.TempDir(), t.TempDir()

	id := h8c(t, devnuc, "id")
	if id != h8c(t, devnuc, "id") {
		t.Fatal("h8c id is not stable")
	}
	var key string
	for _, line := range strings.Split(id, "\n") {
		if rest, ok := strings.CutPrefix(line, "key "); ok {
			key = strings.TrimSpace(rest)
		}
	}
	if !strings.HasPrefix(key, "ssh-ed25519 ") {
		t.Fatalf("no key line in:\n%s", id)
	}

	// Unquoted, the key line arrives as several arguments.
	h8c(t, n100, append([]string{"trust", "add"}, strings.Fields(key)...)...)
	if got := h8c(t, n100, "trust", "add", key); got != "already trusted\n" {
		t.Errorf("second add printed %q", got)
	}
	if got := h8c(t, n100, "trust", "list"); got != key+"\n" {
		t.Errorf("trust list = %q, want %q", got, key+"\n")
	}

	comment := strings.Fields(key)[2]
	h8c(t, n100, "trust", "remove", comment)
	if got := h8c(t, n100, "trust", "list"); got != "" {
		t.Errorf("trust list after remove = %q", got)
	}

	var out bytes.Buffer
	if err := run([]string{"--data-dir", n100, "trust", "remove", comment}, &out); err == nil {
		t.Error("removing a key that is not there succeeded")
	}
}

func TestPeersWithoutARunningNode(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"--data-dir", t.TempDir(), "peers"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %v, want a hint that the node is not running", err)
	}
}

func TestDataDirectoryIsTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	h8c(t, dir, "id")
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("data directory mode = %o after h8c id, want 700", perm)
	}
}

// A second node on the same data directory must stop before it touches the
// first one's control socket.
func TestUpRefusesALockedDataDirectory(t *testing.T) {
	dir := t.TempDir()
	unlock, err := datadir.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	var out bytes.Buffer
	err = run([]string{"--data-dir", dir, "up", "--listen", "127.0.0.1:0", "--discover", "none"}, &out)
	if !errors.Is(err, datadir.ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the refused node left files behind: %v", entries)
	}
}

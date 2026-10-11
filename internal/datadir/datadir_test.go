package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnsureTightensAnExistingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { // Mkdir is subject to the umask
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Join(t.TempDir(), "new", "data")} {
		if err := Ensure(d); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 700", d, perm)
		}
	}
}

func TestLockIsExclusiveUntilReleased(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock: err = %v, want ErrLocked", err)
	}
	if other, err := Lock(t.TempDir()); err != nil {
		t.Errorf("another directory is locked too: %v", err)
	} else {
		other()
	}

	unlock()
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	again()
}

func TestWriteFileReplacesAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted_keys")
	for _, content := range []string{"first\n", "second\n"} {
		if err := WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Errorf("content = %q, want %q", got, content)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the file", len(entries))
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o644 {
			t.Errorf("mode = %o, want 644", perm)
		}
	}
}

// A write that cannot complete must leave the old file as it was.
func TestWriteFileKeepsTheOldFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")
	if err := WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The target is a directory, so the final rename fails.
	target := filepath.Join(dir, "taken")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(target, []byte("new"), 0o600); err == nil {
		t.Fatal("WriteFile replaced a non-empty directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("failed write left a temporary file: %v", entries)
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Errorf("unrelated file changed to %q", got)
	}
}

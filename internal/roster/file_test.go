package roster

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hebi8xc/internal/sshkey"
)

func newKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestFileAddListRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusted_keys")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := newKey(t), newKey(t)
	if Trusts(f, "me", a) {
		t.Fatal("empty roster trusts a key")
	}

	for _, add := range []struct {
		key     ed25519.PublicKey
		comment string
		want    bool
	}{{a, "devnuc", true}, {b, "n100", true}, {a, "again", false}} {
		added, err := f.Add(add.key, add.comment)
		if err != nil {
			t.Fatal(err)
		}
		if added != add.want {
			t.Errorf("Add(%s) = %v, want %v", add.comment, added, add.want)
		}
	}
	if !Trusts(f, "me", a) || !Trusts(f, "me", b) {
		t.Fatal("added keys are not trusted")
	}

	// A second handle on the same file sees what the first one wrote.
	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.Entries()
	if len(entries) != 2 || entries[0].Comment != "devnuc" || entries[1].Comment != "n100" {
		t.Fatalf("entries = %+v", entries)
	}

	for _, match := range []string{"devnuc", sshkey.Fingerprint(b)} {
		n, err := f.Remove(match)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("Remove(%q) = %d, want 1", match, n)
		}
	}
	if Trusts(f, "me", a) || Trusts(f, "me", b) {
		t.Error("removed keys are still trusted")
	}
	if n, _ := f.Remove("nobody"); n != 0 {
		t.Errorf("Remove(nobody) = %d", n)
	}
}

func TestFileRefreshSeesOutsideEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusted_keys")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	content := "# my machines\n\n" + sshkey.Marshal(key) + " laptop\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if Trusts(f, "me", key) {
		t.Fatal("Keys read the file without a Refresh")
	}
	if err := f.Refresh(context.Background(), "me"); err != nil {
		t.Fatal(err)
	}
	if !Trusts(f, "me", key) {
		t.Fatal("Refresh did not pick up the edit")
	}

	// Removing through the roster keeps the hand-written comment line.
	if _, err := f.Remove("laptop"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# my machines\n") {
		t.Errorf("comment line lost: %q", raw)
	}
}

func TestFileRefreshKeepsOldKeysOnBadLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusted_keys")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	if _, err := f.Add(key, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = f.Refresh(context.Background(), "me")
	if err == nil || !strings.Contains(err.Error(), "trusted_keys:1") {
		t.Fatalf("Refresh error = %v, want one naming line 1", err)
	}
	if !Trusts(f, "me", key) {
		t.Error("a failed Refresh dropped the cached keys")
	}
}

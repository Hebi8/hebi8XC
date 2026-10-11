package identity

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadIsStableAcrossRestarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.Name == "" {
		t.Fatalf("incomplete identity: %+v", first)
	}

	second, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Name != first.Name || !second.Key.Equal(first.Key) {
		t.Errorf("identity changed on reload: %+v != %+v", second, first)
	}
}

func TestKeyFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
}

func TestDistinctDirsGetDistinctIdentities(t *testing.T) {
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.Key.Equal(b.Key) {
		t.Error("two fresh identities share an id or key")
	}
}

// A lost key must not be silently replaced: the node would come back as a
// stranger to every roster that lists it.
func TestLoadRefusesHalfAnIdentity(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, keyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("Load regenerated an identity whose key file was missing")
	}
}

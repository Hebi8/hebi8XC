package roster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/github/githubtest"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

const (
	aliceID = 4242
	alice   = "github:4242"
)

// An RSA key as GitHub lists it; nodes only have Ed25519 keys.
const rsaKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7"

type githubFixture struct {
	t    *testing.T
	fake *githubtest.Server
	path string
	now  time.Time
	*GitHub
}

func newGitHub(t *testing.T) *githubFixture {
	t.Helper()
	f := &githubFixture{
		t:    t,
		fake: githubtest.New(t),
		path: filepath.Join(t.TempDir(), "github_keys.json"),
		now:  time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
	}
	f.fake.AddAccount(aliceID, "alice")
	f.reopen()
	return f
}

// reopen loads the roster from its cache file, as a restart does.
func (f *githubFixture) reopen() {
	f.t.Helper()
	g, err := OpenGitHub(f.path, f.fake.Client(), func() time.Time { return f.now })
	if err != nil {
		f.t.Fatal(err)
	}
	f.GitHub = g
}

func (f *githubFixture) refresh() {
	f.t.Helper()
	if err := f.Refresh(context.Background(), alice); err != nil {
		f.t.Fatal(err)
	}
}

func TestGitHubRosterTakesOnlyNodeKeys(t *testing.T) {
	f := newGitHub(t)
	node, commits, other := newKey(t), newKey(t), newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "0123456789abcdef"), sshkey.Marshal(node))
	f.fake.AddKey(aliceID, "git commit signing", sshkey.Marshal(commits))
	f.fake.AddKey(aliceID, "note about hebi8xc: laptop", sshkey.Marshal(other))
	f.fake.AddKey(aliceID, KeyTitle("old", "fedcba9876543210"), rsaKey)

	if Trusts(f, alice, node) {
		t.Fatal("a key is trusted before the first refresh")
	}
	f.refresh()

	if !Trusts(f, alice, node) {
		t.Error("the node key is not trusted")
	}
	if Trusts(f, alice, commits) || Trusts(f, alice, other) {
		t.Error("a signing key without the title prefix is trusted")
	}
	entries := f.Entries(alice)
	if len(entries) != 1 || entries[0].Comment != "hebi8xc: laptop (0123456789abcdef)" {
		t.Errorf("entries = %+v, want only the Ed25519 node key", entries)
	}
	if login, at := f.Fetched(alice); login != "alice" || !at.Equal(f.now) {
		t.Errorf("fetched %q at %v", login, at)
	}
}

// Another account's keys are never trusted, whoever asks and whatever the
// cache holds.
func TestGitHubRosterIsPerAccount(t *testing.T) {
	f := newGitHub(t)
	const bob = "github:7"
	f.fake.AddAccount(7, "bob")
	aliceKey, bobKey := newKey(t), newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(aliceKey))
	f.fake.AddKey(7, KeyTitle("laptop", "b"), sshkey.Marshal(bobKey))

	f.refresh()
	if Trusts(f, alice, bobKey) {
		t.Error("alice's roster trusts bob's key")
	}
	if Trusts(f, bob, aliceKey) || len(f.Entries(bob)) != 0 {
		t.Error("alice's cache answers for bob")
	}
	if Trusts(f, "alice", aliceKey) {
		t.Error("the cache answers for a user that is not a GitHub account")
	}
	if err := f.Refresh(context.Background(), "alice"); err == nil {
		t.Error("Refresh accepted a user that is not a GitHub account")
	}
}

func TestGitHubRosterKeepsTheCacheWhenRefreshFails(t *testing.T) {
	f := newGitHub(t)
	key := newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(key))
	f.refresh()

	for name, breakIt := range map[string]func(bool){
		"unreachable":  f.fake.SetDown,
		"rate limited": f.fake.SetRateLimited,
	} {
		breakIt(true)
		f.now = f.now.Add(2 * time.Hour)
		err := f.Refresh(context.Background(), alice)
		if err == nil || !strings.Contains(err.Error(), "using the 1 keys fetched 2h0m0s ago") {
			t.Errorf("%s: err = %v, want one that says how old the cache is", name, err)
		}
		if !Trusts(f, alice, key) {
			t.Errorf("%s: the cached key is no longer trusted", name)
		}
		breakIt(false)
		f.refresh()
	}

	// The cache also carries a node through a restart without network.
	f.fake.SetDown(true)
	f.reopen()
	if !Trusts(f, alice, key) {
		t.Error("the key is not trusted after a restart from the cache")
	}
	if err := f.Refresh(context.Background(), alice); err == nil {
		t.Error("Refresh succeeded while GitHub is down")
	}

	// Without a cache the error says so.
	fresh := newGitHub(t)
	fresh.fake.SetDown(true)
	err := fresh.Refresh(context.Background(), alice)
	if err == nil || !strings.Contains(err.Error(), "no cached keys") {
		t.Errorf("err = %v, want one that says there is no cache", err)
	}
}

func TestGitHubRosterDropsRemovedKeys(t *testing.T) {
	f := newGitHub(t)
	stays, goes := newKey(t), newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("desktop", "a"), sshkey.Marshal(stays))
	f.fake.AddKey(aliceID, KeyTitle("lost laptop", "b"), sshkey.Marshal(goes))
	f.refresh()

	f.fake.RemoveKey(aliceID, sshkey.Marshal(goes))
	if !Trusts(f, alice, goes) {
		t.Fatal("the removal took effect without a refresh")
	}
	f.refresh()
	if Trusts(f, alice, goes) || !Trusts(f, alice, stays) {
		t.Error("after the refresh the removed key should be gone and the other one kept")
	}
	f.reopen()
	if Trusts(f, alice, goes) {
		t.Error("the removed key came back from the cache file")
	}
}

// The account is found by its id. After a rename the old login may belong
// to somebody else; that account's keys must not be picked up.
func TestGitHubRosterFollowsARename(t *testing.T) {
	f := newGitHub(t)
	key, squatter := newKey(t), newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(key))
	f.refresh()

	f.fake.Rename(aliceID, "alice-new")
	f.fake.AddAccount(99, "alice")
	f.fake.AddKey(99, KeyTitle("laptop", "z"), sshkey.Marshal(squatter))
	f.refresh()

	if !Trusts(f, alice, key) {
		t.Error("the account's key is lost after the rename")
	}
	if Trusts(f, alice, squatter) {
		t.Error("the key of the account that took the old login is trusted")
	}
	if login, _ := f.Fetched(alice); login != "alice-new" {
		t.Errorf("login = %q, want the new one", login)
	}
}

func TestGitHubCacheFileIsPrivateAndWhole(t *testing.T) {
	f := newGitHub(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(newKey(t)))
	f.refresh()

	st, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("cache mode = %o, want 600", perm)
	}
	if entries, _ := os.ReadDir(filepath.Dir(f.path)); len(entries) != 1 {
		t.Errorf("refresh left temporary files: %v", entries)
	}

	// A cache that does not parse is an error, not an empty roster.
	if err := os.WriteFile(f.path, []byte(`{"account_id": 4242, "keys": [{"key": "ssh-ed25519 nope"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGitHub(f.path, f.fake.Client(), time.Now); err == nil {
		t.Error("OpenGitHub accepted a cache with a broken key")
	}
}

func TestUnionTrustsEitherRoster(t *testing.T) {
	f := newGitHub(t)
	local, err := OpenFile(filepath.Join(t.TempDir(), "trusted_keys"))
	if err != nil {
		t.Fatal(err)
	}
	onGitHub, byHand, nobody := newKey(t), newKey(t), newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(onGitHub))
	if _, err := local.Add(byHand, "offline box"); err != nil {
		t.Fatal(err)
	}

	both := Union(local, f.GitHub)
	if err := both.Refresh(context.Background(), alice); err != nil {
		t.Fatal(err)
	}
	if !Trusts(both, alice, onGitHub) || !Trusts(both, alice, byHand) {
		t.Error("the union does not trust a key of one of its rosters")
	}
	if Trusts(both, alice, nobody) {
		t.Error("the union trusts a key nobody listed")
	}

	// One roster failing does not hide the other.
	f.fake.SetDown(true)
	if err := both.Refresh(context.Background(), alice); err == nil {
		t.Error("the union swallowed a refresh failure")
	}
	if !Trusts(both, alice, onGitHub) || !Trusts(both, alice, byHand) {
		t.Error("a failed refresh changed what the union trusts")
	}
}

package roster

import (
	"context"
	"fmt"
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
	g, err := OpenGitHub(f.path, f.fake.Client(), "alice", func() time.Time { return f.now })
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

}

// A cache file that cannot be read as a cache must not stop anything: it
// counts as no cache, the reason is available, and a refresh repairs it.
func TestGitHubRosterRecoversFromABrokenCache(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":   "{{{",
		"broken key": `{"account_id": 4242, "login": "alice", "keys": [{"key": "ssh-ed25519 nope", "title": "hebi8xc: x (y)"}]}`,
	} {
		f := newGitHub(t)
		key := newKey(t)
		f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(key))
		if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		f.reopen()
		if err := f.Discarded(); err == nil || !strings.Contains(err.Error(), f.path) {
			t.Errorf("%s: Discarded = %v, want the reason and the file", name, err)
		}
		if len(f.Entries(alice)) != 0 {
			t.Errorf("%s: keys were taken from a broken cache", name)
		}

		f.refresh()
		f.reopen()
		if f.Discarded() != nil || !Trusts(f, alice, key) {
			t.Errorf("%s: the refresh did not repair the cache", name)
		}
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

func (f *githubFixture) requests() (lists, lookups int) {
	return f.fake.Requests("GET /users/{login}/ssh_signing_keys"), f.fake.Requests("GET /user/{id}")
}

// cacheFile returns the cache as it is on disk.
func (f *githubFixture) cacheFile() string {
	f.t.Helper()
	raw, err := os.ReadFile(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}

// squat renames alice's account and lets another account take the login
// "alice" and register key as a node key of its own.
func (f *githubFixture) squat(newLogin, key string) {
	f.fake.Rename(aliceID, newLogin)
	f.fake.AddAccount(99, "alice")
	f.fake.AddKey(99, KeyTitle("laptop", "z"), key)
}

func TestGitHubRefreshIsTwoRequests(t *testing.T) {
	f := newGitHub(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(newKey(t)))
	for i := 1; i <= 3; i++ { // the first uses the login from h8c login, the others the cached one
		f.refresh()
		if lists, lookups := f.requests(); lists != i || lookups != i {
			t.Fatalf("after %d refreshes: %d key lists and %d id lookups, want %d of each", i, lists, lookups, i)
		}
	}
}

// The login changes hands at the worst moments of a refresh. Whenever the
// keys were read under a login the account no longer has, they are thrown
// away: the other account's key reaches neither the roster nor the cache.
func TestGitHubRosterNeverTakesKeysOfTheOldLogin(t *testing.T) {
	for name, moment := range map[string]string{
		"renamed before the keys are listed":            "GET /users/{login}/ssh_signing_keys",
		"renamed between listing the keys and the id":   "GET /user/{id}",
		"renamed before the keys, on the first refresh": "",
	} {
		f := newGitHub(t)
		own := newKey(t)
		f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(own))

		squatterKey := newKey(t)
		squatter := sshkey.Marshal(squatterKey)
		if moment == "" {
			f.squat("alice-new", squatter) // the roster only has the login from h8c login
		} else {
			f.refresh()
			f.fake.Before(moment, func() { f.squat("alice-new", squatter) })
		}
		listsBefore, lookupsBefore := f.requests()
		f.refresh()

		if Trusts(f, alice, squatterKey) || strings.Contains(f.cacheFile(), squatter) {
			t.Errorf("%s: the key of the account that took the login is trusted or cached", name)
		}
		if !Trusts(f, alice, own) {
			t.Errorf("%s: the account's own key was lost", name)
		}
		if login, _ := f.Fetched(alice); login != "alice-new" {
			t.Errorf("%s: login = %q, want alice-new", name, login)
		}
		// The first list is discarded: two more requests, four in all.
		if lists, lookups := f.requests(); lists-listsBefore != 2 || lookups-lookupsBefore != 2 {
			t.Errorf("%s: %d key lists and %d id lookups, want 2 of each",
				name, lists-listsBefore, lookups-lookupsBefore)
		}
	}
}

// An old login that nobody took answers 404; the account is still found.
func TestGitHubRosterFindsARenamedAccountWhoseOldLoginIsFree(t *testing.T) {
	f := newGitHub(t)
	own := newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(own))
	f.refresh()
	f.fake.Rename(aliceID, "alice-new")
	f.refresh()
	if login, _ := f.Fetched(alice); login != "alice-new" || !Trusts(f, alice, own) {
		t.Errorf("after the rename: login %q, own key trusted %v", login, Trusts(f, alice, own))
	}
}

// If the login moves again while the keys are read a second time, the
// refresh gives up and the cache stays as it was.
func TestGitHubRefreshGivesUpWhenTheLoginKeepsChanging(t *testing.T) {
	f := newGitHub(t)
	own := newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(own))
	f.refresh()
	before := f.cacheFile()

	f.fake.Before("GET /user/{id}", func() { f.fake.Rename(aliceID, "alice-2") })
	f.fake.Before("GET /user/{id}", func() { f.fake.Rename(aliceID, "alice-3") })
	err := f.Refresh(context.Background(), alice)
	if err == nil || !strings.Contains(err.Error(), "login changed") {
		t.Fatalf("err = %v, want the refresh to give up", err)
	}
	if f.cacheFile() != before || !Trusts(f, alice, own) {
		t.Error("a refresh that gave up changed the cache")
	}

	f.refresh() // things have settled
	if login, _ := f.Fetched(alice); login != "alice-3" {
		t.Errorf("login = %q, want alice-3", login)
	}
}

// More signing keys than h8c reads is a failed refresh, not a roster made
// of the first pages.
func TestGitHubRosterRejectsATruncatedList(t *testing.T) {
	f := newGitHub(t)
	node := newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("laptop", "a"), sshkey.Marshal(node))
	f.refresh()

	for i := range 1000 {
		f.fake.AddKey(aliceID, "other", fmt.Sprintf("ssh-rsa KEY%d", i))
	}
	late := newKey(t)
	f.fake.AddKey(aliceID, KeyTitle("desktop", "b"), sshkey.Marshal(late))

	err := f.Refresh(context.Background(), alice)
	if err == nil || !strings.Contains(err.Error(), "1000 or more") {
		t.Fatalf("err = %v, want a refusal to use a cut-off list", err)
	}
	if !Trusts(f, alice, node) || len(f.Entries(alice)) != 1 {
		t.Error("the failed refresh changed the roster")
	}
}

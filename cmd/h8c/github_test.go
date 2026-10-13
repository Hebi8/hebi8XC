package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/github"
	"github.com/Hebi8/hebi8XC/internal/github/githubtest"
	"github.com/Hebi8/hebi8XC/internal/identity"
	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/roster"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

const aliceID = 4242

// fakeGitHub points the commands at an in-memory GitHub with the account
// alice, and makes the device flow poll without waiting.
func fakeGitHub(t *testing.T) *githubtest.Server {
	t.Helper()
	fake := githubtest.New(t)
	fake.AddAccount(aliceID, "alice")
	fake.SignIn(aliceID)
	realGH, realWait := gh, pollWait
	gh, pollWait = fake.Client(), func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { gh, pollWait = realGH, realWait })
	return fake
}

func h8cFails(t *testing.T, dir string, args ...string) error {
	t.Helper()
	var out bytes.Buffer
	err := run(append([]string{"--data-dir", dir}, args...), &out)
	if err == nil {
		t.Fatalf("h8c %v succeeded:\n%s", args, out.String())
	}
	return err
}

func nodeKey(t *testing.T, dir string) (title, key string) {
	t.Helper()
	ident, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return roster.KeyTitle(ident.Name, ident.ID), sshkey.Marshal(ident.Public())
}

func TestLoginRegistersTheNodeKeyOnce(t *testing.T) {
	fake := fakeGitHub(t)
	fake.SignIn(aliceID, "authorization_pending", "slow_down")
	dir := t.TempDir()

	out := h8c(t, dir, "login")
	for _, want := range []string{
		"https://github.com/login/device", githubtest.UserCode,
		"Logged in as alice (id 4242).", "Registered this node's key", "The account lists 1 node keys.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("login output lacks %q:\n%s", want, out)
		}
	}
	title, key := nodeKey(t, dir)
	keys := fake.Keys(aliceID)
	if len(keys) != 1 || keys[0].Title != title || keys[0].Key != key {
		t.Fatalf("keys on the account = %+v, want this node's key titled %q", keys, title)
	}
	if got := h8c(t, dir, "id"); !strings.Contains(got, "github  alice (id 4242)\n") {
		t.Errorf("h8c id does not show the account:\n%s", got)
	}

	// Again: nothing new is registered.
	again := h8c(t, dir, "login")
	if !strings.Contains(again, "already on the account") {
		t.Errorf("second login output:\n%s", again)
	}
	if n := fake.Requests("POST /user/ssh_signing_keys"); n != 1 {
		t.Errorf("the key was posted %d times, want once", n)
	}
	if len(fake.Keys(aliceID)) != 1 {
		t.Errorf("keys on the account = %+v", fake.Keys(aliceID))
	}

	// No token, access or refresh, is printed or left in the data directory.
	secrets := append(fake.Tokens(), "ghu_", "ghr_")
	if len(fake.Tokens()) != 2 {
		t.Fatalf("the fake issued %d tokens, want 2", len(fake.Tokens()))
	}
	texts := map[string]string{"output": out + again}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path)
		texts[path] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range texts {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s contains %q", where, secret)
			}
		}
	}
}

func TestFailedLoginLeavesTheNodeLoggedOut(t *testing.T) {
	loggedOut := func(t *testing.T, dir string) {
		t.Helper()
		if _, ok, err := github.LoadAccount(filepath.Join(dir, node.AccountFile)); ok || err != nil {
			t.Errorf("an account was recorded (ok=%v, err=%v)", ok, err)
		}
		if got := h8c(t, dir, "id"); !strings.Contains(got, "github  not logged in\n") {
			t.Errorf("h8c id:\n%s", got)
		}
	}

	t.Run("cancelled on GitHub", func(t *testing.T) {
		fake := fakeGitHub(t)
		fake.SignIn(aliceID, "authorization_pending", "access_denied")
		dir := t.TempDir()
		if err := h8cFails(t, dir, "login"); !errors.Is(err, github.ErrDenied) {
			t.Errorf("err = %v, want ErrDenied", err)
		}
		loggedOut(t, dir)
		if len(fake.Keys(aliceID)) != 0 {
			t.Error("a key was registered by a cancelled login")
		}
	})

	t.Run("code expired", func(t *testing.T) {
		fake := fakeGitHub(t)
		fake.SignIn(aliceID, "expired_token")
		dir := t.TempDir()
		if err := h8cFails(t, dir, "login"); !errors.Is(err, github.ErrExpired) {
			t.Errorf("err = %v, want ErrExpired", err)
		}
		loggedOut(t, dir)
	})

	// The key is on the account under a title the roster would skip. Saying
	// "logged in" would leave a node that nobody trusts.
	for name, title := range map[string]string{
		"key registered without the prefix": "added by hand",
		"key registered without a title":    "",
	} {
		t.Run(name, func(t *testing.T) {
			fake := fakeGitHub(t)
			dir := t.TempDir()
			_, key := nodeKey(t, dir)
			fake.AddKey(aliceID, title, key)
			err := h8cFails(t, dir, "login")
			if !strings.Contains(err.Error(), fmt.Sprintf("as %q", title)) || !strings.Contains(err.Error(), keySettingsURL) {
				t.Errorf("err = %v, want the title and where to delete it", err)
			}
			loggedOut(t, dir)
			if n := fake.Requests("POST /user/ssh_signing_keys"); n != 0 {
				t.Errorf("posted the key %d times", n)
			}
		})
	}
}

// A roster cache that cannot be read is replaced, with a warning. It stops
// neither the login nor the node.
func TestBrokenRosterCacheIsReplaced(t *testing.T) {
	fakeGitHub(t)
	cache := func(dir string) string { return filepath.Join(dir, node.GitHubRosterFile) }
	spoil := func(dir string) {
		t.Helper()
		if err := os.WriteFile(cache(dir), []byte("{{{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	spoil(dir)
	out := h8c(t, dir, "login")
	if !strings.Contains(out, "warning: ignoring the GitHub roster cache") ||
		!strings.Contains(out, "Logged in as alice") || !strings.Contains(out, "The account lists 1 node keys.") {
		t.Errorf("login over a broken cache:\n%s", out)
	}
	if got := h8c(t, dir, "roster"); strings.Contains(got, "warning") || !strings.Contains(got, "1 node keys") {
		t.Errorf("roster after the login repaired the cache:\n%s", got)
	}

	spoil(dir)
	if got := h8c(t, dir, "roster"); !strings.Contains(got, "warning: ignoring the GitHub roster cache") ||
		!strings.Contains(got, "never fetched") {
		t.Errorf("roster over a broken cache:\n%s", got)
	}
	var logs bytes.Buffer
	n := startUp(t, dir, &logs)
	if !strings.Contains(logs.String(), "ignoring the GitHub roster cache") {
		t.Errorf("the node did not say that it ignored the cache:\n%s", &logs)
	}
	if err := n.RefreshRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h8c(t, dir, "roster"); strings.Contains(got, "warning") || !strings.Contains(got, "1 node keys") {
		t.Errorf("roster after the node refreshed:\n%s", got)
	}
}

func TestRosterListsKeysBySource(t *testing.T) {
	fake := fakeGitHub(t)
	laptop, desktop := t.TempDir(), t.TempDir()

	if got := h8c(t, laptop, "roster"); !strings.Contains(got, "github  not logged in\n") {
		t.Errorf("roster before login:\n%s", got)
	}
	if err := h8cFails(t, laptop, "roster", "refresh"); !strings.Contains(err.Error(), "h8c login") {
		t.Errorf("refresh before login: %v", err)
	}

	h8c(t, laptop, "login")
	_, laptopKey := nodeKey(t, laptop)
	_, desktopKey := nodeKey(t, desktop)
	commitKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFWeVDEg+tjCem68s7Pd5m2o9PAN+Hv6WuzNBnpaIUmr"
	fake.AddKey(aliceID, "git commit signing", commitKey)
	h8c(t, laptop, "trust", "add", desktopKey, "offline-box")

	got := h8c(t, laptop, "roster")
	for _, want := range []string{
		"github  alice (id 4242), 1 node keys, fetched ",
		"local   1 keys added by hand\n",
		"github  " + laptopKey + "  hebi8xc: ",
		"local   " + desktopKey + "  offline-box\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("roster lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, commitKey) {
		t.Errorf("roster lists the commit signing key:\n%s", got)
	}

	// The second machine logs in. The first one sees it after a refresh.
	h8c(t, desktop, "login")
	if got := h8c(t, laptop, "roster"); strings.Contains(got, "github  "+desktopKey) {
		t.Fatal("the cache changed without a refresh")
	}
	if got := h8c(t, laptop, "roster", "refresh"); !strings.Contains(got, "2 node keys") ||
		!strings.Contains(got, "github  "+desktopKey) {
		t.Errorf("roster after refresh:\n%s", got)
	}

	// A refresh that fails says so and keeps the cache.
	fake.SetDown(true)
	if err := h8cFails(t, laptop, "roster", "refresh"); !strings.Contains(err.Error(), "using the 2 keys fetched") {
		t.Errorf("refresh while GitHub is down: %v", err)
	}
	if got := h8c(t, laptop, "roster"); !strings.Contains(got, "2 node keys") {
		t.Errorf("roster after a failed refresh:\n%s", got)
	}
}

// startUp does what h8c up does, except that the test runs the rounds.
func startUp(t *testing.T, dir string, logs *bytes.Buffer, peers ...string) *node.Node {
	t.Helper()
	log := slog.New(slog.NewTextHandler(logs, nil))
	n, release, err := openNode(context.Background(), dir, upOptions{listen: "127.0.0.1:0", static: peers, discover: "none"}, log)
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

func listenAddr(t *testing.T, logs *bytes.Buffer) string {
	t.Helper()
	_, rest, ok := strings.Cut(logs.String(), "listen=")
	if !ok {
		t.Fatalf("no listen address in the log:\n%s", logs)
	}
	return strings.Fields(rest)[0]
}

// The README flow: log in on each machine, then h8c up. Nobody copies a
// key. Deleting a machine's key on GitHub and refreshing cuts it off.
func TestLoggedInMachinesConnect(t *testing.T) {
	fake := fakeGitHub(t)
	laptop, desktop := t.TempDir(), t.TempDir()
	h8c(t, laptop, "login")
	h8c(t, desktop, "login")
	h8c(t, laptop, "roster", "refresh") // the laptop logged in first

	if err := h8cFails(t, laptop, "up", "--user", "alice"); !strings.Contains(err.Error(), "--user does not apply") {
		t.Errorf("up --user while logged in: %v", err)
	}

	ctx := context.Background()
	var laptopLog, desktopLog bytes.Buffer
	a := startUp(t, laptop, &laptopLog)
	if !strings.Contains(laptopLog.String(), "user=github:4242") || !strings.Contains(laptopLog.String(), "github=alice") {
		t.Errorf("the node did not start under the GitHub account:\n%s", &laptopLog)
	}
	b := startUp(t, desktop, &desktopLog, listenAddr(t, &laptopLog))
	b.Round(ctx)
	a.Round(ctx)
	for _, dir := range []string{laptop, desktop} {
		if got := h8c(t, dir, "peers"); strings.Count(got, " online ") != 1 {
			t.Errorf("peers of %s:\n%s", dir, got)
		}
	}
	if len(h8c(t, laptop, "trust", "list")) != 0 || len(h8c(t, desktop, "trust", "list")) != 0 {
		t.Error("a key was added by hand")
	}

	// The desktop's key is deleted on GitHub; the laptop is told to look.
	_, desktopKey := nodeKey(t, desktop)
	fake.RemoveKey(aliceID, desktopKey)
	if got := h8c(t, laptop, "roster", "refresh"); !strings.Contains(got, "1 node keys") {
		t.Errorf("roster after the removal:\n%s", got)
	}
	if strings.Contains(desktopLog.String(), "refused this node") {
		t.Fatal("the desktop was refused before its key was removed")
	}
	b.Round(ctx)
	if !strings.Contains(desktopLog.String(), "refused this node") {
		t.Errorf("the running laptop still accepts the removed key; desktop log:\n%s", &desktopLog)
	}
}

func TestNameShowsAndSetsTheNodeName(t *testing.T) {
	dir := t.TempDir()
	before := h8c(t, dir, "id")
	hostName := strings.TrimSpace(h8c(t, dir, "name"))
	if !strings.Contains(before, "name    "+hostName+"\n") {
		t.Fatalf("h8c name printed %q, h8c id:\n%s", hostName, before)
	}

	out := h8c(t, dir, "name", "living", "room", "box")
	if out != "This node is now called \"living room box\".\n" {
		t.Errorf("setting the name while logged out and stopped printed:\n%s", out)
	}
	if got := h8c(t, dir, "name"); got != "living room box\n" {
		t.Errorf("h8c name = %q", got)
	}
	// Only the name changed.
	want := strings.ReplaceAll(before, hostName, "living room box")
	if got := h8c(t, dir, "id"); got != want {
		t.Errorf("h8c id after the rename:\n%s\nwant:\n%s", got, want)
	}

	if got := h8c(t, dir, "name", "living room box"); !strings.Contains(got, "already called") {
		t.Errorf("setting the same name printed:\n%s", got)
	}
	if err := h8cFails(t, dir, "name", " "); !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty name: %v", err)
	}
}

// Changing the name does not reach out to GitHub or to the running node;
// it says what is left to do.
func TestNameSaysWhatStillShowsTheOldName(t *testing.T) {
	fake := fakeGitHub(t)
	dir := t.TempDir()
	h8c(t, dir, "name", "laptop")
	h8c(t, dir, "login")
	oldTitle := fake.Keys(aliceID)[0].Title
	if !strings.HasPrefix(oldTitle, "hebi8xc: laptop (") {
		t.Fatalf("the key was registered as %q, want the name set before the login", oldTitle)
	}

	var logs bytes.Buffer
	startUp(t, dir, &logs)
	out := h8c(t, dir, "name", "desk")
	for _, want := range []string{`now called "desk"`, "restart h8c up", keySettingsURL, "h8c login"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if keys := fake.Keys(aliceID); len(keys) != 1 || keys[0].Title != oldTitle {
		t.Errorf("h8c name changed the keys on GitHub: %+v", keys)
	}
	if n := fake.Requests("POST /user/ssh_signing_keys") + fake.Requests("POST /login/device/code"); n != 2 {
		t.Errorf("h8c name talked to GitHub: %d logins or registrations, want the 2 of the login", n)
	}
}

package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/github"
	"github.com/Hebi8/hebi8XC/internal/github/githubtest"
)

const alice = 4242

// waits records what the device flow asked to wait for and returns at once.
type waits []time.Duration

func (w *waits) wait(_ context.Context, d time.Duration) error {
	*w = append(*w, d)
	return nil
}

func seconds(s ...int) []time.Duration {
	out := make([]time.Duration, len(s))
	for i, v := range s {
		out[i] = time.Duration(v) * time.Second
	}
	return out
}

func startFlow(t *testing.T, fake *githubtest.Server) (*github.Client, *github.DeviceCode) {
	t.Helper()
	c := fake.Client()
	dc, err := c.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != githubtest.UserCode || dc.VerificationURI == "" {
		t.Fatalf("device code = %+v", dc)
	}
	return c, dc
}

func TestDeviceFlowWaitsUntilTheCodeIsEntered(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	fake.SignIn(alice, "authorization_pending", "authorization_pending")
	c, dc := startFlow(t, fake)

	var w waits
	token, err := c.WaitForToken(context.Background(), dc, w.wait)
	if err != nil {
		t.Fatal(err)
	}
	if want := seconds(5, 5, 5); !slices.Equal(w, want) {
		t.Errorf("waited %v, want %v: one interval before every poll", w, want)
	}

	got, err := c.Viewer(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if got != (github.Account{ID: alice, Login: "alice"}) {
		t.Errorf("token belongs to %+v", got)
	}
}

func TestDeviceFlowSlowsDownWhenTold(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	fake.SignIn(alice, "authorization_pending", "slow_down", "authorization_pending", "slow_down")
	c, dc := startFlow(t, fake)

	var w waits
	if _, err := c.WaitForToken(context.Background(), dc, w.wait); err != nil {
		t.Fatal(err)
	}
	if want := seconds(5, 5, 10, 10, 15); !slices.Equal(w, want) {
		t.Errorf("waited %v, want %v", w, want)
	}
}

func TestDeviceFlowStopsOnExpiryAndDenial(t *testing.T) {
	for answer, want := range map[string]error{
		"expired_token": github.ErrExpired,
		"access_denied": github.ErrDenied,
	} {
		fake := githubtest.New(t)
		fake.AddAccount(alice, "alice")
		fake.SignIn(alice, "authorization_pending", answer)
		c, dc := startFlow(t, fake)

		var w waits
		token, err := c.WaitForToken(context.Background(), dc, w.wait)
		if !errors.Is(err, want) || token != "" {
			t.Errorf("%s: got token %q, err %v; want %v", answer, token, err, want)
		}
		if len(w) != 2 {
			t.Errorf("%s: polled %d times, want 2", answer, len(w))
		}
	}
}

// If GitHub never says the code expired, the flow still ends when the
// code's lifetime is over.
func TestDeviceFlowEndsWithTheCodeLifetime(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	pending := make([]string, 1000)
	for i := range pending {
		pending[i] = "authorization_pending"
	}
	fake.SignIn(alice, pending...)
	c, dc := startFlow(t, fake)

	var w waits
	_, err := c.WaitForToken(context.Background(), dc, w.wait)
	if !errors.Is(err, github.ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	if want := dc.ExpiresIn / dc.Interval; len(w) != want {
		t.Errorf("polled %d times, want %d", len(w), want)
	}
}

func TestDeviceFlowStopsWhenCancelled(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	c, dc := startFlow(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.WaitForToken(ctx, dc, github.Sleep); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := fake.Requests("POST /login/oauth/access_token"); n != 0 {
		t.Errorf("polled %d times after cancellation", n)
	}
}

func TestSigningKeysAreListedAcrossPages(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	for i := range 250 {
		fake.AddKey(alice, fmt.Sprintf("key %d", i), fmt.Sprintf("ssh-ed25519 KEY%d", i))
	}
	keys, err := fake.Client().SigningKeys(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 250 || keys[249].Title != "key 249" {
		t.Errorf("listed %d keys, want all 250 in order", len(keys))
	}
	if n := fake.Requests("GET /users/{login}/ssh_signing_keys"); n != 3 {
		t.Errorf("made %d requests, want 3 pages", n)
	}
}

func TestRateLimitIsNamedInTheError(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	fake.SetRateLimited(true)
	_, err := fake.Client().SigningKeys(context.Background(), "alice")
	var se *github.StatusError
	if !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("err = %v, want a 403 StatusError", err)
	}
	if want := "rate limit reached, resets at "; len(se.Message) <= len(want) || se.Message[:len(want)] != want {
		t.Errorf("message = %q", se.Message)
	}
}

func TestAccountFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	if _, ok, err := github.LoadAccount(path); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v, want not logged in", ok, err)
	}
	want := github.Account{ID: alice, Login: "alice"}
	if err := github.SaveAccount(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := github.LoadAccount(path)
	if err != nil || !ok || got != want {
		t.Errorf("loaded %+v, %v, %v", got, ok, err)
	}
	if id, ok := github.AccountID(got.User()); !ok || id != alice {
		t.Errorf("AccountID(%q) = %d, %v", got.User(), id, ok)
	}
	for _, user := range []string{"alice", "github:", "github:x", "github:-1"} {
		if _, ok := github.AccountID(user); ok {
			t.Errorf("AccountID(%q) succeeded", user)
		}
	}
}

// A request that carries the token is not redirected: net/http would keep
// the Authorization header on the way to a subdomain or to plain HTTP. A
// read without a token may follow.
func TestTokenIsNotSentAfterARedirect(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	fake.SignIn(alice)
	c, dc := startFlow(t, fake)
	var w waits
	token, err := c.WaitForToken(context.Background(), dc, w.wait)
	if err != nil {
		t.Fatal(err)
	}

	var sawToken, requests atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			sawToken.Add(1)
		}
		rw.Write([]byte("[]"))
	}))
	defer elsewhere.Close()
	fake.SetRedirect(elsewhere.URL)
	ctx := context.Background()

	var se *github.StatusError
	if _, err := c.Viewer(ctx, token); !errors.As(err, &se) || se.Status != http.StatusTemporaryRedirect {
		t.Errorf("Viewer: err = %v, want the 307 as a failure", err)
	}
	if _, err := c.ViewerSigningKeys(ctx, token); err == nil {
		t.Error("ViewerSigningKeys followed a redirect")
	}
	if err := c.AddSigningKey(ctx, token, "hebi8xc: laptop (a)", "ssh-ed25519 KEY"); err == nil {
		t.Error("AddSigningKey followed a redirect")
	}
	if requests.Load() != 0 {
		t.Fatalf("%d requests with a token reached the redirect target, %d of them with the token",
			requests.Load(), sawToken.Load())
	}

	if _, err := c.SigningKeys(ctx, "alice"); err != nil {
		t.Errorf("a read without a token did not follow the redirect: %v", err)
	}
	if requests.Load() != 1 || sawToken.Load() != 0 {
		t.Errorf("after the public read: %d requests, %d with a token", requests.Load(), sawToken.Load())
	}
}

func TestMoreKeysThanAreReadIsAnError(t *testing.T) {
	fake := githubtest.New(t)
	fake.AddAccount(alice, "alice")
	for i := range 999 {
		fake.AddKey(alice, "key", fmt.Sprintf("ssh-ed25519 KEY%d", i))
	}
	if keys, err := fake.Client().SigningKeys(context.Background(), "alice"); err != nil || len(keys) != 999 {
		t.Fatalf("999 keys: got %d, %v", len(keys), err)
	}
	fake.AddKey(alice, "key", "ssh-ed25519 KEY999")
	if keys, err := fake.Client().SigningKeys(context.Background(), "alice"); err == nil {
		t.Errorf("1000 keys: got %d keys and no error; a full last page may hide more", len(keys))
	}
}

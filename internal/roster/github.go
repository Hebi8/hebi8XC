package roster

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8XC/internal/datadir"
	"github.com/Hebi8/hebi8XC/internal/github"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

// KeyTitlePrefix marks the SSH signing keys on a GitHub account that are
// node keys. Signing keys with other titles, such as the ones the user
// signs commits with, are not nodes.
const KeyTitlePrefix = "hebi8xc: "

// KeyTitle is the title a node's key is registered under.
func KeyTitle(nodeName, nodeID string) string {
	return fmt.Sprintf("%s%s (%s)", KeyTitlePrefix, nodeName, nodeID)
}

// GitHub is a Roster whose source of truth is the public list of SSH
// signing keys of a GitHub account. The user it is asked about is
// "github:<account id>"; the id is durable, so the roster follows the
// account through a rename.
//
// Keys answers from a cache file, which Refresh rewrites. Reading the list
// needs no token.
type GitHub struct {
	path   string
	client *github.Client
	now    func() time.Time

	refreshing sync.Mutex // one Refresh at a time

	mu    sync.RWMutex
	cache githubCache
}

type githubCache struct {
	AccountID int64       `json:"account_id"`
	Login     string      `json:"login"`
	FetchedAt time.Time   `json:"fetched_at"`
	Keys      []githubKey `json:"keys"`
}

type githubKey struct {
	Key   string `json:"key"`
	Title string `json:"title"`

	parsed ed25519.PublicKey
}

// OpenGitHub loads the cache at path. A missing file is an empty cache.
func OpenGitHub(path string, client *github.Client, now func() time.Time) (*GitHub, error) {
	g := &GitHub{path: path, client: client, now: now}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &g.cache); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range g.cache.Keys {
		k := &g.cache.Keys[i]
		if k.parsed, _, err = sshkey.Parse(k.Key); err != nil {
			return nil, fmt.Errorf("%s: key %d: %w", path, i+1, err)
		}
	}
	return g, nil
}

// Keys returns the cached node keys of user, or none if the cache belongs
// to another account.
func (g *GitHub) Keys(user string) ([]ed25519.PublicKey, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.holds(user) {
		return nil, nil
	}
	keys := make([]ed25519.PublicKey, len(g.cache.Keys))
	for i, k := range g.cache.Keys {
		keys[i] = k.parsed
	}
	return keys, nil
}

// holds reports whether the cache is about user. Callers hold mu.
func (g *GitHub) holds(user string) bool {
	id, ok := github.AccountID(user)
	return ok && id == g.cache.AccountID
}

// Entries returns the cached node keys of user with their titles.
func (g *GitHub) Entries(user string) []Entry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.holds(user) {
		return nil
	}
	entries := make([]Entry, len(g.cache.Keys))
	for i, k := range g.cache.Keys {
		entries[i] = Entry{Key: k.parsed, Comment: k.Title}
	}
	return entries
}

// Fetched returns the login the account of user had at the last successful
// refresh and when that was. The time is zero if there never was one.
func (g *GitHub) Fetched(user string) (login string, at time.Time) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.holds(user) {
		return "", time.Time{}
	}
	return g.cache.Login, g.cache.FetchedAt
}

// Refresh fetches the account's signing keys and replaces the cache. If it
// fails the cache stays in use, and the error says how old the cache is.
func (g *GitHub) Refresh(ctx context.Context, user string) error {
	g.refreshing.Lock()
	defer g.refreshing.Unlock()

	cache, err := g.fetch(ctx, user)
	if err != nil {
		return fmt.Errorf("GitHub roster not refreshed, %s: %w", g.age(user), err)
	}
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	if err := datadir.WriteFile(g.path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	g.mu.Lock()
	g.cache = cache
	g.mu.Unlock()
	return nil
}

func (g *GitHub) fetch(ctx context.Context, user string) (githubCache, error) {
	id, ok := github.AccountID(user)
	if !ok {
		return githubCache{}, fmt.Errorf("%q is not a GitHub account", user)
	}
	// Keys can only be listed by login, and a login can pass to someone
	// else. The id is resolved first, every time, so the keys always come
	// from the account that logged in.
	account, err := g.client.AccountByID(ctx, id)
	if err != nil {
		return githubCache{}, err
	}
	listed, err := g.client.SigningKeys(ctx, account.Login)
	if err != nil {
		return githubCache{}, err
	}
	cache := githubCache{AccountID: id, Login: account.Login, FetchedAt: g.now(), Keys: []githubKey{}}
	for _, k := range listed {
		if !strings.HasPrefix(k.Title, KeyTitlePrefix) {
			continue
		}
		parsed, _, err := sshkey.Parse(k.Key)
		if err != nil {
			continue // not Ed25519, so not a node key
		}
		cache.Keys = append(cache.Keys, githubKey{Key: sshkey.Marshal(parsed), Title: k.Title, parsed: parsed})
	}
	return cache, nil
}

// age describes the cache for the error of a failed refresh.
func (g *GitHub) age(user string) string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.holds(user) || g.cache.FetchedAt.IsZero() {
		return "and there are no cached keys"
	}
	return fmt.Sprintf("using the %d keys fetched %s ago",
		len(g.cache.Keys), g.now().Sub(g.cache.FetchedAt).Round(time.Second))
}

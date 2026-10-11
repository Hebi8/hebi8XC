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
// account through a rename and never takes the keys of whoever holds the
// old login.
//
// Keys answers from a cache file, which Refresh rewrites. Reading the list
// needs no token.
type GitHub struct {
	path      string
	client    *github.Client
	login     string // where to look before a refresh has stored the login
	now       func() time.Time
	discarded error

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

// OpenGitHub loads the cache at path. A missing file is an empty cache, and
// so is one that cannot be read as a cache: the next refresh rewrites it,
// and Discarded tells why it was ignored. login is the login the account
// had when the node logged in.
func OpenGitHub(path string, client *github.Client, login string, now func() time.Time) (*GitHub, error) {
	g := &GitHub{path: path, client: client, login: login, now: now}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	cache, err := parseCache(raw)
	if err != nil {
		g.discarded = fmt.Errorf("ignoring the GitHub roster cache %s: %w", path, err)
		return g, nil
	}
	g.cache = cache
	return g, nil
}

func parseCache(raw []byte) (githubCache, error) {
	var cache githubCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		return githubCache{}, err
	}
	for i := range cache.Keys {
		k := &cache.Keys[i]
		var err error
		if k.parsed, _, err = sshkey.Parse(k.Key); err != nil {
			return githubCache{}, fmt.Errorf("key %d: %w", i+1, err)
		}
	}
	return cache, nil
}

// Discarded returns why the cache file was ignored when the roster was
// opened, or nil if it was not.
func (g *GitHub) Discarded() error {
	return g.discarded
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

// fetch reads the account's node keys. Keys can only be listed by login,
// and a login can pass to another account, so a list only counts when the
// id, looked up after the list was read, still has the login it was read
// under. Normally that is two requests. When the login has changed, the
// list is thrown away and read again under the new login, once.
func (g *GitHub) fetch(ctx context.Context, user string) (githubCache, error) {
	id, ok := github.AccountID(user)
	if !ok {
		return githubCache{}, fmt.Errorf("%q is not a GitHub account", user)
	}
	g.mu.RLock()
	login := g.login
	if g.holds(user) && g.cache.Login != "" {
		login = g.cache.Login
	}
	g.mu.RUnlock()

	for range 2 {
		var listed []github.SigningKey
		listErr := errors.New("no login to list keys under")
		if login != "" {
			listed, listErr = g.client.SigningKeys(ctx, login)
			// Nobody has this login any more: the account was renamed.
			if listErr != nil && !github.IsNotFound(listErr) {
				return githubCache{}, listErr
			}
		}
		account, err := g.client.AccountByID(ctx, id)
		if err != nil {
			return githubCache{}, err
		}
		if listErr == nil && strings.EqualFold(account.Login, login) {
			return g.nodeKeys(id, account.Login, listed), nil
		}
		login = account.Login
	}
	return githubCache{}, errors.New("the account's login changed while its keys were being read")
}

// nodeKeys keeps the keys that are node keys: Ed25519, with the title prefix.
func (g *GitHub) nodeKeys(id int64, login string, listed []github.SigningKey) githubCache {
	cache := githubCache{AccountID: id, Login: login, FetchedAt: g.now(), Keys: []githubKey{}}
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
	return cache
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

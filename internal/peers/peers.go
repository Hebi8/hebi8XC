// Package peers is the node list: every node that has ever passed the
// handshake, where it can be reached and whether it is reachable now.
package peers

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/Hebi8/hebi8XC/internal/datadir"
)

const (
	// maxAddrs bounds how many addresses are kept per peer; the ones that
	// worked longest ago are dropped first.
	maxAddrs = 8

	// saveEvery is how stale the last-seen times on disk may get while
	// nothing else changes.
	saveEvery = time.Minute
)

// Addr is one address a peer answered on and the sources that suggested it.
type Addr struct {
	Addr    string    `json:"addr"`
	Sources []string  `json:"sources"`
	LastOK  time.Time `json:"last_ok"`
}

type Peer struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Key      string    `json:"key"`     // "ssh-ed25519 ..."
	Sources  []string  `json:"sources"` // every source that ever led to this peer
	Addrs    []Addr    `json:"addrs"`
	LastSeen time.Time `json:"last_seen"`
	Online   bool      `json:"online,omitempty"` // runtime state, never stored
}

// Contact is one authenticated exchange with a peer. Addr is empty when the
// peer called us and we have not reached it ourselves.
type Contact struct {
	ID      string
	Name    string
	Key     string
	Addr    string
	Sources []string
}

// Table is safe for concurrent use.
type Table struct {
	path         string
	now          func() time.Time
	offlineAfter time.Duration

	mu        sync.Mutex
	peers     map[string]*Peer
	changed   bool // something other than a last-seen time changed
	touched   bool
	lastSaved time.Time
}

// Load reads the table stored at path; a missing file is an empty table.
// Every loaded peer starts offline. A peer goes offline again when it has
// not been in contact for offlineAfter.
func Load(path string, now func() time.Time, offlineAfter time.Duration) (*Table, error) {
	t := &Table{path: path, now: now, offlineAfter: offlineAfter, peers: make(map[string]*Peer), lastSaved: now()}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	var stored []*Peer
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, errors.New(path + ": " + err.Error())
	}
	for _, p := range stored {
		t.peers[p.ID] = p
	}
	return t, nil
}

// Observe records a contact and reports whether it brought the peer online.
// Contacts with the same node id merge into one peer, whatever address or
// source they came from.
func (t *Table) Observe(c Contact) (cameOnline bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	p := t.peers[c.ID]
	if p == nil {
		p = &Peer{ID: c.ID}
		t.peers[c.ID] = p
		t.changed = true
	}
	if p.Name != c.Name || p.Key != c.Key {
		p.Name, p.Key = c.Name, c.Key
		t.changed = true
	}
	if merged := union(p.Sources, c.Sources); len(merged) != len(p.Sources) {
		p.Sources = merged
		t.changed = true
	}
	if c.Addr != "" {
		t.observeAddr(p, c, now)
	}
	cameOnline = !p.Online
	p.Online = true
	p.LastSeen = now
	t.changed = t.changed || cameOnline
	t.touched = true
	return cameOnline
}

func (t *Table) observeAddr(p *Peer, c Contact, now time.Time) {
	i := slices.IndexFunc(p.Addrs, func(a Addr) bool { return a.Addr == c.Addr })
	if i < 0 {
		p.Addrs = append(p.Addrs, Addr{Addr: c.Addr})
		i = len(p.Addrs) - 1
		t.changed = true
	}
	a := &p.Addrs[i]
	if merged := union(a.Sources, c.Sources); len(merged) != len(a.Sources) {
		a.Sources = merged
		t.changed = true
	}
	a.LastOK = now
	if len(p.Addrs) > maxAddrs {
		slices.SortStableFunc(p.Addrs, func(x, y Addr) int { return y.LastOK.Compare(x.LastOK) })
		p.Addrs = p.Addrs[:maxAddrs]
	}
}

// Sweep marks peers that have been silent for too long as offline and
// returns them. They stay in the table.
func (t *Table) Sweep() []Peer {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var gone []Peer
	for _, p := range t.peers {
		if p.Online && now.Sub(p.LastSeen) > t.offlineAfter {
			p.Online = false
			t.changed = true
			gone = append(gone, clone(p))
		}
	}
	return gone
}

// List returns a copy of every peer, sorted by name, then id.
func (t *Table) List() []Peer {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Peer, 0, len(t.peers))
	for _, p := range t.peers {
		out = append(out, clone(p))
	}
	slices.SortFunc(out, func(a, b Peer) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// Addrs returns every address a known peer has answered on.
func (t *Table) Addrs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, p := range t.peers {
		for _, a := range p.Addrs {
			out = append(out, a.Addr)
		}
	}
	return out
}

// Save writes the table if it changed. Last-seen times alone are written at
// most once per saveEvery, unless force is set.
func (t *Table) Save(force bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.changed && !(t.touched && (force || now.Sub(t.lastSaved) >= saveEvery)) {
		return nil
	}
	stored := make([]Peer, 0, len(t.peers))
	for _, p := range t.peers {
		c := *p
		c.Online = false
		stored = append(stored, c)
	}
	slices.SortFunc(stored, func(a, b Peer) int { return cmp.Compare(a.ID, b.ID) })
	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	if err := datadir.WriteFile(t.path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	t.changed, t.touched, t.lastSaved = false, false, now
	return nil
}

func union(have, add []string) []string {
	out := slices.Clone(have)
	for _, s := range add {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

func clone(p *Peer) Peer {
	c := *p
	c.Sources = slices.Clone(p.Sources)
	c.Addrs = slices.Clone(p.Addrs)
	for i := range c.Addrs {
		c.Addrs[i].Sources = slices.Clone(c.Addrs[i].Sources)
	}
	return c
}

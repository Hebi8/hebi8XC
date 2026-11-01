package sessions

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
)

// Combined publishes several local sources. Sources later in the list win
// duplicate keys. An ACP-native copy also wins over Wake's antigravity:<id>:
// the native database is current without waiting for Wake to refresh.
// Titles and paths are not identities: only the same native id is deduplicated.
type Combined struct {
	Sources []Source
	mu      sync.Mutex
	seen    map[int]bool
	routes  map[string]Source
}

func (c *Combined) Sessions(ctx context.Context) ([]Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[int]bool)
	}
	routes := make(map[string]Source)
	all := make(map[string]Session)
	var missing error
	available := false
	for i, source := range c.Sources {
		list, err := source.Sessions(ctx)
		if err != nil {
			// A source that has never existed is optional. Losing an existing
			// source must not turn its whole feed into deletions.
			if errors.Is(err, ErrNoIndex) && !c.seen[i] {
				missing = errors.Join(missing, err)
				continue
			}
			return nil, err
		}
		c.seen[i] = true
		available = true
		for _, s := range list {
			all[s.Key] = s
			routes[s.Key] = source
		}
	}
	if !available {
		if missing == nil {
			missing = ErrNoIndex
		}
		return nil, missing
	}
	for key := range all {
		if id, ok := strings.CutPrefix(key, "antigravity-acp:"); ok {
			delete(all, "antigravity:"+id)
			delete(routes, "antigravity:"+id)
		}
	}
	out := make([]Session, 0, len(all))
	for _, s := range all {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Session) int { return cmp.Compare(a.Key, b.Key) })
	c.routes = routes
	return out, nil
}

func (c *Combined) Messages(ctx context.Context, key string, after int64, limit, budget int) ([]Message, bool, error) {
	c.mu.Lock()
	source := c.routes[key]
	c.mu.Unlock()
	if source == nil {
		return nil, false, ErrNotFound
	}
	return source.Messages(ctx, key, after, limit, budget)
}

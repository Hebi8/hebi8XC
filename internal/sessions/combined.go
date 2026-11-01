package sessions

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
)

// Combined publishes Wake and ACP sessions. An ACP-native copy wins over Wake's antigravity:<id>:
// the native database is current without waiting for Wake to refresh.
// Titles and paths are not identities: only the same native id is deduplicated.
type Combined struct {
	Wake, ACP Source
	mu        sync.Mutex
	last      map[int][]Session
}

func (c *Combined) Sessions(ctx context.Context) ([]Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = make(map[int][]Session)
	}
	all := make(map[string]Session)
	var missing error
	available := false
	for i, source := range []Source{c.Wake, c.ACP} {
		if source == nil {
			continue
		}
		list, err := source.Sessions(ctx)
		if err != nil {
			if previous, ok := c.last[i]; ok {
				list = previous
			} else if errors.Is(err, ErrNoIndex) {
				missing = errors.Join(missing, err)
				continue
			} else {
				return nil, err
			}
		} else {
			c.last[i] = list
		}
		available = true
		for _, s := range list {
			all[s.Key] = s
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
		}
	}
	out := make([]Session, 0, len(all))
	for _, s := range all {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Session) int { return cmp.Compare(a.Key, b.Key) })
	return out, nil
}

func (c *Combined) Messages(ctx context.Context, key string, after int64, limit, budget int) ([]Message, bool, error) {
	source := c.Wake
	if strings.HasPrefix(key, "antigravity-acp:") {
		source = c.ACP
	}
	if source == nil {
		return nil, false, ErrNotFound
	}
	return source.Messages(ctx, key, after, limit, budget)
}

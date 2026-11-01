package sessions

import (
	"errors"
	"testing"
)

func TestCombinedDedupAndRouting(t *testing.T) {
	wake, acp := newSource(), newSource()
	wake.put("antigravity:same", "Wake", 10, "old")
	wake.put("antigravity:other", "same title", 10, "different session")
	wake.put("claude:same", "another agent", 10, "claude")
	acp.put("antigravity-acp:same", "ACP", 20, "new")
	c := &Combined{Sources: []Source{wake, acp}}
	list, err := c.Sessions(ctx)
	if err != nil || len(list) != 3 {
		t.Fatal(list, err)
	}
	for _, s := range list {
		if s.Key == "antigravity:same" {
			t.Fatal("duplicate survived")
		}
	}
	msgs, _, err := c.Messages(ctx, "antigravity-acp:same", 0, 10, PageBudget)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "new" {
		t.Fatal(msgs, err)
	}
	if _, _, err := c.Messages(ctx, "antigravity:same", 0, 10, PageBudget); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	msgs, _, err = c.Messages(ctx, "claude:same", 0, 10, PageBudget)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "claude" {
		t.Fatal(msgs, err)
	}
}

func TestCombinedOptionalAndFailedSources(t *testing.T) {
	wake, acp := newSource(), newSource()
	wake.err = ErrNoIndex
	acp.err = ErrNoIndex
	c := &Combined{Sources: []Source{wake, acp}}
	if _, err := c.Sessions(ctx); !errors.Is(err, ErrNoIndex) {
		t.Fatal(err)
	}
	acp.err = nil
	acp.put("antigravity-acp:a", "ACP", 10, "text")
	if list, err := c.Sessions(ctx); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	wake.err = nil
	wake.put("codex:b", "Wake", 20, "text")
	if list, err := c.Sessions(ctx); err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	wake.err = ErrNoIndex
	if _, err := c.Sessions(ctx); !errors.Is(err, ErrNoIndex) {
		t.Fatal("disappearance would delete existing feed", err)
	}
	wake.err = errors.New("broken schema")
	if _, err := c.Sessions(ctx); err != wake.err {
		t.Fatal(err)
	}
}

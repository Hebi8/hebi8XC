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
	c := &Combined{Wake: wake, ACP: acp}
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
	if msgs, _, err := c.Messages(ctx, "antigravity:same", 0, 10, PageBudget); err != nil || len(msgs) != 1 || msgs[0].Text != "old" {
		t.Fatal(msgs, err)
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
	c := &Combined{Wake: wake, ACP: acp}
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
	for _, failure := range []error{ErrNoIndex, errors.New("broken schema")} {
		wake.err = failure
		acp.put("antigravity-acp:a", "ACP updated", 30, "new text")
		list, err := c.Sessions(ctx)
		if err != nil || len(list) != 2 || list[0].Title != "ACP updated" || list[1].Title != "Wake" {
			t.Fatal("failed source should retain its list while ACP advances", list, err)
		}
	}
	wake.err = nil
	wake.remove("codex:b")
	wake.put("codex:c", "Wake recovered", 40, "text")
	acp.err = errors.New("unreadable directory")
	if list, err := c.Sessions(ctx); err != nil || len(list) != 2 || list[0].Title != "ACP updated" || list[1].Key != "codex:c" {
		t.Fatal("recovery should publish Wake changes while ACP retains its list", list, err)
	}
}

func TestCombinedMessagesBeforeListing(t *testing.T) {
	wake, acp := newSource(), newSource()
	wake.put("codex:a", "Wake", 10, "wake text")
	acp.put("antigravity-acp:b", "ACP", 20, "acp text")
	c := &Combined{Wake: wake, ACP: acp}
	for key, want := range map[string]string{"codex:a": "wake text", "antigravity-acp:b": "acp text"} {
		msgs, _, err := c.Messages(ctx, key, 0, 10, PageBudget)
		if err != nil || len(msgs) != 1 || msgs[0].Text != want {
			t.Fatal(key, msgs, err)
		}
	}
}

func TestCombinedRestartReadsPersistedFeed(t *testing.T) {
	n := newNetwork(t)
	m := n.machine("desk", "Desk")
	wake, acp := newSource(), newSource()
	wake.put("antigravity:same", "Wake duplicate", 10, "old")
	wake.put("codex:a", "Wake", 10, "wake text")
	acp.put("antigravity-acp:same", "ACP", 20, "acp text")
	m.Source = &Combined{Wake: wake, ACP: acp}
	m.SyncLocal(ctx)
	if err := m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	m.open("desk", "Desk")
	m.Source = &Combined{Wake: wake, ACP: acp}
	// A fresh Combined has not listed either source; the persisted feed still
	// authorizes reads and rejects the Wake entry hidden by deduplication.
	for key, want := range map[string]string{"codex:a": "wake text", "antigravity-acp:same": "acp text"} {
		page, err := m.LocalMessages(ctx, key, 0, MaxPage)
		if err != nil || len(page.Messages) != 1 || page.Messages[0].Text != want {
			t.Fatal(key, page, err)
		}
	}
	if _, err := m.LocalMessages(ctx, "antigravity:same", 0, MaxPage); !errors.Is(err, ErrNotFound) {
		t.Fatal("hidden duplicate must remain unreadable", err)
	}
}

func TestCombinedInitialFailureAndEmptySuccess(t *testing.T) {
	wake, acp := newSource(), newSource()
	acp.put("antigravity-acp:a", "ACP", 10, "text")
	wake.err = errors.New("broken schema")
	c := &Combined{Wake: wake, ACP: acp}
	if _, err := c.Sessions(ctx); err != wake.err {
		t.Fatal("a source without a successful list must fail the round", err)
	}
	wake.err = nil
	if list, err := c.Sessions(ctx); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	wake.err = errors.New("broken again")
	acp.put("antigravity-acp:b", "New ACP", 20, "text")
	if list, err := c.Sessions(ctx); err != nil || len(list) != 2 {
		t.Fatal("an empty successful list also counts as a prior success", list, err)
	}
}

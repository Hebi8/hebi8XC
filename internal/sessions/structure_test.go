package sessions

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStructuredWireAndCopies(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("agent:one", "Example", 1000, "text")
	original := &desk.src.messages["agent:one"][0]
	original.Kind = "text"
	original.Thinking = strings.Repeat("思", 20000)
	original.Usage = &Usage{Input: 7, Output: 5}
	original.Model = "model-y"
	for range 100 {
		original.Calls = append(original.Calls, Call{Call: "call", Tool: "shell", Input: strings.Repeat("中", 6000), Output: strings.Repeat("\x00", 6000), Error: true, Agent: "agent:child"})
	}
	net.sync()
	pg, err := desk.LocalMessages(ctx, "agent:one", 0, MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	m := pg.Messages[0]
	raw, _ := json.Marshal(m.Calls)
	if pg.Format != 1 || len(raw) > 512<<10 || m.CallsDropped == 0 || len(m.Thinking) > 32<<10 {
		t.Fatal("wire limits")
	}
	for _, c := range m.Calls {
		if len(c.Input) > 4<<10 || len(c.Output) > 4<<10 || !c.Truncated || !utf8.ValidString(c.Input) {
			t.Fatal("call limits")
		}
	}
	if len(original.Calls) != 100 || len(original.Calls[0].Input) != 18000 {
		t.Fatal("wire clamp changed local source")
	}
	_, r, err := laptop.readAll("desk", "agent:one", 10)
	if err != nil || r.Format != 1 || len(r.Messages[0].Calls) != len(m.Calls) || r.Messages[0].Usage.Input != 7 {
		t.Fatal("copy lost structure", err)
	}
	desk.online = false
	_, r, err = laptop.readAll("desk", "agent:one", 10)
	if err != nil || r.Format != 1 || r.From != "cache" || !r.Messages[0].Calls[0].Error {
		t.Fatal("offline structure", err)
	}
}

func TestOldCopyRefreshesWhenOnline(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("agent:one", "Example", 1000, "text")
	net.sync()
	if _, _, err := laptop.readAll("desk", "agent:one", 10); err != nil {
		t.Fatal(err)
	}
	// A restart forgets capabilities; this is a persisted pre-structure copy.
	laptop.Store.Close()
	laptop.open(laptop.Self.ID, laptop.Self.Name)
	desk.src.messages["agent:one"][0].Kind = "text"
	desk.src.messages["agent:one"][0].Calls = []Call{{Call: "one", Tool: "shell"}}
	desk.online = false
	_, r, err := laptop.readAll("desk", "agent:one", 10)
	if err != nil || r.Format != 0 || len(r.Messages[0].Calls) != 0 {
		t.Fatal("old offline copy", err)
	}
	desk.online = true
	_, r, err = laptop.readAll("desk", "agent:one", 10)
	if err != nil || r.Format != 1 || r.From != "peer" || len(r.Messages[0].Calls) != 1 {
		t.Fatal("old copy not refreshed", err)
	}
}

func TestStructureMigrationRepublishesOnce(t *testing.T) {
	desk, _, _ := pair(t)
	desk.src.put("agent:one", "Example", 1000, "text")
	desk.SyncLocal(ctx)
	_, rev, _, _ := desk.Store.Lookup(ctx, desk.Self.ID, "agent:one")
	// Drop only the Phase 1 additions to reproduce a stock store with a feed.
	for _, q := range []string{
		"DELETE FROM meta WHERE k='schema'",
		"ALTER TABLE sessions DROP COLUMN app", "ALTER TABLE sessions DROP COLUMN reader", "ALTER TABLE sessions DROP COLUMN tool_calls", "ALTER TABLE sessions DROP COLUMN tool_errors",
		"ALTER TABLE content DROP COLUMN format",
		"ALTER TABLE messages DROP COLUMN kind", "ALTER TABLE messages DROP COLUMN thinking", "ALTER TABLE messages DROP COLUMN calls", "ALTER TABLE messages DROP COLUMN usage", "ALTER TABLE messages DROP COLUMN model", "ALTER TABLE messages DROP COLUMN calls_dropped",
	} {
		if _, err := desk.Store.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	desk.Store.Close()
	desk.open(desk.Self.ID, desk.Self.Name)
	desk.SyncLocal(ctx)
	_, next, _, _ := desk.Store.Lookup(ctx, desk.Self.ID, "agent:one")
	if next <= rev {
		t.Fatal("migration did not republish")
	}
	desk.Store.Close()
	desk.open(desk.Self.ID, desk.Self.Name)
	desk.SyncLocal(ctx)
	_, last, _, _ := desk.Store.Lookup(ctx, desk.Self.ID, "agent:one")
	if last != next {
		t.Fatal("migration republished twice")
	}
}

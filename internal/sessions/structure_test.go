package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
	removeStructureSchema(t, desk.Store)
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

func removeStructureSchema(t *testing.T, store *Store) {
	t.Helper()
	// Drop only the Phase 1 additions to reproduce a stock store with a feed.
	for _, q := range []string{
		"DELETE FROM meta WHERE k='schema'",
		"ALTER TABLE sessions DROP COLUMN app", "ALTER TABLE sessions DROP COLUMN reader", "ALTER TABLE sessions DROP COLUMN tool_calls", "ALTER TABLE sessions DROP COLUMN tool_errors",
		"ALTER TABLE content DROP COLUMN format",
		"ALTER TABLE messages DROP COLUMN kind", "ALTER TABLE messages DROP COLUMN thinking", "ALTER TABLE messages DROP COLUMN calls", "ALTER TABLE messages DROP COLUMN usage", "ALTER TABLE messages DROP COLUMN model", "ALTER TABLE messages DROP COLUMN calls_dropped",
	} {
		if _, err := store.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFormatProbeIsPerSessionRevision(t *testing.T) {
	desk, laptop, net := pair(t)
	for _, key := range []string{"agent:plain", "agent:structured"} {
		desk.src.put(key, "Example", 1000, "text")
	}
	net.sync()
	for _, key := range []string{"agent:plain", "agent:structured"} {
		if _, _, err := laptop.readAll("desk", key, 10); err != nil {
			t.Fatal(err)
		}
	}
	probes := map[string]int{}
	laptop.Fetch = func(ctx context.Context, p Peer, path string, out any) error {
		if strings.Contains(path, "limit=1&") || strings.HasSuffix(path, "limit=1") {
			probes[path]++
		}
		return net.fetch(ctx, p, path, out)
	}
	// The negative probe for plain must not suppress the structured session.
	desk.src.messages["agent:structured"][0].Kind = "text"
	desk.src.messages["agent:structured"][0].Calls = []Call{{Call: "one", Tool: "shell"}}
	for range 3 {
		if _, _, err := laptop.readAll("desk", "agent:plain", 10); err != nil {
			t.Fatal(err)
		}
	}
	_, r, err := laptop.readAll("desk", "agent:structured", 10)
	if err != nil || r.Format != 1 || r.From != "peer" || len(r.Messages[0].Calls) != 1 {
		t.Fatalf("structure refresh: %+v %v", r, err)
	}
	plainPath := "/v1/sessions/messages?key=agent%3Aplain&after=0&limit=1"
	structuredPath := "/v1/sessions/messages?key=agent%3Astructured&after=0&limit=1"
	if probes[plainPath] != 1 || probes[structuredPath] != 1 {
		t.Fatalf("probes: %v", probes)
	}
	desk.src.add("agent:plain", "more text")
	net.sync()
	for range 3 {
		if _, _, err := laptop.readAll("desk", "agent:plain", 10); err != nil {
			t.Fatal(err)
		}
	}
	if probes[plainPath] != 2 {
		t.Fatalf("new revision probe count: %v", probes)
	}
}

func TestStructureMigrationReplaysRemoteMetadataKeepsCopy(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("agent:one", "Example", 1000, "saved text")
	meta := desk.src.sessions["agent:one"]
	calls, errs := int64(3), int64(1)
	meta.App, meta.Reader, meta.ToolCalls, meta.ToolErrors = "terminal", "wake", &calls, &errs
	desk.src.sessions[meta.Key] = meta
	net.sync()
	if _, _, err := laptop.readAll("desk", meta.Key, 10); err != nil {
		t.Fatal(err)
	}
	before, ok, err := laptop.Store.Cached(ctx, desk.Self.ID, meta.Key)
	if err != nil || !ok {
		t.Fatal(err)
	}
	feed, err := laptop.Store.Feed(ctx, desk.Self.ID)
	if err != nil || feed.Cursor == 0 {
		t.Fatal("feed", err)
	}
	// B has consumed A's new feed using the old schema, losing optional fields.
	removeStructureSchema(t, laptop.Store)
	laptop.Store.Close()
	laptop.open(laptop.Self.ID, laptop.Self.Name)
	replay, err := laptop.Store.Feed(ctx, desk.Self.ID)
	if err != nil || replay.Cursor != 0 || replay.Epoch != feed.Epoch {
		t.Fatalf("replay cursor: %+v %v", replay, err)
	}
	net.sync()
	got, _, ok, err := laptop.Store.Lookup(ctx, desk.Self.ID, meta.Key)
	if err != nil || !ok || got.App != meta.App || got.Reader != meta.Reader || got.ToolCalls == nil || *got.ToolCalls != calls || got.ToolErrors == nil || *got.ToolErrors != errs {
		t.Fatalf("metadata: %+v %v", got, err)
	}
	after, ok, err := laptop.Store.Cached(ctx, desk.Self.ID, meta.Key)
	if err != nil || !ok || !reflect.DeepEqual(before, after) {
		t.Fatalf("copy changed: %+v -> %+v %v", before, after, err)
	}
	desk.online = false
	texts, r, err := laptop.readAll("desk", meta.Key, 10)
	if err != nil || r.From != "cache" || !reflect.DeepEqual(texts, []string{"saved text"}) {
		t.Fatalf("saved messages: %v %+v %v", texts, r, err)
	}
}

func TestCopyToleratesRevisionAndFormatChange(t *testing.T) {
	for _, formats := range [][2]int{{1, 1}, {1, 0}, {0, 1}} {
		t.Run(fmt.Sprint(formats), func(t *testing.T) {
			desk, laptop, net := pair(t)
			desk.src.put("agent:one", "Example", 1000, "text")
			net.sync()
			requests := 0
			laptop.Fetch = func(_ context.Context, _ Peer, _ string, out any) error {
				requests++
				pg := out.(*messagesPage)
				*pg = messagesPage{Rev: int64(requests), Format: formats[requests-1], More: requests == 1, Messages: []Message{{ID: int64(requests), Text: "page"}}}
				return nil
			}
			_, r, err := laptop.readAll("desk", "agent:one", 10)
			if err != nil || requests != 2 || r.From != "peer" || r.Format != min(formats[0], formats[1]) || len(r.Messages) != 2 {
				t.Fatalf("copy: %+v %v (%d requests)", r, err, requests)
			}
			cp, ok, err := laptop.Store.Cached(ctx, desk.Self.ID, "agent:one")
			if err != nil || !ok || cp.Rev != 1 || !cp.Complete {
				t.Fatalf("copy revision: %+v %v", cp, err)
			}
			// Mixed pages retain format 0, allowing a structure refresh on next read.
			if formats[0] != formats[1] {
				desk.src.messages["agent:one"][0].Kind = "text"
				laptop.Fetch = net.fetch
				_, r, err = laptop.readAll("desk", "agent:one", 10)
				if err != nil || r.Format != 1 || r.From != "peer" {
					t.Fatalf("next reading: %+v %v", r, err)
				}
			}
		})
	}
}

func TestCopyStopsWhenTheFeedChangesBetweenPages(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("agent:one", "Example", 1000, "text")
	net.sync()
	epoch, err := desk.Store.Epoch()
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	laptop.Fetch = func(_ context.Context, _ Peer, _ string, out any) error {
		requests++
		pg := out.(*messagesPage)
		*pg = messagesPage{Epoch: epoch, Rev: 1, Format: 1, More: true, Messages: []Message{{ID: int64(requests), Text: "page"}}}
		if requests == 2 {
			pg.Epoch, pg.More = "another feed", false
		}
		return nil
	}
	if _, _, err := laptop.readAll("desk", "agent:one", 10); !errors.Is(err, ErrFeedChanged) || requests != 2 {
		t.Fatalf("read across two feeds: %v (%d requests)", err, requests)
	}
	if _, ok, err := laptop.Store.Cached(ctx, desk.Self.ID, "agent:one"); err != nil || ok {
		t.Fatalf("a copy was kept across two feeds: %v %v", ok, err)
	}
}

func TestDeletedSessionClearsStructureMetadata(t *testing.T) {
	desk, _, _ := pair(t)
	desk.src.put("agent:one", "Example", 1000, "text")
	s := desk.src.sessions["agent:one"]
	calls, errs := int64(2), int64(1)
	s.App, s.Reader, s.ToolCalls, s.ToolErrors = "terminal", "wake", &calls, &errs
	desk.src.sessions[s.Key] = s
	desk.SyncLocal(ctx)
	desk.src.remove(s.Key)
	desk.SyncLocal(ctx)
	changes, err := desk.Store.Changes(ctx, desk.Self.ID, 0, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("changes: %+v %v", changes, err)
	}
	c := changes[0]
	if !c.Deleted || c.App != "" || c.Reader != "" || c.ToolCalls != nil || c.ToolErrors != nil {
		t.Fatalf("tombstone: %+v", c)
	}
}

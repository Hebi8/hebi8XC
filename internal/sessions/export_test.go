package sessions_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/antigravity"
	"github.com/Hebi8/hebi8XC/internal/sessions/antigravity/acptest"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

var exportContext = context.Background()

type exportSource struct {
	entries    map[string]sessions.Session
	rows       map[string][]sessions.Message
	reads      int
	err        error
	readErr    error
	failAt     int64
	failKey    string
	beforeRead func(string)
}

func (s *exportSource) Sessions(context.Context) ([]sessions.Session, error) {
	var out []sessions.Session
	for _, entry := range s.entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, s.err
}

func (s *exportSource) Messages(_ context.Context, key string, after int64, limit, budget int) ([]sessions.Message, bool, error) {
	s.reads++
	if s.beforeRead != nil {
		s.beforeRead(key)
	}
	if s.readErr != nil && after >= s.failAt && (s.failKey == "" || s.failKey == key) {
		return nil, false, s.readErr
	}
	var out []sessions.Message
	size := 0
	for _, m := range s.rows[key] {
		if m.ID <= after {
			continue
		}
		size += sessions.MessageSize(m)
		if len(out) == limit || (size > budget && len(out) > 0) {
			return out, true, nil
		}
		out = append(out, m)
	}
	return out, false, nil
}

func (s *exportSource) put(key string, updated int64, messages ...sessions.Message) {
	agent, _, _ := strings.Cut(key, ":")
	s.entries[key] = sessions.Session{Key: key, Agent: agent, Title: "Example", CreatedAt: updated - 1,
		UpdatedAt: updated, Messages: int64(len(messages)), Stamp: "initial"}
	s.rows[key] = messages
}

func (s *exportSource) changed(key string) {
	entry := s.entries[key]
	entry.Stamp += "+"
	entry.Messages = int64(len(s.rows[key]))
	s.entries[key] = entry
}

type exportFixture struct {
	logs bytes.Buffer
	t    *testing.T
	path string
	svc  *sessions.Service
	src  *exportSource
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	f := &exportFixture{t: t, path: filepath.Join(t.TempDir(), "sessions.db"),
		src: &exportSource{entries: map[string]sessions.Session{}, rows: map[string][]sessions.Message{}}}
	f.open()
	return f
}

func (f *exportFixture) open() {
	f.t.Helper()
	store, err := sessions.OpenStore(f.path, func() time.Time { return time.UnixMilli(1000) })
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { store.Close() })
	f.svc = &sessions.Service{Self: sessions.Peer{ID: "desk", Name: "Desk"}, Store: store, Source: f.src,
		Log: slog.New(slog.NewTextHandler(&f.logs, nil))}
}

func (f *exportFixture) restart() {
	f.t.Helper()
	if err := f.svc.Store.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.open()
}

func (f *exportFixture) refresh() {
	f.t.Helper()
	if err := f.svc.RefreshLocal(exportContext); err != nil {
		f.t.Fatal(err)
	}
}

func (f *exportFixture) page(options sessions.ExportOptions) sessions.ExportPage {
	f.t.Helper()
	page, err := f.svc.Export(exportContext, options)
	if err != nil {
		f.t.Fatal(err)
	}
	return page
}

func (f *exportFixture) sql(statement string, args ...any) {
	f.t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *exportFixture) number(query string) int64 {
	f.t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func exportMessage(id, seq int64, text string) sessions.Message {
	return sessions.Message{ID: id, Seq: seq, Role: "assistant", Time: 10 + seq, Text: text, Kind: "text", Structure: 1}
}

func TestExportSnapshotsChangesAndDeletion(t *testing.T) {
	f := newExportFixture(t)
	const key = "agent:one"
	m := exportMessage(1, 0, "first")
	m.Thinking, m.Model, m.Sidechain = "thought", "model-x", "side"
	m.Calls = []sessions.Call{{Call: "c1", Tool: "shell", Input: "example", Agent: "agent:child"}}
	m.Usage = &sessions.Usage{Input: 3, Output: 5, CacheRead: 7, CacheWrite: 11}
	f.src.put(key, 100, m)
	// Startup and ordinary rounds must not create a log before since arrives.
	f.svc.SyncLocal(exportContext)
	f.refresh()
	if f.src.reads != 0 || f.number(`SELECT COUNT(*) FROM export_log`) != 0 {
		t.Fatal("refresh logged history before the first export")
	}
	if err := f.svc.Store.ApplyRemote(exportContext, "laptop", "Laptop", "remote-epoch",
		[]sessions.Change{{Rev: 1, Session: sessions.Session{Key: "agent:remote", Agent: "agent"}}}, 1, true); err != nil {
		t.Fatal(err)
	}
	initial := f.page(sessions.ExportOptions{})
	if len(initial.Items) != 2 || initial.More || initial.Next != 2 || initial.Epoch == "" {
		t.Fatalf("initial page: %+v", initial)
	}
	wireMessage := m
	wireMessage.Structure = 0
	if initial.Items[0].Type != "session" || initial.Items[0].Message != nil || initial.Items[0].Session.Messages != 1 ||
		initial.Items[1].Type != "message" || initial.Items[1].Session != nil || !reflect.DeepEqual(*initial.Items[1].Message, wireMessage) {
		t.Fatal("snapshot shape or complete message changed")
	}
	for _, item := range initial.Items {
		if item.Key != key || item.Agent != "agent" || item.G != 0 || item.Rev != 1 {
			t.Fatalf("identity: %+v", item)
		}
	}
	// Only IDs changed during a reparse: the pagination identity is not content.
	f.src.rows[key][0].ID = 100
	f.src.changed(key)
	f.refresh()
	idOnly := f.page(sessions.ExportOptions{After: initial.Next})
	if len(idOnly.Items) != 1 || idOnly.Items[0].G != 0 || idOnly.Items[0].Type != "session" {
		t.Fatal("ID-only reparse changed message identity", idOnly)
	}
	f.src.rows[key][0].Text = "first grows"
	f.src.rows[key][0].Calls = []sessions.Call{{Call: "c1", Tool: "shell", Output: "failed", Error: true}}
	f.src.rows[key] = append(f.src.rows[key], exportMessage(101, 1, "second"))
	f.src.changed(key)
	entry := f.src.entries[key]
	entry.Title, entry.CreatedAt, entry.UpdatedAt = "Title arrived", 30, 200
	f.src.entries[key] = entry
	f.refresh()
	changed := f.page(sessions.ExportOptions{After: idOnly.Next})
	if len(changed.Items) != 3 || changed.Items[0].Session.Title != entry.Title ||
		changed.Items[0].Session.CreatedAt != 30 || changed.Items[0].Session.UpdatedAt != 200 || changed.Items[0].Session.Messages != 2 {
		t.Fatalf("changed page: %+v", changed)
	}
	if changed.Items[1].G != 0 || changed.Items[1].Message.Text != "first grows" || !changed.Items[1].Message.Calls[0].Error {
		t.Fatal("message content change lost")
	}
	// Reading from the log cannot resolve the original item through live rows.
	replay := f.page(sessions.ExportOptions{Limit: 2})
	if !reflect.DeepEqual(replay.Items, initial.Items) || !replay.More {
		t.Fatal("old snapshots changed after a source update")
	}
	reads := f.src.reads
	f.refresh()
	if f.src.reads != reads || len(f.page(sessions.ExportOptions{After: changed.Next}).Items) != 0 {
		t.Fatal("unchanged revision rescanned or appended")
	}
	delete(f.src.entries, key)
	delete(f.src.rows, key)
	f.refresh()
	deleted := f.page(sessions.ExportOptions{After: changed.Next})
	if len(deleted.Items) != 1 || !deleted.Items[0].Deleted || deleted.Items[0].Type != "session" || deleted.Items[0].Key != key {
		t.Fatalf("deletion: %+v", deleted)
	}
}

func TestExportGenerationKeepsEarlierSnapshots(t *testing.T) {
	for _, identity := range []string{"role", "time"} {
		t.Run(identity, func(t *testing.T) {
			f := newExportFixture(t)
			const key = "agent:one"
			f.src.put(key, 100, exportMessage(1, 0, "one"), exportMessage(2, 1, "two"))
			initial := f.page(sessions.ExportOptions{})
			if identity == "role" {
				f.src.rows[key][0].Role = "user"
			} else {
				f.src.rows[key][0].Time++
			}
			f.src.changed(key)
			f.refresh()
			next := f.page(sessions.ExportOptions{After: initial.Next})
			if len(next.Items) != 3 {
				t.Fatal("entire session was not re-emitted", next)
			}
			for _, item := range next.Items {
				if item.G != 1 {
					t.Fatal("generation did not advance once", item)
				}
			}
			all := f.page(sessions.ExportOptions{})
			if !reflect.DeepEqual(all.Items[:3], initial.Items) {
				t.Fatal("renumbering replaced the earlier generation")
			}
			f.page(sessions.ExportOptions{After: next.Next, Ack: &next.Next})
			f.restart()
			// Identity baseline must survive acknowledgement and restart.
			f.src.rows[key][1].Time++
			f.src.changed(key)
			f.refresh()
			last := f.page(sessions.ExportOptions{After: next.Next})
			if len(last.Items) != 3 || last.Items[0].G != 2 || last.Items[0].Cursor <= next.Next {
				t.Fatal("ack/restart lost generation baseline", last)
			}
			// Deletion and restoration must not overwrite an older identity.
			saved := f.src.entries[key]
			delete(f.src.entries, key)
			f.refresh()
			f.src.entries[key] = saved
			f.src.rows[key][0].Time++
			f.refresh()
			restored := f.page(sessions.ExportOptions{After: last.Next})
			if len(restored.Items) != 4 || !restored.Items[0].Deleted || restored.Items[1].G != 3 {
				t.Fatal("restored session overwrote an earlier identity", restored)
			}
		})
	}
}

func TestExportSinceBaselinePersists(t *testing.T) {
	f := newExportFixture(t)
	watermark := int64(100)
	f.src.put("agent:old", 99, exportMessage(1, 1, "old one"), exportMessage(2, 2, "old two"))
	f.src.put("agent:boundary", 100, exportMessage(1, 1, "boundary"))
	f.refresh()
	f.refresh()
	initial := f.page(sessions.ExportOptions{Since: &watermark, Limit: 1})
	if len(initial.Items) != 1 || initial.Items[0].Key != "agent:boundary" || !initial.More {
		t.Fatal("since first page", initial)
	}
	f.restart()
	next := f.page(sessions.ExportOptions{Since: &watermark, After: initial.Next})
	if initial.Epoch != next.Epoch || len(next.Items) != 1 || next.Items[0].Key != "agent:boundary" {
		t.Fatal("since lost across restart or pagination", next)
	}
	f.refresh()
	if len(f.page(sessions.ExportOptions{After: next.Next}).Items) != 0 {
		t.Fatal("old baseline leaked into export after restart")
	}
	// Timestamp stays below W; only content/stamp changes. Export the full
	// formerly excluded session, including its unchanged second message.
	f.src.rows["agent:old"][0].Text += " changed"
	f.src.changed("agent:old")
	f.refresh()
	changed := f.page(sessions.ExportOptions{After: next.Next})
	if len(changed.Items) != 3 || changed.Items[0].Key != "agent:old" || changed.Items[0].G != 0 || changed.Items[2].Message.Text != "old two" {
		t.Fatal("first post-baseline change must emit the full old session", changed)
	}
	f.src.put("agent:late", 1, exportMessage(1, 0, "restored old file"))
	f.refresh()
	late := f.page(sessions.ExportOptions{After: changed.Next})
	if len(late.Items) != 2 || late.Items[0].Key != "agent:late" {
		t.Fatal("later arrival with old timestamp was filtered", late)
	}

}

func TestExportEpochAndAck(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:one", 100, exportMessage(1, 1, "one"))
	initial := f.page(sessions.ExportOptions{})
	f.src.err = errors.New("source unavailable")
	empty := f.page(sessions.ExportOptions{After: initial.Next, Ack: &initial.Next})
	if empty.Next != initial.Next || empty.More || len(empty.Items) != 0 || f.number(`SELECT COUNT(*) FROM export_log`) != 0 {
		t.Fatal("ack did not prune through cursor", empty)
	}
	f.restart()
	if page := f.page(sessions.ExportOptions{After: initial.Next, Ack: &initial.Next}); page.Epoch != initial.Epoch {
		t.Fatal("epoch changed on reopen")
	}
	f.src.err = nil
	f.src.rows["agent:one"][0].Text = "two"
	f.src.changed("agent:one")
	f.refresh()
	next := f.page(sessions.ExportOptions{After: initial.Next})
	if len(next.Items) != 2 || next.Items[0].Cursor <= initial.Next {
		t.Fatal("cursor reused after pruning all snapshots", next)
	}
	// A lost store has no export baseline. A first call carrying the previous
	// epoch's ack/after must return its new epoch without pruning new items.
	fresh := newExportFixture(t)
	fresh.src.put("agent:old", 50, exportMessage(1, 1, "old"))
	fresh.src.put("agent:new", 100, exportMessage(1, 1, "new"))
	fresh.refresh()
	w := int64(100)
	page := fresh.page(sessions.ExportOptions{After: next.Next, Ack: &next.Next, Since: &w})
	if page.Epoch == next.Epoch || len(page.Items) != 2 || page.Items[0].Key != "agent:new" {
		t.Fatal("new epoch did not reestablish since", page)
	}
}

func TestExportFilteredIdentityAndDeletion(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:renumbered", 1, exportMessage(1, 0, "old"))
	f.src.put("agent:deleted", 1, exportMessage(1, 0, "gone"))
	w := int64(100)
	if page := f.page(sessions.ExportOptions{Since: &w}); len(page.Items) != 0 {
		t.Fatal("old sessions were not filtered")
	}
	f.restart()
	f.src.rows["agent:renumbered"][0].Role = "user"
	f.src.changed("agent:renumbered")
	delete(f.src.entries, "agent:deleted")
	f.refresh()
	page := f.page(sessions.ExportOptions{})
	if len(page.Items) != 3 || page.Items[0].Key != "agent:renumbered" || page.Items[0].G != 1 ||
		page.Items[1].G != 1 || !page.Items[2].Deleted || page.Items[2].Key != "agent:deleted" {
		t.Fatal("filtered baseline lost identity or deletion", page)
	}
}

type invalidExportSource struct {
	*exportSource
	page []sessions.Message
	more bool
}

func (s invalidExportSource) Messages(context.Context, string, int64, int, int) ([]sessions.Message, bool, error) {
	return s.page, s.more, nil
}

func TestExportRejectsInvalidSourcePages(t *testing.T) {
	for _, test := range []struct {
		name string
		page []sessions.Message
		more bool
	}{
		{name: "empty with more", more: true},
		{name: "nonadvancing ID", page: []sessions.Message{exportMessage(0, 1, "one")}},
		{name: "duplicate seq", page: []sessions.Message{exportMessage(1, 0, "one"), exportMessage(2, 0, "two")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExportFixture(t)
			f.src.put("agent:one", 100)
			f.svc.Source = invalidExportSource{exportSource: f.src, page: test.page, more: test.more}
			if _, err := f.svc.Export(exportContext, sessions.ExportOptions{}); err != nil || f.logs.Len() == 0 {
				t.Fatal("failure not logged", err)
			}
			if f.number(`SELECT COUNT(*) FROM export_scan`) != 0 || f.number(`SELECT COUNT(*) FROM export_log`) != 0 {
				t.Fatal("invalid page advanced persistent scan")
			}
		})
	}
}

func TestExportScanTransactionFailure(t *testing.T) {
	f := newExportFixture(t)
	const key = "agent:one"
	f.src.put(key, 100, exportMessage(1, 1, "before"))
	initial := f.page(sessions.ExportOptions{})
	beforeRev := f.number(`SELECT rev FROM export_scan WHERE key='agent:one'`)
	f.src.rows[key][0].Text = "after"
	f.src.changed(key)
	// This fails only after new log rows have actually been appended, at the
	// final scan-position write. Both the rows and the baseline must roll back.
	f.sql(fmt.Sprintf(`CREATE TRIGGER fail_export_scan BEFORE UPDATE ON export_scan
 WHEN (SELECT COUNT(*) FROM export_log)>%d BEGIN SELECT RAISE(ABORT,'scan position failure'); END`, initial.Next))
	if err := f.svc.RefreshLocal(exportContext); err != nil || !strings.Contains(f.logs.String(), "scan position failure") {
		t.Fatal("export failure not logged separately", err)
	}
	if f.number(`SELECT rev FROM export_scan WHERE key='agent:one'`) != beforeRev || f.number(`SELECT COUNT(*) FROM export_log`) != initial.Next {
		t.Fatal("append and scan position were not atomic")
	}
	f.sql(`DROP TRIGGER fail_export_scan`)
	f.restart()
	f.refresh() // SetLocal sees no new revision, but the failed scan must retry.
	next := f.page(sessions.ExportOptions{After: initial.Next})
	if len(next.Items) != 2 || next.Items[1].Message.Text != "after" {
		t.Fatal("retry skipped the rolled back change", next)
	}
}

func TestExportInitialTransactionFailure(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:one", 100, exportMessage(1, 1, "one"))
	f.sql(`CREATE TRIGGER fail_export_init BEFORE INSERT ON export_scan
 WHEN (SELECT COUNT(*) FROM export_log)>0 BEGIN SELECT RAISE(ABORT,'initial scan failure'); END`)
	w := int64(0)
	if _, err := f.svc.Export(exportContext, sessions.ExportOptions{Since: &w}); err != nil || !strings.Contains(f.logs.String(), "initial scan failure") {
		t.Fatal("initial failure not logged", err)
	}
	for _, table := range []string{"export_log", "export_scan", "export_messages"} {
		if f.number(`SELECT COUNT(*) FROM `+table) != 0 {
			t.Fatal("initialization leaked", table)
		}
	}
	f.sql(`DROP TRIGGER fail_export_init`)
	f.restart()
	f.refresh()
	page := f.page(sessions.ExportOptions{Since: &w})
	if len(page.Items) != 2 || f.number(`SELECT since FROM export_state`) != w {
		t.Fatal("retry lost initial cutoff", page)
	}
}

func TestExportSourcePagesAndFailure(t *testing.T) {
	f := newExportFixture(t)
	var messages []sessions.Message
	for i := int64(1); i <= sessions.MaxPage+3; i++ {
		messages = append(messages, exportMessage(i, i, "text"))
	}
	f.src.put("agent:one", 100, messages...)
	f.src.readErr, f.src.failAt = errors.New("page failed"), sessions.MaxPage
	if _, err := f.svc.Export(exportContext, sessions.ExportOptions{}); err != nil || !strings.Contains(f.logs.String(), "page failed") {
		t.Fatal("page failure not logged", err)
	}
	if f.number(`SELECT COUNT(*) FROM export_log`) != 0 || f.number(`SELECT COUNT(*) FROM export_scan`) != 0 {
		t.Fatal("partial source read committed")
	}
	f.src.readErr = nil
	f.refresh()
	var after int64
	var got []sessions.ExportItem
	for {
		page := f.page(sessions.ExportOptions{After: after, Limit: 57})
		for _, item := range page.Items {
			if item.Cursor <= after {
				t.Fatal("cursor did not advance")
			}
			after = item.Cursor
		}
		got = append(got, page.Items...)
		if page.Next != after {
			t.Fatal("wrong next cursor")
		}
		if !page.More {
			break
		}
	}
	if len(got) != len(messages)+1 || got[len(got)-1].Message.Seq != int64(len(messages)) {
		t.Fatal("source or export paging lost messages", len(got))
	}
}

func TestExportWakeFullTextAcrossBudget(t *testing.T) {
	f := newExportFixture(t)
	d := waketest.New(t, t.TempDir())
	const key = "codex:example"
	d.Put(waketest.Session{Key: key, Agent: "codex"},
		waketest.Message{Role: "user", Text: "indexed prefix"},
		waketest.Message{Role: "assistant", Text: "another prefix"},
		waketest.Message{Role: "assistant", Text: "end"})
	full := strings.Repeat("全文", sessions.PageBudget) + " tail"
	d.Structure(key, 1, sessions.Message{Text: full, Thinking: strings.Repeat("t", 40<<10),
		Calls: []sessions.Call{{Call: "c1", Tool: "shell", Input: strings.Repeat("i", 20<<10), Output: strings.Repeat("o", 20<<10)}}})
	d.Structure(key, 2, sessions.Message{Text: full + " second"})
	d.Structure(key, 3, sessions.Message{})
	x := wake.Open(d.Path)
	defer x.Close()
	f.svc.Source = x
	first := f.page(sessions.ExportOptions{})
	if len(first.Items) != 1 || !first.More || first.Items[0].Type != "session" {
		t.Fatal("session page should precede large messages", first.Next)
	}
	second := f.page(sessions.ExportOptions{After: first.Next})
	if len(second.Items) != 1 || !second.More || second.Items[0].Message.Text != full || second.Items[0].Message.Truncated ||
		len(second.Items[0].Message.Thinking) != 40<<10 || len(second.Items[0].Message.Calls[0].Output) != 20<<10 {
		t.Fatal("local export applied wire clamps or lost structure")
	}
	third := f.page(sessions.ExportOptions{After: second.Next})
	if len(third.Items) != 1 || !third.More || third.Items[0].Message.Text != full+" second" {
		t.Fatal("second large source page lost text")
	}
	last := f.page(sessions.ExportOptions{After: third.Next})
	if len(last.Items) != 1 || last.More || last.Items[0].Message.Text != "end" {
		t.Fatal("large message paging skipped the tail")
	}
	// The changed suffix lies well beyond the old wire/text limit.
	d.Exec(`UPDATE message_extras SET full_text=full_text || ' changed' WHERE session_key=? AND seq=1`, key)
	d.Exec(`UPDATE sessions SET file_mtime=file_mtime+1 WHERE key=?`, key)
	d.SealStructure(key)
	f.refresh()
	change := f.page(sessions.ExportOptions{After: last.Next})
	changedMessage := f.page(sessions.ExportOptions{After: change.Next})
	if len(changedMessage.Items) != 1 || changedMessage.Items[0].Message.Text != full+" changed" || changedMessage.Items[0].G != 0 {
		t.Fatal("change beyond text limit missed")
	}
}

func TestExportSchemaMigrationOnce(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:one", 100, exportMessage(1, 1, "one"))
	f.refresh()
	epoch, err := f.svc.Store.Epoch()
	if err != nil {
		t.Fatal(err)
	}
	_, rev, _, err := f.svc.Store.Lookup(exportContext, "desk", "agent:one")
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO nodes(node,name,epoch,cursor,synced_at) VALUES ('remote','remote','peer-epoch',99,1000)`)
	// Reproduce a Phase 1 store, without changing its feed or identity.
	f.sql(`DROP TABLE export_log; DROP TABLE export_scan; DROP TABLE export_messages; DROP TABLE export_state;
 ALTER TABLE sessions DROP COLUMN tokens; ALTER TABLE sessions DROP COLUMN files; ALTER TABLE sessions DROP COLUMN structure;
 UPDATE meta SET v='1' WHERE k='schema'`)
	f.restart()
	if f.number(`SELECT v FROM meta WHERE k='schema'`) != 2 {
		t.Fatal("migration did not advance schema")
	}
	if f.number(`SELECT count(*) FROM nodes WHERE node='remote' AND epoch='peer-epoch' AND cursor=0 AND synced_at=1000`) != 1 {
		t.Fatal("phase 2 migration failed to replay remote metadata")
	}
	f.refresh()
	_, after, _, err := f.svc.Store.Lookup(exportContext, "desk", "agent:one")
	if err != nil || rev >= after {
		t.Fatal("phase 2 migration did not republish the feed", rev, after, err)
	}
	page := f.page(sessions.ExportOptions{})
	f.restart()
	f.refresh()
	if again := f.page(sessions.ExportOptions{}); page.Epoch != epoch || !reflect.DeepEqual(page, again) {
		t.Fatal("reopen reran migration or discarded export state")
	}
	f.sql(`UPDATE meta SET v='99' WHERE k='schema'`)
	f.svc.Store.Close()
	if store, err := sessions.OpenStore(f.path, time.Now); err == nil {
		store.Close()
		t.Fatal("unknown schema was silently accepted")
	}
}

func TestExportOptionsAndJSON(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:one", 100, exportMessage(1, 0, "one"))
	page := f.page(sessions.ExportOptions{})
	raw, err := json.Marshal(page.Items[1])
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"cursor", "type", "key", "agent", "g", "seq", "rev", "message"} {
		if _, ok := object[field]; !ok {
			t.Fatal("required wire field missing", field)
		}
	}
	if _, ok := object["session"]; ok {
		t.Fatal("message item contains session")
	}
}

func TestExportUnstructuredFullText(t *testing.T) {
	full := strings.Repeat("文", sessions.MaxText+10)
	for _, reader := range []string{"wake", "acp"} {
		t.Run(reader, func(t *testing.T) {
			f := newExportFixture(t)
			dir := t.TempDir()
			if reader == "wake" {
				d := waketest.New(t, dir)
				d.Put(waketest.Session{Key: "agent:example", Agent: "agent"}, waketest.Message{Role: "user", Text: full})
				x := wake.Open(d.Path)
				defer x.Close()
				f.svc.Source = x
			} else {
				d := acptest.New(t, dir, "example")
				d.Put(0, "user", full, "", "", 1000)
				f.svc.Source = antigravity.Open(dir)
			}
			page := f.page(sessions.ExportOptions{})
			var got *sessions.Message
			for _, item := range page.Items {
				if item.Message != nil {
					got = item.Message
				}
			}
			if got == nil || got.Text != full || got.Truncated {
				t.Fatal("unstructured export cut text")
			}
		})
	}
}

func TestExportEpochProtectsLostFirstResponse(t *testing.T) {
	f := newExportFixture(t)
	for i := int64(1); i <= 5; i++ {
		f.src.put(fmt.Sprintf("agent:%d", i), 100, exportMessage(1, 1, "text"))
	}
	initial := f.page(sessions.ExportOptions{}) // response lost, or another caller initialized
	ack := int64(5)
	page := f.page(sessions.ExportOptions{Epoch: "previous-epoch", After: 5, Ack: &ack})
	if page.Epoch != initial.Epoch || page.Next != 10 || len(page.Items) != 10 || f.number(`SELECT count(*) FROM export_log`) != 10 {
		t.Fatal("stale epoch pruned unseen items", page)
	}
	page = f.page(sessions.ExportOptions{Epoch: page.Epoch, After: 5, Ack: &ack})
	if len(page.Items) != 5 || f.number(`SELECT count(*) FROM export_log`) != 5 {
		t.Fatal("matching epoch ack failed")
	}
}

func TestExportBaselineStampOnly(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:old", 10, exportMessage(1, 1, "old"))
	w := int64(100)
	if len(f.page(sessions.ExportOptions{Since: &w}).Items) != 0 {
		t.Fatal("baseline exported")
	}
	f.src.changed("agent:old")
	f.refresh()
	if len(f.page(sessions.ExportOptions{}).Items) != 0 || f.number(`SELECT logged FROM export_scan WHERE key='agent:old'`) != 0 {
		t.Fatal("stamp-only change exported history")
	}
	meta := f.src.entries["agent:old"]
	meta.Title = "new title"
	meta.Archived = true
	f.src.entries["agent:old"] = meta
	f.refresh()
	if len(f.page(sessions.ExportOptions{}).Items) != 0 {
		t.Fatal("metadata-only baseline exported")
	}
	f.src.rows["agent:old"][0].Text = "changed"
	f.src.changed("agent:old")
	f.refresh()
	if len(f.page(sessions.ExportOptions{}).Items) != 2 {
		t.Fatal("real baseline change missed")
	}
}

func TestExportSessionFailureDoesNotBlockOtherSessions(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:bad", 10, exportMessage(1, 1, "old"))
	f.src.put("agent:good", 200, exportMessage(1, 1, "new"))
	f.src.failKey = "agent:bad"
	f.src.readErr = errors.New("unreadable fixture")
	w := int64(100)
	page := f.page(sessions.ExportOptions{Since: &w})
	if len(page.Items) != 2 || page.Items[0].Key != "agent:good" || !strings.Contains(f.logs.String(), "unreadable fixture") {
		t.Fatal("other session blocked", page)
	}
	f.src.readErr = nil
	f.src.put("agent:late", 1, exportMessage(1, 1, "late old file"))
	f.restart()
	f.refresh()
	page = f.page(sessions.ExportOptions{After: page.Next})
	if len(page.Items) != 2 || page.Items[0].Key != "agent:late" || f.number(`SELECT logged FROM export_scan WHERE key='agent:bad'`) != 0 {
		t.Fatal("initial cutoff changed across retry", page)
	}
	f.src.readErr = errors.New("second failure")
	f.src.failKey = "agent:good"
	f.src.changed("agent:good")
	f.svc.Peers = func() []sessions.Peer { return nil }
	f.svc.SyncLocal(exportContext)
	listing, err := f.svc.List(exportContext, sessions.Query{})
	if err != nil || len(listing.Nodes) != 1 || listing.Nodes[0].Note != "" {
		t.Fatal("export failure marked feed unshared", listing, err)
	}
}

func TestExportSourceReadReleasesStoreConnection(t *testing.T) {
	f := newExportFixture(t)
	f.src.put("agent:one", 100, exportMessage(1, 1, "text"))
	// A source read can synchronously ask for the peer feed. No sleep or timing
	// threshold: holding sessions.db's only connection would deadlock this call.
	f.src.beforeRead = func(string) {
		if _, err := f.svc.LocalChanges(exportContext, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.page(sessions.ExportOptions{}).Items) != 2 {
		t.Fatal("missing export")
	}
}

func TestExportLimitsAndDeletedMetadata(t *testing.T) {
	f := newExportFixture(t)
	var messages []sessions.Message
	for i := int64(1); i <= 510; i++ {
		messages = append(messages, exportMessage(i, i, "text"))
	}
	f.src.put("agent:one", 100, messages...)
	n := int64(7)
	meta := f.src.entries["agent:one"]
	meta.Tokens = &sessions.Tokens{Total: &n}
	meta.Files = []string{"/home/alice/a.go"}
	f.src.entries[meta.Key] = meta
	page := f.page(sessions.ExportOptions{})
	if len(page.Items) != 100 || !page.More {
		t.Fatal("default limit", len(page.Items))
	}
	if page = f.page(sessions.ExportOptions{Limit: 999}); len(page.Items) != 500 || !page.More {
		t.Fatal("max limit", len(page.Items))
	}
	last := f.page(sessions.ExportOptions{After: page.Next})
	delete(f.src.entries, meta.Key)
	f.refresh()
	tombstone := f.page(sessions.ExportOptions{After: last.Next})
	if len(tombstone.Items) != 1 || !tombstone.Items[0].Deleted || tombstone.Items[0].Session.Tokens == nil || *tombstone.Items[0].Session.Tokens.Total != n || !reflect.DeepEqual(tombstone.Items[0].Session.Files, meta.Files) {
		t.Fatal("deleted snapshot lost metadata", tombstone)
	}
}

func TestExportStaleStructureKeepsRichSnapshots(t *testing.T) {
	for _, stale := range []string{"stamp", "row-count", "backfill"} {
		t.Run(stale, func(t *testing.T) {
			f := newExportFixture(t)
			d := waketest.New(t, t.TempDir())
			const key = "codex:example"
			d.Put(waketest.Session{Key: key, Agent: "codex"}, waketest.Message{Role: "assistant", Text: "indexed prefix", Time: 1000})
			rich := sessions.Message{Text: strings.Repeat("full", 10000), Thinking: "reason", Usage: &sessions.Usage{Input: 7}, Calls: []sessions.Call{{Tool: "edit", Files: []sessions.File{{Path: "/home/alice/a.go", Op: "change"}}}}}
			d.Structure(key, 1, rich)
			x := wake.Open(d.Path)
			defer x.Close()
			f.svc.Source = x
			initial := f.page(sessions.ExportOptions{})
			f.page(sessions.ExportOptions{After: initial.Next, Ack: &initial.Next}) // protection survives pruning
			switch stale {
			case "stamp":
				d.Exec(`UPDATE sessions SET file_mtime=file_mtime+1 WHERE key=?`, key)
			case "row-count":
				d.Exec(`UPDATE structure_sessions SET rows=0 WHERE session_key=?`, key)
			case "backfill":
				d.Exec(`DELETE FROM message_extras; DELETE FROM tool_calls; DELETE FROM tool_files; DELETE FROM structure_sessions`)
			}
			f.refresh()
			degraded := f.page(sessions.ExportOptions{After: initial.Next})
			if len(degraded.Items) != 1 || degraded.Items[0].Type != "session" || degraded.Items[0].Session.ToolCalls == nil || degraded.Items[0].Session.Tokens == nil || len(degraded.Items[0].Session.Files) != 1 {
				t.Fatal("downgrade overwrote rich snapshot", degraded)
			}
			d.Exec(`INSERT INTO messages(session_key,seq,role,text,ts) VALUES (?,2,'user','new question',2000)`, key)
			d.Exec(`UPDATE sessions SET file_mtime=file_mtime+1,message_count=2 WHERE key=?`, key)
			f.refresh()
			added := f.page(sessions.ExportOptions{After: degraded.Next})
			if len(added.Items) != 2 || added.Items[1].Seq != 2 || added.Items[1].Message.Text != "new question" {
				t.Fatal("new seq suppressed", added)
			}
			rich.Text += " completed"
			d.Structure(key, 1, rich)
			d.Structure(key, 2, sessions.Message{})
			f.refresh()
			recovered := f.page(sessions.ExportOptions{After: added.Next})
			if len(recovered.Items) != 3 || recovered.Items[1].Message.Text != rich.Text || recovered.Items[1].Message.Usage == nil || recovered.Items[1].G != 0 || recovered.Items[2].Message.Kind != "text" {
				t.Fatal("recovered structure not emitted", recovered)
			}
		})
	}
}

func TestExportDefersRenumberingWhileDegraded(t *testing.T) {
	f := newExportFixture(t)
	rich := exportMessage(1, 1, "full body")
	rich.Kind = "text"
	f.src.put("agent:one", 100, rich)
	initial := f.page(sessions.ExportOptions{})
	plain := exportMessage(2, 2, "new text")
	f.src.rows["agent:one"] = []sessions.Message{{ID: 1, Seq: 1, Role: rich.Role, Time: rich.Time, Text: "prefix"}, plain}
	f.src.changed("agent:one")
	f.refresh()
	added := f.page(sessions.ExportOptions{After: initial.Next})
	f.src.rows["agent:one"][1].Time++
	f.src.changed("agent:one")
	f.refresh()
	degraded := f.page(sessions.ExportOptions{After: added.Next})
	if len(degraded.Items) != 1 || degraded.Items[0].G != 0 {
		t.Fatal("renumbered before full source was available", degraded)
	}
	f.src.rows["agent:one"][0] = rich
	f.src.rows["agent:one"][1].Kind = "text"
	f.src.changed("agent:one")
	f.refresh()
	restored := f.page(sessions.ExportOptions{After: degraded.Next})
	if len(restored.Items) != 3 || restored.Items[0].G != 1 || restored.Items[1].Message.Text != rich.Text {
		t.Fatal("full new generation missing", restored)
	}
}

func TestExportFirstBaselineChangeDuringDegradation(t *testing.T) {
	f := newExportFixture(t)
	const key = "agent:old"
	old := exportMessage(1, 1, "full history")
	old.Structure = 2
	old.Usage = &sessions.Usage{Input: 7}
	f.src.put(key, 10, old)
	w := int64(100)
	if len(f.page(sessions.ExportOptions{Since: &w}).Items) != 0 {
		t.Fatal("baseline exported")
	}
	plain := old
	plain.Structure = 0
	plain.Kind = ""
	plain.Usage = nil
	plain.Text = "prefix"
	f.src.rows[key] = []sessions.Message{plain}
	f.src.changed(key)
	f.refresh()
	if len(f.page(sessions.ExportOptions{}).Items) != 0 || f.number(`SELECT structured FROM export_messages WHERE key='agent:old' AND seq=1`) != 2 {
		t.Fatal("downgrade changed baseline")
	}
	next := sessions.Message{ID: 2, Seq: 2, Role: "user", Time: 200, Text: "continue"}
	f.src.rows[key] = append(f.src.rows[key], next)
	f.src.changed(key)
	f.refresh()
	first := f.page(sessions.ExportOptions{})
	if len(first.Items) != 3 || first.Items[1].Message.Text != "prefix" || first.Items[2].Message.Text != "continue" || f.number(`SELECT structured FROM export_messages WHERE key='agent:old' AND seq=1`) != 0 {
		t.Fatal("first emission lost baseline history", first)
	}
	f.src.rows[key][0] = old
	f.src.rows[key][1].Kind = "text"
	f.src.rows[key][1].Structure = 2
	f.src.changed(key)
	f.refresh()
	restored := f.page(sessions.ExportOptions{After: first.Next})
	if len(restored.Items) != 3 || restored.Items[1].Message.Text != "full history" || restored.Items[1].Message.Usage == nil {
		t.Fatal("baseline history never recovered", restored)
	}
}

func TestExportInitializationIncludesSourceAheadOfFeed(t *testing.T) {
	f := newExportFixture(t)
	const key = "agent:old"
	f.src.put(key, 10, exportMessage(1, 1, "old"))
	f.src.beforeRead = func(k string) {
		f.src.beforeRead = nil
		f.src.rows[k] = append(f.src.rows[k], sessions.Message{ID: 2, Seq: 2, Role: "user", Time: 100, Text: "arrived after watermark"})
	}
	w := int64(100)
	page := f.page(sessions.ExportOptions{Since: &w})
	if len(page.Items) != 3 || page.Items[2].Message.Text != "arrived after watermark" {
		t.Fatal("source advanced into baseline", page)
	}
	f.src.changed(key)
	f.refresh()
	after := f.page(sessions.ExportOptions{After: page.Next})
	if len(after.Items) != 1 || after.Items[0].Type != "session" {
		t.Fatal("feed catchup changed message delivery", after)
	}
}

func TestExportFormatRollbackPreservesLevelTwo(t *testing.T) {
	f := newExportFixture(t)
	d := waketest.New(t, t.TempDir())
	const key = "codex:example"
	total := int64(19)
	d.Put(waketest.Session{Key: key, Agent: "codex", Tokens: &total}, waketest.Message{Role: "assistant", Text: "answer", Time: 10})
	d.Structure(key, 1, sessions.Message{Usage: &sessions.Usage{Input: 7}, Calls: []sessions.Call{{Tool: "edit", Files: []sessions.File{{Path: "/home/alice/a.go", Op: "change"}}}}})
	x := wake.Open(d.Path)
	defer x.Close()
	f.svc.Source = x
	first := f.page(sessions.ExportOptions{})
	d.Exec(`UPDATE schema_meta SET value='1' WHERE key='structure_format'; UPDATE structure_sessions SET format='1'`)
	f.refresh()
	rollback := f.page(sessions.ExportOptions{After: first.Next})
	if len(rollback.Items) != 1 || rollback.Items[0].Session.Tokens.Input == nil || *rollback.Items[0].Session.Tokens.Input != 7 || len(rollback.Items[0].Session.Files) != 1 || f.number(`SELECT structured FROM export_messages WHERE key='codex:example'`) != 2 {
		t.Fatal("format rollback erased phase 2", rollback)
	}
	d.Exec(`UPDATE schema_meta SET value='2' WHERE key='structure_format'; UPDATE structure_sessions SET format='2'`)
	f.refresh()
	recovered := f.page(sessions.ExportOptions{After: rollback.Next})
	if len(recovered.Items) != 1 {
		t.Fatal("unchanged rich message resent", recovered)
	}
}

func TestExportEmptySessionDowngrade(t *testing.T) {
	f := newExportFixture(t)
	const key = "agent:empty"
	f.src.put(key, 10)
	zero := int64(0)
	count := int64(7)
	meta := f.src.entries[key]
	meta.Structure = 2
	meta.ToolCalls = &zero
	meta.Tokens = &sessions.Tokens{Total: &count}
	f.src.entries[key] = meta
	first := f.page(sessions.ExportOptions{})
	meta.Structure = 0
	meta.ToolCalls = nil
	meta.Tokens = nil
	meta.Stamp += "changed"
	f.src.entries[key] = meta
	f.refresh()
	page := f.page(sessions.ExportOptions{After: first.Next})
	if len(page.Items) != 1 || page.Items[0].Session.ToolCalls == nil || page.Items[0].Session.Tokens == nil || f.number(`SELECT structured FROM export_scan WHERE key='agent:empty'`) != 2 {
		t.Fatal("empty-session downgrade lost metadata", page)
	}
}

func TestExportEpochMismatchEmptyPageStartsAtZero(t *testing.T) {
	f := newExportFixture(t)
	f.page(sessions.ExportOptions{})
	ack := int64(99)
	page := f.page(sessions.ExportOptions{Epoch: "old-store", After: 99, Ack: &ack})
	if len(page.Items) != 0 || page.Next != 0 || page.More {
		t.Fatal("old cursor leaked into empty page", page)
	}
}

func TestExportCancellationStopsSessionLoop(t *testing.T) {
	f := newExportFixture(t)
	for _, key := range []string{"agent:a", "agent:b", "agent:c"} {
		f.src.put(key, 10, exportMessage(1, 1, "text"))
	}
	ctx, cancel := context.WithCancel(exportContext)
	defer cancel()
	f.src.beforeRead = func(string) { cancel() }
	f.src.readErr = context.Canceled
	if _, err := f.svc.Export(ctx, sessions.ExportOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.src.reads != 1 || strings.Count(f.logs.String(), "export session") != 1 {
		t.Fatal("continued reading after cancellation", f.src.reads, f.logs.String())
	}
}

func TestExportBatchSavepointIsolatesFailure(t *testing.T) {
	f := newExportFixture(t)
	for _, key := range []string{"agent:a", "agent:b", "agent:c"} {
		f.src.put(key, 10, exportMessage(1, 1, "text"))
	}
	f.sql(`CREATE TRIGGER fail_one BEFORE INSERT ON export_scan WHEN NEW.key='agent:b' BEGIN SELECT RAISE(ABORT,'one failure'); END`)
	page := f.page(sessions.ExportOptions{})
	if len(page.Items) != 4 || page.Items[0].Key != "agent:a" || page.Items[2].Key != "agent:c" || f.number(`SELECT count(*) FROM export_messages WHERE key='agent:b'`) != 0 {
		t.Fatal("failed session affected neighbours", page)
	}
	f.sql(`DROP TRIGGER fail_one`)
	f.refresh()
	next := f.page(sessions.ExportOptions{After: page.Next})
	if len(next.Items) != 2 || next.Items[0].Key != "agent:b" {
		t.Fatal("failed session not retried", next)
	}
}

func TestExportBatchBoundaries(t *testing.T) {
	t.Run("session count", func(t *testing.T) {
		f := newExportFixture(t)
		for i := 0; i < 101; i++ {
			f.src.put(fmt.Sprintf("agent:%03d", i), 10)
		}
		f.src.beforeRead = func(key string) {
			if key == "agent:100" && f.number(`SELECT count(*) FROM export_scan`) != 100 {
				t.Fatal("first 100 sessions not committed before next source read")
			}
		}
		f.page(sessions.ExportOptions{})
	})
	t.Run("byte budget", func(t *testing.T) {
		f := newExportFixture(t)
		f.src.put("agent:a", 10, exportMessage(1, 1, strings.Repeat("x", sessions.PageBudget)))
		f.src.put("agent:b", 10)
		f.src.beforeRead = func(key string) {
			if key == "agent:b" && f.number(`SELECT count(*) FROM export_scan`) != 1 {
				t.Fatal("full byte budget not committed before next source read")
			}
		}
		f.page(sessions.ExportOptions{})
	})
}

func TestExportDeletionRetainsLatestSnapshot(t *testing.T) {
	f := newExportFixture(t)
	const key = "agent:one"
	f.src.put(key, 100, exportMessage(1, 1, "message"))
	first := f.page(sessions.ExportOptions{})
	meta := f.src.entries[key]
	calls, errs, input := int64(2), int64(1), int64(7)
	meta.Title = "Latest title"
	meta.Project = "parser"
	meta.ProjectPath = "/home/alice/src/parser"
	meta.App = "terminal"
	meta.CreatedAt = 20
	meta.UpdatedAt = 200
	meta.ToolCalls = &calls
	meta.ToolErrors = &errs
	meta.Tokens = &sessions.Tokens{Input: &input}
	meta.Files = []string{"/home/alice/src/parser/main.go"}
	f.src.entries[key] = meta
	f.refresh()
	latest := f.page(sessions.ExportOptions{After: first.Next})
	if len(latest.Items) != 1 {
		t.Fatal(latest)
	}
	f.page(sessions.ExportOptions{After: latest.Next, Ack: &latest.Next})
	delete(f.src.entries, key)
	f.refresh()
	deleted := f.page(sessions.ExportOptions{After: latest.Next})
	if len(deleted.Items) != 1 || !deleted.Items[0].Deleted || deleted.Items[0].Rev <= latest.Items[0].Rev || !reflect.DeepEqual(deleted.Items[0].Session, latest.Items[0].Session) {
		t.Fatal("deletion did not retain latest snapshot", deleted)
	}
	feed, err := f.svc.Store.Changes(exportContext, "desk", latest.Items[0].Rev, 1)
	if err != nil || len(feed) != 1 || feed[0].Tokens != nil || len(feed[0].Files) != 0 {
		t.Fatal("peer deletion contract changed", err)
	}
}

func TestExportRollbackUsesFreshCallCounts(t *testing.T) {
	f := newExportFixture(t)
	d := waketest.New(t, t.TempDir())
	const key = "codex:example"
	d.Put(waketest.Session{Key: key, Agent: "codex"}, waketest.Message{Role: "assistant", Text: "first", Time: 10})
	d.Structure(key, 1, sessions.Message{Usage: &sessions.Usage{Input: 7}, Calls: []sessions.Call{{Tool: "edit", Files: []sessions.File{{Path: "/home/alice/a.go", Op: "change"}}}}})
	x := wake.Open(d.Path)
	defer x.Close()
	f.svc.Source = x
	first := f.page(sessions.ExportOptions{})
	d.Append(key, waketest.Message{Role: "assistant", Text: "second", Time: 20})
	d.StructureV1(key, 2, sessions.Message{Calls: []sessions.Call{{Tool: "shell", Error: true}}})
	f.refresh()
	page := f.page(sessions.ExportOptions{After: first.Next})
	if len(page.Items) != 2 {
		t.Fatal(page)
	}
	meta := page.Items[0].Session
	if meta.ToolCalls == nil || *meta.ToolCalls != 2 || meta.ToolErrors == nil || *meta.ToolErrors != 1 || meta.Tokens == nil || *meta.Tokens.Input != 7 || len(meta.Files) != 1 || f.number(`SELECT structured FROM export_scan WHERE key='codex:example'`) != 2 {
		t.Fatal("rollback retained stale call counts", meta)
	}
}

func TestExportACPTranscriptLossPreservesStructure(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint("empty=", empty), func(t *testing.T) {
			f := newExportFixture(t)
			root := t.TempDir()
			dir := filepath.Join(root, "conversations")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			d := acptest.New(t, dir, "example")
			if empty {
				d.Transcript()
			} else {
				d.CWD("/home/alice/src/parser")
				d.Put(0, "user", "Fix the parser", "", "", 1000)
				d.Put(1, "assistant", "Done", "", "model-x", 2000)
				d.Transcript(
					map[string]any{"step_index": 0, "type": "USER_INPUT"},
					map[string]any{"step_index": 1, "type": "PLANNER_RESPONSE", "thinking": "Check the loop", "input_tokens": 7, "output_tokens": 5, "tool_calls": []any{map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": `"/home/alice/src/parser/main.go"`}}}},
					map[string]any{"step_index": 2, "type": "CODE_ACTION", "content": "written"},
				)
			}
			f.svc.Source = antigravity.Open(dir)
			first := f.page(sessions.ExportOptions{})
			want := first.Items[0].Session
			if want.ToolCalls == nil || (!empty && (want.Tokens == nil || len(want.Files) != 1 || len(first.Items) != 3)) {
				t.Fatal("missing initial ACP structure", first)
			}
			path := filepath.Join(root, "brain", "example", ".system_generated", "logs", "transcript.jsonl")
			if err := os.Rename(path, path+".away"); err != nil {
				t.Fatal(err)
			}
			f.refresh()
			lost := f.page(sessions.ExportOptions{After: first.Next})
			if len(lost.Items) != 1 || lost.Items[0].Type != "session" || !reflect.DeepEqual(lost.Items[0].Session, want) {
				t.Fatal("ACP downgrade overwrote snapshot", lost)
			}
			if err := os.Rename(path+".away", path); err != nil {
				t.Fatal(err)
			}
			f.refresh()
			recovered := f.page(sessions.ExportOptions{After: lost.Next})
			// The revision notification remains; unchanged message snapshots do not
			// reappear when the original transcript is readable again.
			if len(recovered.Items) != 1 || recovered.Items[0].Type != "session" || !reflect.DeepEqual(recovered.Items[0].Session, want) {
				t.Fatal("ACP recovery resent content", recovered)
			}
			f.refresh()
			if len(f.page(sessions.ExportOptions{After: recovered.Next}).Items) != 0 {
				t.Fatal("unchanged ACP recovery kept exporting")
			}
		})
	}
}

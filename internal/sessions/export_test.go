package sessions_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

var exportContext = context.Background()

type exportSource struct {
	entries map[string]sessions.Session
	rows    map[string][]sessions.Message
	reads   int
	err     error
	readErr error
	failAt  int64
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
	if s.readErr != nil && after >= s.failAt {
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
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	return sessions.Message{ID: id, Seq: seq, Role: "assistant", Time: 10 + seq, Text: text, Kind: "text"}
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
	if initial.Items[0].Type != "session" || initial.Items[0].Message != nil || initial.Items[0].Session.Messages != 1 ||
		initial.Items[1].Type != "message" || initial.Items[1].Session != nil || !reflect.DeepEqual(*initial.Items[1].Message, m) {
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
	other := watermark + 1
	if _, err := f.svc.Export(exportContext, sessions.ExportOptions{Since: &other}); !errors.Is(err, sessions.ErrExportOptions) {
		t.Fatal("watermark silently changed", err)
	}
	if f.number(`SELECT since FROM export_state`) != watermark {
		t.Fatal("watermark not persisted")
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
			if _, err := f.svc.Export(exportContext, sessions.ExportOptions{}); err == nil {
				t.Fatal("invalid source page was silently accepted")
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
	if err := f.svc.RefreshLocal(exportContext); err == nil || !strings.Contains(err.Error(), "scan position failure") {
		t.Fatal("injected failure was swallowed", err)
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
	if _, err := f.svc.Export(exportContext, sessions.ExportOptions{Since: &w}); err == nil {
		t.Fatal("initialization failure swallowed")
	}
	for _, table := range []string{"export_log", "export_scan", "export_messages", "export_state"} {
		if f.number(`SELECT COUNT(*) FROM `+table) != 0 {
			t.Fatal("initialization leaked", table)
		}
	}
	f.sql(`DROP TRIGGER fail_export_init`)
	f.restart()
	w = 101
	page := f.page(sessions.ExportOptions{Since: &w})
	if len(page.Items) != 0 || f.number(`SELECT since FROM export_state`) != w {
		t.Fatal("failed initialization fixed the wrong watermark", page)
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
	if _, err := f.svc.Export(exportContext, sessions.ExportOptions{}); !errors.Is(err, f.src.readErr) {
		t.Fatal("second-page error swallowed", err)
	}
	if f.number(`SELECT COUNT(*) FROM export_log`) != 0 || f.number(`SELECT COUNT(*) FROM export_state`) != 0 {
		t.Fatal("partial source read committed")
	}
	f.src.readErr = nil
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
	// Reproduce a Phase 1 store, without changing its feed or identity.
	f.sql(`DROP TABLE export_log; DROP TABLE export_scan; DROP TABLE export_messages; DROP TABLE export_state;
 UPDATE meta SET v='1' WHERE k='schema'`)
	f.restart()
	if f.number(`SELECT v FROM meta WHERE k='schema'`) != 2 {
		t.Fatal("migration did not advance schema")
	}
	f.refresh()
	_, after, _, err := f.svc.Store.Lookup(exportContext, "desk", "agent:one")
	if err != nil || rev != after {
		t.Fatal("export migration republished the feed", rev, after, err)
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
	negative := int64(-1)
	for _, options := range []sessions.ExportOptions{{After: -1}, {Limit: -1}, {Ack: &negative}} {
		if _, err := f.svc.Export(exportContext, options); !errors.Is(err, sessions.ErrExportOptions) {
			t.Fatal("invalid options accepted", options, err)
		}
	}
	page := f.page(sessions.ExportOptions{})
	tooHigh := page.Next + 1
	if _, err := f.svc.Export(exportContext, sessions.ExportOptions{Ack: &tooHigh}); !errors.Is(err, sessions.ErrExportOptions) {
		t.Fatal("future ack accepted", err)
	}
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

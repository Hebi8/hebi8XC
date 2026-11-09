package antigravity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/antigravity/acptest"
)

var ctx = context.Background()

func TestSessionsAndMessages(t *testing.T) {
	dir := t.TempDir()
	d := acptest.New(t, dir, "example")
	d.CWD("/home/alice/project")
	d.Put(0, "user", "你好\nPlease fix the parser.", "", "", 1700000000123)
	d.Put(2, "assistant", "draft", "Finished.\n```go\nreturn nil\n```", "example-model", 1700000001123)
	d.Put(3, "assistant", "", "", "example-model", 1700000002123) // tool-only response
	d.Exec("INSERT INTO steps VALUES (4,21,3,X'',X'')")
	x := Open(dir)
	list, err := x.Sessions(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	s := list[0]
	if s.Key != "antigravity-acp:example" || s.Agent != Agent || s.Title != "你好 Please fix the parser." || s.Project != "project" || s.ProjectPath != "/home/alice/project" || s.Model != "example-model" || s.Messages != 2 || s.CreatedAt != 1700000000123 || s.UpdatedAt != 1700000002173 {
		t.Fatalf("session: %+v", s)
	}
	msgs, more, err := x.Messages(ctx, s.Key, 0, 1, sessions.PageBudget)
	if err != nil || !more || len(msgs) != 1 || msgs[0].ID != 1 || msgs[0].Seq != 0 || msgs[0].Text != "你好\nPlease fix the parser." || msgs[0].Role != "user" || msgs[0].Time != 1700000000123 {
		t.Fatalf("first: %+v %v %v", msgs, more, err)
	}
	msgs, more, err = x.Messages(ctx, s.Key, 1, 1, sessions.PageBudget)
	if err != nil || more || len(msgs) != 1 || msgs[0].ID != 3 || msgs[0].Text != "Finished.\n```go\nreturn nil\n```" || msgs[0].Role != "assistant" {
		t.Fatalf("last: %+v %v %v", msgs, more, err)
	}
	msgs, more, err = x.Messages(ctx, s.Key, 3, 10, sessions.PageBudget)
	if err != nil || more || len(msgs) != 0 {
		t.Fatalf("end: %+v %v %v", msgs, more, err)
	}
}

func TestWALSnapshotAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	d := acptest.New(t, dir, "live")
	d.Put(0, "user", "committed", "", "", 1000)
	x := Open(dir)
	before, err := x.Sessions(ctx)
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	writer, err := d.SQL.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec("UPDATE steps SET step_payload=? WHERE idx=0", acptest.Bytes(19, acptest.Bytes(2, []byte("in flight")))); err != nil {
		t.Fatal(err)
	}
	msgs, _, err := x.Messages(ctx, "antigravity-acp:live", 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "committed" {
		t.Fatal(msgs, err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	after, err := x.Sessions(ctx)
	if err != nil || len(after) != 1 || after[0].Stamp == before[0].Stamp {
		t.Fatal("WAL edit not reflected", err)
	}
	msgs, _, err = x.Messages(ctx, "antigravity-acp:live", 0, 10, sessions.PageBudget)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "in flight" {
		t.Fatal(msgs, err)
	}
	db, tx, err := open(ctx, d.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM steps"); err == nil {
		t.Fatal("reader permits writes")
	}
}

func TestMissingEmptyAndBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	x := Open(filepath.Join(dir, "later"))
	if _, err := x.Sessions(ctx); !errors.Is(err, sessions.ErrNoIndex) {
		t.Fatal(err)
	}
	if err := os.Mkdir(x.dir, 0700); err != nil {
		t.Fatal(err)
	}
	acptest.New(t, x.dir, "empty")
	if err := os.WriteFile(filepath.Join(x.dir, "broken.db"), []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := acptest.New(t, x.dir, "bad-proto")
	bad.Exec("INSERT INTO steps VALUES (0,14,3,X'',X'ff')")
	list, err := x.Sessions(ctx)
	if err != nil || len(list) != 1 || list[0].Messages != 0 {
		t.Fatal(list, err)
	}
	for _, key := range []string{"antigravity-acp:missing", "antigravity:empty", "antigravity-acp:../outside", "antigravity-acp:..\\outside", "antigravity-acp:"} {
		if _, _, err := x.Messages(ctx, key, 0, 10, 1000); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("%q: %v", key, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := x.Sessions(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPageLimits(t *testing.T) {
	dir := t.TempDir()
	d := acptest.New(t, dir, "pages")
	x := Open(dir)
	key := "antigravity-acp:pages"
	d.Put(0, "user", strings.Repeat("世", sessions.MaxText+1), "", "", 1000)
	d.Put(1, "assistant", "answer", "", "", 2000)
	msgs, more, err := x.Messages(ctx, key, 0, 10, 1)
	if err != nil || !more || len(msgs) != 1 || msgs[0].Truncated || utf8.RuneCountInString(msgs[0].Text) != sessions.MaxText+1 || !utf8.ValidString(msgs[0].Text) {
		t.Fatalf("large first: count=%d more=%v err=%v", len(msgs), more, err)
	}
	msgs, more, err = x.Messages(ctx, key, 1, 10, 1)
	if err != nil || more || len(msgs) != 1 || msgs[0].Truncated {
		t.Fatal(msgs, more, err)
	}
	for i := int64(2); i < 502; i++ {
		d.Put(i, "user", "small", "", "", 3000+i)
	}
	msgs, more, err = x.Messages(ctx, key, 1, 1000, sessions.PageBudget)
	if err != nil || !more || len(msgs) != sessions.MaxPage {
		t.Fatal(len(msgs), more, err)
	}
	last := msgs[len(msgs)-1].ID
	msgs, more, err = x.Messages(ctx, key, last, 1000, sessions.PageBudget)
	if err != nil || more || len(msgs) != 1 || msgs[0].ID != 502 {
		t.Fatal(msgs, more, err)
	}
	// SQL/decoder must not read through to a malformed later page.
	d.Exec("UPDATE steps SET step_payload=X'ff' WHERE idx=501")
	msgs, more, err = x.Messages(ctx, key, 0, 1, sessions.PageBudget)
	if err != nil || !more || len(msgs) != 1 {
		t.Fatal(len(msgs), more, err)
	}
	// JSON escaping counts towards the byte budget, even for short text.
	d.Put(0, "user", strings.Repeat("\x00", 100), "", "", 1000)
	msgs, _, err = x.Messages(ctx, key, 0, 1, sessions.PageBudget)
	if err != nil {
		t.Fatal(err)
	}
	budget := sessions.MessageSize(msgs[0])
	msgs, more, err = x.Messages(ctx, key, 0, 10, budget)
	if err != nil || !more || len(msgs) != 1 {
		t.Fatal(len(msgs), more, err)
	}
}

func TestProtoBounds(t *testing.T) {
	for _, b := range [][]byte{{0}, {0xff}, {0x0a, 0x80}, {0x0a, 5, 1}, {0x0b}, {0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}} {
		p := proto{size: int64(len(b)), read: func(o, n int64) ([]byte, error) { return b[o : o+n], nil }}
		if _, err := p.child(1); err == nil {
			t.Fatalf("accepted %x", b)
		}
	}
	// A huge unknown field is skipped without asking the backing store to read it.
	b := acptest.Join(acptest.Bytes(50, make([]byte, 2<<20)), acptest.Bytes(1, []byte("known")))
	var read int64
	p := proto{size: int64(len(b)), read: func(o, n int64) ([]byte, error) { read += n; return b[o : o+n], nil }}
	child, err := p.child(1)
	if err != nil {
		t.Fatal(err)
	}
	text, cut, err := child.text(10)
	if err != nil || cut || text != "known" || read > 100 {
		t.Fatal(text, cut, err, read)
	}
}

func TestUnchangedStampReusesSession(t *testing.T) {
	dir := t.TempDir()
	d := acptest.New(t, dir, "cached")
	d.Put(0, "user", "First title", "", "", 1000)
	d.Exec("PRAGMA journal_mode=DELETE")
	if err := d.SQL.Close(); err != nil {
		t.Fatal(err)
	}
	x := Open(dir)
	before, err := x.Sessions(ctx)
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	st, err := os.Stat(d.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the contents with another valid database, preserving size and mtime.
	other := acptest.New(t, t.TempDir(), "replacement")
	other.Put(0, "user", "Other title", "", "", 1000)
	other.Exec("PRAGMA journal_mode=DELETE")
	if err := other.SQL.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(other.Path)
	if err != nil || int64(len(data)) != st.Size() {
		t.Fatal("replacement must have the same size", len(data), err)
	}
	if err := os.WriteFile(d.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(d.Path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if stamp, err := fileStamp(d.Path); err != nil || stamp != before[0].Stamp {
		t.Fatal("test must preserve the stamp", stamp, err)
	}
	if fresh, err := x.session(ctx, "cached.db"); err != nil || fresh.Title != "Other title" {
		t.Fatal("an uncached read must see the replacement", fresh, err)
	}
	after, err := x.Sessions(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal(after, err)
	}
}

func TestFailedReadRetainsSessionAndRetries(t *testing.T) {
	dir := t.TempDir()
	d := acptest.New(t, dir, "recover")
	d.Put(0, "user", "First title", "", "", 1000)
	x := Open(dir)
	before, err := x.Sessions(ctx)
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	d.Exec("UPDATE steps SET step_payload=X'ff' WHERE idx=0")
	stamp, err := fileStamp(d.Path)
	if err != nil || stamp == before[0].Stamp {
		t.Fatal("test must change the stamp", stamp, err)
	}
	if _, err := x.session(ctx, "recover.db"); err == nil {
		t.Fatal("test database should fail to parse")
	}
	after, err := x.Sessions(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("a failed read must keep all prior metadata, including stamp", after, err)
	}
	d.Put(0, "user", "Recovered title", "", "", 2000)
	after, err = x.Sessions(ctx)
	if err != nil || len(after) != 1 || after[0].Title != "Recovered title" || after[0].UpdatedAt != 2050 {
		t.Fatal("a repaired file must be retried", after, err)
	}
}

func TestDeletedFileRemovesSession(t *testing.T) {
	dir := t.TempDir()
	d := acptest.New(t, dir, "deleted")
	d.Put(0, "user", "First title", "", "", 1000)
	if err := d.SQL.Close(); err != nil {
		t.Fatal(err)
	}
	x := Open(dir)
	if list, err := x.Sessions(ctx); err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if err := os.Remove(d.Path); err != nil {
		t.Fatal(err)
	}
	if list, err := x.Sessions(ctx); err != nil || len(list) != 0 {
		t.Fatal("deleted file must leave the list", list, err)
	}
}

func TestTitles(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"whitespace", " \n\t你好\r\n  example\u3000project \n", "你好 example project"},
		{"unicode", strings.Repeat("世", 81), strings.Repeat("世", 80)},
		{"collapse before truncating", "first" + strings.Repeat(" \n", 300) + strings.Repeat("好", 80), "first " + strings.Repeat("好", 74)},
		{"blank", " \n\t", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			d := acptest.New(t, dir, "title")
			d.Put(0, "assistant", "Earlier assistant text", "", "", 1000)
			d.Put(1, "user", tc.text, "", "", 2000)
			d.Put(2, "user", "Later user text", "", "", 3000)
			list, err := Open(dir).Sessions(ctx)
			if err != nil || len(list) != 1 || list[0].Title != tc.want {
				t.Fatal(list, err)
			}
		})
	}
	dir := t.TempDir()
	d := acptest.New(t, dir, "assistant-only")
	d.Put(0, "assistant", "Only assistant text", "", "", 1000)
	if list, err := Open(dir).Sessions(ctx); err != nil || len(list) != 1 || list[0].Title != "" {
		t.Fatal(list, err)
	}
}

func BenchmarkSessions(b *testing.B) {
	dir := b.TempDir()
	for i := 0; i < 219; i++ {
		d := acptest.New(b, dir, fmt.Sprintf("example-%03d", i))
		d.Exec("BEGIN")
		for step := int64(0); step < 200; step++ {
			role := "user"
			if step%2 != 0 {
				role = "assistant"
			}
			d.Put(step, role, "Please update the example parser.", "", "example-model", 1700000000000+step)
		}
		d.Exec("COMMIT")
		if err := d.SQL.Close(); err != nil {
			b.Fatal(err)
		}
		// Keep the benchmark independent of the clock and filesystem precision.
		stamp := time.Unix(1700000000, 0)
		if err := os.Chtimes(d.Path, stamp, stamp); err != nil {
			b.Fatal(err)
		}
	}
	for _, name := range []string{"Cold", "Unchanged"} {
		b.Run(name, func(b *testing.B) {
			x := Open(dir)
			// Opening a closed WAL database can create its empty WAL. Warm
			// again so the first measured unchanged round has that stamp too.
			for warm := 0; warm < 2; warm++ {
				if list, err := x.Sessions(ctx); err != nil || len(list) != 219 {
					b.Fatal(len(list), err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if name == "Cold" {
					x = Open(dir)
				}
				if list, err := x.Sessions(ctx); err != nil || len(list) != 219 {
					b.Fatal(len(list), err)
				}
			}
		})
	}
}

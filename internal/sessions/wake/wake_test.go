package wake_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

var ctx = context.Background()

func keys(list []sessions.Session) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.Key)
	}
	slices.Sort(out)
	return out
}

func TestSessionsAreThoseOfThisMachine(t *testing.T) {
	db := waketest.New(t, t.TempDir())
	db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", Title: "Fix the parser", Project: "parser",
		Path: "/home/alice/src/parser", Branch: "main", Model: "model-x", UpdatedAt: 1_700_000_000_000},
		waketest.Message{Role: "user", Text: "hello"}, waketest.Message{Role: "assistant", Text: "hi"})
	db.Put(waketest.Session{Key: "codex:b2", Agent: "codex", Title: "Child", Parent: "claude:a1", Spawned: true,
		Archived: true, UpdatedAt: 1_700_000_100_000})
	// A session Wake mirrored from another machine over SSH.
	db.Put(waketest.Session{Key: "claude:otherbox:c3", Agent: "claude", Title: "Elsewhere", Host: "otherbox",
		UpdatedAt: 1_700_000_200_000}, waketest.Message{Role: "user", Text: "not from here"})

	x := wake.Open(db.Path)
	defer x.Close()
	list, err := x.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(list); !slices.Equal(got, []string{"claude:a1", "codex:b2"}) {
		t.Fatalf("sessions = %v, want the two of this machine", got)
	}
	slices.SortFunc(list, func(a, b sessions.Session) int { return strings.Compare(a.Key, b.Key) })
	a, b := list[0], list[1]
	a.Stamp = ""
	want := sessions.Session{Key: "claude:a1", Agent: "claude", Title: "Fix the parser", Project: "parser",
		ProjectPath: "/home/alice/src/parser", Branch: "main", Model: "model-x",
		CreatedAt: 1_699_999_999_000, UpdatedAt: 1_700_000_000_002, Messages: 2}
	if a != want {
		t.Errorf("session = %+v\n   want %+v", a, want)
	}
	if b.Parent != "claude:a1" || !b.Spawned || !b.Archived || b.Branch != "" || b.Model != "" {
		t.Errorf("child session = %+v", b)
	}

	// A mirrored session cannot be read either, whoever asks for its key.
	if msgs, err := x.Messages(ctx, "claude:otherbox:c3", 0, 10); err != nil || len(msgs) != 0 {
		t.Errorf("messages of a mirrored session: %v, %v", msgs, err)
	}
}

func TestStampFollowsTheContent(t *testing.T) {
	db := waketest.New(t, t.TempDir())
	db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", UpdatedAt: 1000}, waketest.Message{Role: "user", Text: "one"})
	x := wake.Open(db.Path)
	defer x.Close()
	stamp := func() string {
		list, err := x.Sessions(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("sessions: %v, %v", list, err)
		}
		return list[0].Stamp
	}
	first := stamp()
	if stamp() != first {
		t.Error("the stamp changed without a change")
	}
	// Wake writes while the reader has the index open.
	db.Append("claude:a1", waketest.Message{Role: "assistant", Text: "two"})
	if stamp() == first {
		t.Error("the stamp did not change when the session grew")
	}
}

func TestMessagesInPages(t *testing.T) {
	db := waketest.New(t, t.TempDir())
	db.Put(waketest.Session{Key: "other:z", Agent: "other", UpdatedAt: 1}, waketest.Message{Role: "user", Text: "another session"})
	db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", UpdatedAt: 1000},
		waketest.Message{Role: "user", Text: "one", Time: 111},
		waketest.Message{Role: "assistant", Text: "two"},
		waketest.Message{Role: "assistant", Text: "side", Sidechain: "sc-1"},
		waketest.Message{Role: "tool", Text: strings.Repeat("长", sessions.MaxText+5)},
		waketest.Message{Role: "user", Text: "five"})
	x := wake.Open(db.Path)
	defer x.Close()

	var all []sessions.Message
	after := int64(0)
	for {
		page, err := x.Messages(ctx, "claude:a1", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		after = page[len(page)-1].ID
	}
	if len(all) != 5 {
		t.Fatalf("read %d messages in pages of 2, want 5", len(all))
	}
	for i, m := range all {
		if m.Seq != int64(i+1) || (i > 0 && m.ID <= all[i-1].ID) {
			t.Errorf("message %d has seq %d and id %d: not in order", i, m.Seq, m.ID)
		}
	}
	if all[0].Text != "one" || all[0].Time != 111 || all[0].Role != "user" || all[1].Time != 0 {
		t.Errorf("first messages = %+v, %+v", all[0], all[1])
	}
	if all[2].Sidechain != "sc-1" || all[0].Sidechain != "" {
		t.Errorf("side thread: %q, %q", all[2].Sidechain, all[0].Sidechain)
	}
	if long := all[3]; !long.Truncated || len([]rune(long.Text)) != sessions.MaxText {
		t.Errorf("long message: truncated=%v, %d characters", long.Truncated, len([]rune(long.Text)))
	}
	if all[4].Truncated || all[4].Text != "five" {
		t.Errorf("last message = %+v", all[4])
	}
	if msgs, err := x.Messages(ctx, "claude:nope", 0, 10); err != nil || len(msgs) != 0 {
		t.Errorf("unknown session: %v, %v", msgs, err)
	}
	// A key is data, never SQL.
	if msgs, err := x.Messages(ctx, "x' OR '1'='1", 0, 10); err != nil || len(msgs) != 0 {
		t.Errorf("key with a quote: %v, %v", msgs, err)
	}
}

func TestUnknownStructureIsRefused(t *testing.T) {
	for name, breakIt := range map[string]string{
		"column sessions.host":     `ALTER TABLE sessions RENAME COLUMN host TO origin`,
		"column messages.text":     `ALTER TABLE messages RENAME COLUMN text TO body`,
		"column sessions.agent_id": `ALTER TABLE sessions RENAME COLUMN agent_id TO agent`,
		"table messages":           `DROP TABLE messages`,
	} {
		db := waketest.New(t, t.TempDir())
		db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", UpdatedAt: 1})
		db.Exec(breakIt)
		x := wake.Open(db.Path)
		_, err := x.Sessions(ctx)
		var schema *wake.SchemaError
		if !errors.As(err, &schema) || schema.Missing != name {
			t.Errorf("%s: err = %v, want a SchemaError naming it", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "cannot make out the structure of the Wake index") {
			t.Errorf("%s: message = %q", name, err)
		}
		if _, err := x.Messages(ctx, "claude:a1", 0, 1); !errors.As(err, &schema) {
			t.Errorf("%s: Messages err = %v", name, err)
		}
		x.Close()
	}

	// A file that is no database at all.
	path := filepath.Join(t.TempDir(), "wake.db")
	if err := os.WriteFile(path, []byte("this is not a database, just some text that is long enough"), 0o600); err != nil {
		t.Fatal(err)
	}
	x := wake.Open(path)
	defer x.Close()
	if _, err := x.Sessions(ctx); err == nil || errors.Is(err, sessions.ErrNoIndex) {
		t.Errorf("not a database: err = %v", err)
	}
}

// The index may be built after the node has started, and a broken one may
// be repaired: the reader looks again each time.
func TestIndexThatAppearsLater(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with space")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	x := wake.Open(filepath.Join(dir, "wake.db"))
	defer x.Close()
	if _, err := x.Sessions(ctx); !errors.Is(err, sessions.ErrNoIndex) {
		t.Fatalf("no file: err = %v, want ErrNoIndex", err)
	}

	db := waketest.New(t, dir)
	db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", UpdatedAt: 1})
	if list, err := x.Sessions(ctx); err != nil || len(list) != 1 {
		t.Fatalf("after the index was built: %v, %v", list, err)
	}

	db.Exec(`ALTER TABLE sessions RENAME COLUMN host TO origin`)
	if _, err := x.Sessions(ctx); err == nil {
		t.Fatal("reading went on after the structure changed")
	}
	var schema *wake.SchemaError
	if _, err := x.Sessions(ctx); !errors.As(err, &schema) {
		t.Fatalf("second read after the change: err = %v, want a SchemaError", err)
	}
	db.Exec(`ALTER TABLE sessions RENAME COLUMN origin TO host`)
	if list, err := x.Sessions(ctx); err != nil || len(list) != 1 {
		t.Errorf("after the repair: %v, %v", list, err)
	}
}

// The reader never writes: the index is opened read-only.
func TestIndexIsOpenedReadOnly(t *testing.T) {
	db := waketest.New(t, t.TempDir())
	db.Put(waketest.Session{Key: "claude:a1", Agent: "claude", UpdatedAt: 1}, waketest.Message{Role: "user", Text: "x"})
	x := wake.Open(db.Path)
	defer x.Close()
	if _, err := x.Sessions(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := x.Sessions(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := x.Messages(ctx, "claude:a1", 0, 10); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("reading changed the database file")
	}
	// And Wake can still write, with the reader's connection open.
	db.Put(waketest.Session{Key: "claude:b2", Agent: "claude", UpdatedAt: 2})
	if list, err := x.Sessions(ctx); err != nil || len(list) != 2 {
		t.Errorf("after Wake wrote: %v, %v", list, err)
	}
}

// fakeCLI is a wake-cli that records how it was called, says said and
// exits with code.
func fakeCLI(t *testing.T, said string, code int) (bin, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake wake-cli is a shell script")
	}
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "wake-cli"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\necho '" + said + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func TestCLIBuildsThenRefreshes(t *testing.T) {
	bin, calls := fakeCLI(t, "Wake is running and keeps the index current itself; nothing to do.", 0)
	db := filepath.Join(t.TempDir(), "wake.db")
	cli := wake.CLI{Bin: bin, DB: db}

	said, err := cli.Refresh(ctx)
	if err != nil || !strings.Contains(said, "nothing to do") {
		t.Fatalf("Refresh = %q, %v", said, err)
	}
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(calls)
	if want := "--db " + db + " index\n--db " + db + " refresh\n"; string(raw) != want {
		t.Errorf("wake-cli was called with\n%swant\n%s", raw, want)
	}

	bin, _ = fakeCLI(t, "wake-cli: this is not a Wake index", 2)
	_, err = wake.CLI{Bin: bin, DB: db}.Refresh(ctx)
	if err == nil || !strings.Contains(err.Error(), "this is not a Wake index") {
		t.Errorf("failing wake-cli: err = %v, want what it said", err)
	}

	if path, ok := wake.FindCLI("/opt/wake/wake-cli"); !ok || path != "/opt/wake/wake-cli" {
		t.Errorf("FindCLI with a name = %q, %v", path, ok)
	}
}

func TestDefaultPath(t *testing.T) {
	path, err := wake.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(path), "/wake/wake.db") || !filepath.IsAbs(path) {
		t.Errorf("DefaultPath = %q", path)
	}
}

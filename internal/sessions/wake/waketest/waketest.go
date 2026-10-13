// Package waketest builds a database with the tables of a Wake index, for
// tests. The table definitions are the ones of Wake v0.9.2; the content is
// whatever a test puts in. It stands in for Wake: no test reads a real index.
package waketest

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Schema is the part of Wake's schema that h8c reads, as Wake v0.9.2
// creates it.
const Schema = `
CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS sessions (
  key            TEXT PRIMARY KEY,
  agent_id       TEXT NOT NULL,
  native_id      TEXT NOT NULL,
  title          TEXT NOT NULL DEFAULT '',
  project_path   TEXT NOT NULL DEFAULT '',
  project_name   TEXT NOT NULL DEFAULT '',
  git_branch     TEXT,
  created_at     INTEGER DEFAULT 0,
  updated_at     INTEGER DEFAULT 0,
  message_count  INTEGER DEFAULT 0,
  tokens_used    INTEGER,
  model          TEXT,
  source         TEXT,
  archived       INTEGER DEFAULT 0,
  file_path      TEXT NOT NULL UNIQUE,
  file_size      INTEGER DEFAULT 0,
  file_mtime     INTEGER DEFAULT 0,
  unknown_lines  INTEGER DEFAULT 0,
  parent_key     TEXT NOT NULL DEFAULT '',
  host           TEXT NOT NULL DEFAULT '',
  spawned        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
  id           INTEGER PRIMARY KEY,
  session_key  TEXT NOT NULL,
  sidechain_id TEXT,
  seq          INTEGER NOT NULL,
  role         TEXT, ts INTEGER,
  text         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_key);
`

// DB is a fake Wake index. Like Wake it keeps a connection open and
// writes in WAL mode, so readers are tested against a live writer.
type DB struct {
	Path string
	t    testing.TB
	db   *sql.DB
}

// Session is a row of the sessions table. Host is empty for a session of
// this machine.
type Session struct {
	Key       string // "<agent>:<id>"
	Agent     string
	Title     string
	Project   string
	Path      string
	Branch    string // "" is stored as NULL
	Model     string // "" is stored as NULL
	UpdatedAt int64  // Unix milliseconds
	Parent    string
	Host      string
	Spawned   bool
	Archived  bool
}

// Message is a row of the messages table.
type Message struct {
	Role      string
	Text      string
	Sidechain string // "" is stored as NULL
	Time      int64
}

// New creates an empty index in dir.
func New(t testing.TB, dir string) *DB {
	t.Helper()
	d := &DB{Path: filepath.Join(dir, "wake.db"), t: t}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(d.Path)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(3000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	d.db = db
	t.Cleanup(func() { db.Close() })
	d.Exec(Schema)
	d.Exec(`INSERT OR REPLACE INTO schema_meta(key, value) VALUES ('fts_format', '16')`)
	return d
}

// Exec runs a statement on the index, as Wake or a newer Wake might.
func (d *DB) Exec(query string, args ...any) {
	d.t.Helper()
	if _, err := d.db.Exec(query, args...); err != nil {
		d.t.Fatal(err)
	}
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Put adds a session or replaces it, with these messages.
func (d *DB) Put(s Session, msgs ...Message) {
	d.t.Helper()
	d.Exec(`DELETE FROM messages WHERE session_key = ?`, s.Key)
	d.Exec(`INSERT OR REPLACE INTO sessions(key, agent_id, native_id, title, project_path, project_name,
		git_branch, created_at, updated_at, message_count, model, archived, file_path, file_size, file_mtime,
		parent_key, host, spawned) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 0, ?, ?, ?, ?)`,
		s.Key, s.Agent, s.Key, s.Title, s.Path, s.Project, null(s.Branch), s.UpdatedAt-1000, s.UpdatedAt,
		null(s.Model), s.Archived, "/sessions/"+s.Key+".jsonl", s.UpdatedAt, s.Parent, s.Host, s.Spawned)
	d.Append(s.Key, msgs...)
}

// Append adds messages to a session, the way a session grows while it is
// used: the count, the size and the times of the session move with it.
func (d *DB) Append(key string, msgs ...Message) {
	d.t.Helper()
	var seq, size int64
	if err := d.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM messages WHERE session_key = ?`, key).Scan(&seq); err != nil {
		d.t.Fatal(err)
	}
	for _, m := range msgs {
		seq++
		size += int64(len(m.Text))
		d.Exec(`INSERT INTO messages(session_key, sidechain_id, seq, role, ts, text) VALUES (?, ?, ?, ?, ?, ?)`,
			key, null(m.Sidechain), seq, m.Role, m.Time, m.Text)
	}
	d.Exec(`UPDATE sessions SET message_count = (SELECT COUNT(*) FROM messages WHERE session_key = ?),
		file_size = file_size + ?, file_mtime = file_mtime + ?, updated_at = updated_at + ? WHERE key = ?`,
		key, size, len(msgs), len(msgs), key)
}

// Delete removes a session and its messages.
func (d *DB) Delete(key string) {
	d.t.Helper()
	d.Exec(`DELETE FROM messages WHERE session_key = ?`, key)
	d.Exec(`DELETE FROM sessions WHERE key = ?`, key)
}

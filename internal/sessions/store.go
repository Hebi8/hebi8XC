package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the node's own database of sessions, a file in the data
// directory. It holds three things:
//
//   - the feed: the sessions of this machine, each with the revision at
//     which it last changed. Peers read the feed by revision.
//   - the copies of the feeds of other nodes.
//   - the messages of sessions of other nodes that were read here.
//
// Sessions of this machine and of others share one table, told apart by the
// node id, so a listing is one query.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;

-- rev is the revision of the node's feed at which the row last changed. For
-- this machine's rows it is assigned here; deleted rows stay, as a mark for
-- peers that have not seen the deletion. For other nodes' rows it is theirs,
-- and a deleted session is simply removed.
CREATE TABLE IF NOT EXISTS sessions (
  node         TEXT NOT NULL,
  key          TEXT NOT NULL,
  rev          INTEGER NOT NULL,
  deleted      INTEGER NOT NULL DEFAULT 0,
  print        TEXT NOT NULL DEFAULT '',
  agent        TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  project      TEXT NOT NULL DEFAULT '',
  project_path TEXT NOT NULL DEFAULT '',
  branch       TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL DEFAULT 0,
  messages     INTEGER NOT NULL DEFAULT 0,
  parent       TEXT NOT NULL DEFAULT '',
  spawned      INTEGER NOT NULL DEFAULT 0,
  archived     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node, key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS sessions_rev ON sessions(node, rev);
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions(updated_at DESC);

-- What is known about the feed of another node: which feed it was (epoch),
-- how far it has been read (cursor), and when it was last read to the end.
CREATE TABLE IF NOT EXISTS nodes (
  node      TEXT PRIMARY KEY,
  name      TEXT NOT NULL DEFAULT '',
  epoch     TEXT NOT NULL DEFAULT '',
  cursor    INTEGER NOT NULL DEFAULT 0,
  synced_at INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- The cached messages of another node's session. gen names the copy in
-- use; a new copy is written under the next gen and replaces the old one
-- only when it is complete.
CREATE TABLE IF NOT EXISTS content (
  node       TEXT NOT NULL,
  key        TEXT NOT NULL,
  gen        INTEGER NOT NULL,
  rev        INTEGER NOT NULL,
  complete   INTEGER NOT NULL,
  fetched_at INTEGER NOT NULL,
  PRIMARY KEY (node, key)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS messages (
  node      TEXT NOT NULL,
  key       TEXT NOT NULL,
  gen       INTEGER NOT NULL,
  id        INTEGER NOT NULL,
  seq       INTEGER NOT NULL,
  sidechain TEXT NOT NULL DEFAULT '',
  role      TEXT NOT NULL DEFAULT '',
  ts        INTEGER NOT NULL DEFAULT 0,
  text      TEXT NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node, key, gen, id)
) WITHOUT ROWID;
`

// OpenStore opens the database at path, creating it for this user only.
func OpenStore(path string, now func() time.Time) (*Store, error) {
	// SQLite gives its journal the permissions of the database file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", OmitHost: true, Path: filepath.ToSlash(path),
		RawQuery: "_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: every operation is short, and none can then find the
	// database locked by another.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: now}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := s.Epoch(); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Epoch names this store's feed. A node whose store was lost starts a new
// feed with a new epoch, and its peers read it from the beginning.
func (s *Store) Epoch() (string, error) {
	var epoch string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'epoch'`).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		epoch = hex.EncodeToString(raw)
		_, err = s.db.Exec(`INSERT INTO meta(k, v) VALUES ('epoch', ?)`, epoch)
	}
	return epoch, err
}

// fingerprint covers everything about a session that a peer may
// care about. The feed gives a session a new revision when it changes.
func fingerprint(sess Session) string {
	raw, _ := json.Marshal(sess)
	sum := sha256.Sum256(append(raw, sess.Stamp...))
	return hex.EncodeToString(sum[:12])
}

// SetLocal makes the feed of this machine, node self, match the sessions
// its index has now. New and changed sessions get the next revisions, and so
// do the marks left for sessions that are gone. It returns how many rows
// changed.
func (s *Store) SetLocal(ctx context.Context, self string, current []Session) (changed int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	type row struct {
		print   string
		deleted bool
	}
	known := make(map[string]row)
	rows, err := tx.QueryContext(ctx, `SELECT key, print, deleted FROM sessions WHERE node = ?`, self)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var key string
		var r row
		if err := rows.Scan(&key, &r.print, &r.deleted); err != nil {
			rows.Close()
			return 0, err
		}
		known[key] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var rev int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rev), 0) FROM sessions WHERE node = ?`, self).Scan(&rev); err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(current))
	for _, sess := range current {
		sess = clamp(sess)
		seen[sess.Key] = true
		p := fingerprint(sess)
		if old, ok := known[sess.Key]; ok && !old.deleted && old.print == p {
			continue
		}
		rev++
		changed++
		if err := upsert(ctx, tx, self, sess, rev, p); err != nil {
			return 0, err
		}
	}
	for key, old := range known {
		if seen[key] || old.deleted {
			continue
		}
		rev++
		changed++
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET rev = ?, deleted = 1, print = '', title = '', project = '', project_path = '',
			       branch = '', model = '', parent = '', messages = 0
			WHERE node = ? AND key = ?`, rev, self, key); err != nil {
			return 0, err
		}
	}
	return changed, tx.Commit()
}

func upsert(ctx context.Context, tx *sql.Tx, node string, sess Session, rev int64, p string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions(node, key, rev, deleted, print, agent, title, project, project_path, branch,
		                     model, created_at, updated_at, messages, parent, spawned, archived)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node, key) DO UPDATE SET
		  rev = excluded.rev, deleted = 0, print = excluded.print, agent = excluded.agent,
		  title = excluded.title, project = excluded.project, project_path = excluded.project_path,
		  branch = excluded.branch, model = excluded.model, created_at = excluded.created_at,
		  updated_at = excluded.updated_at, messages = excluded.messages, parent = excluded.parent,
		  spawned = excluded.spawned, archived = excluded.archived`,
		node, sess.Key, rev, p, sess.Agent, sess.Title, sess.Project, sess.ProjectPath, sess.Branch,
		sess.Model, sess.CreatedAt, sess.UpdatedAt, sess.Messages, sess.Parent, sess.Spawned, sess.Archived)
	return err
}

// clamp cuts the descriptive fields of a session to maxField bytes.
func clamp(sess Session) Session {
	for _, f := range []*string{&sess.Key, &sess.Agent, &sess.Title, &sess.Project, &sess.ProjectPath,
		&sess.Branch, &sess.Model, &sess.Parent} {
		if len(*f) > maxField {
			*f = strings.ToValidUTF8((*f)[:maxField], "")
		}
	}
	return sess
}

// Change is one entry of a feed: a session as it is at revision Rev, or the
// news that the session with this key is gone.
type Change struct {
	Rev     int64 `json:"rev"`
	Deleted bool  `json:"deleted,omitempty"`
	Session
}

const sessionColumns = `key, agent, title, project, project_path, branch, model,
	created_at, updated_at, messages, parent, spawned, archived`

func scanSession(scan func(...any) error, more ...any) (Session, error) {
	var sess Session
	dest := append([]any{&sess.Key, &sess.Agent, &sess.Title, &sess.Project, &sess.ProjectPath, &sess.Branch,
		&sess.Model, &sess.CreatedAt, &sess.UpdatedAt, &sess.Messages, &sess.Parent, &sess.Spawned,
		&sess.Archived}, more...)
	return sess, scan(dest...)
}

// Changes returns the entries of the feed of node above revision after, in
// order of revision, at most limit.
func (s *Store) Changes(ctx context.Context, node string, after int64, limit int) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+`, rev, deleted FROM sessions
		WHERE node = ? AND rev > ? ORDER BY rev LIMIT ?`, node, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		if c.Session, err = scanSession(rows.Scan, &c.Rev, &c.Deleted); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Lookup returns a session of node and its revision. ok is false if the
// node's feed, as far as it is known here, has no such session.
func (s *Store) Lookup(ctx context.Context, node, key string) (sess Session, rev int64, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+`, rev FROM sessions
		WHERE node = ? AND key = ? AND deleted = 0`, node, key)
	sess, err = scanSession(row.Scan, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, 0, false, nil
	}
	return sess, rev, err == nil, err
}

// Feed is what is known about the feed of another node.
type Feed struct {
	Name     string
	Epoch    string
	Cursor   int64
	SyncedAt time.Time // zero if it was never read to the end
}

func (s *Store) Feed(ctx context.Context, node string) (Feed, error) {
	var f Feed
	var synced int64
	err := s.db.QueryRowContext(ctx, `SELECT name, epoch, cursor, synced_at FROM nodes WHERE node = ?`, node).
		Scan(&f.Name, &f.Epoch, &f.Cursor, &synced)
	if errors.Is(err, sql.ErrNoRows) {
		return Feed{}, nil
	}
	if synced > 0 {
		f.SyncedAt = time.UnixMilli(synced)
	}
	return f, err
}

// ErrFull means a node's feed holds more sessions than this node will keep.
var ErrFull = errors.New("the node lists more sessions than are kept")

// ApplyRemote records a page of the feed of another node: the sessions
// that changed, the ones that are gone, and how far the feed is now read.
// A page from a different epoch than the one on record means the node
// started a new feed; everything known about the old one is dropped first.
// done says the page was the last one, so the copy is complete as of now.
func (s *Store) ApplyRemote(ctx context.Context, node, name, epoch string, page []Change, cursor int64, done bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var have string
	err = tx.QueryRowContext(ctx, `SELECT epoch FROM nodes WHERE node = ?`, node).Scan(&have)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if have != epoch {
		for _, table := range []string{"sessions", "content", "messages"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node = ?`, node); err != nil {
				return err
			}
		}
	}
	for _, c := range page {
		c.Session = clamp(c.Session)
		if c.Deleted {
			for _, table := range []string{"sessions", "content", "messages"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node = ? AND key = ?`, node, c.Key); err != nil {
					return err
				}
			}
			continue
		}
		if err := upsert(ctx, tx, node, c.Session, c.Rev, ""); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE node = ?`, node).Scan(&count); err != nil {
		return err
	}
	if count > maxSessionsPerNode {
		return ErrFull
	}

	synced := int64(0)
	if done {
		synced = s.now().UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nodes(node, name, epoch, cursor, synced_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(node) DO UPDATE SET name = excluded.name, epoch = excluded.epoch, cursor = excluded.cursor,
		  synced_at = CASE WHEN excluded.synced_at > 0 THEN excluded.synced_at ELSE nodes.synced_at END`,
		node, name, epoch, cursor, synced); err != nil {
		return err
	}
	return tx.Commit()
}

// Query selects sessions for a listing. Empty fields do not filter.
type Query struct {
	Node    string   // node id
	Agent   string   // exact
	Project string   // part of the project name or path
	Since   int64    // updated at or after, Unix milliseconds
	Text    []string // each must be part of the title or the project name
	Limit   int
	// After continues a listing: it is the Next of the page before.
	After string
}

// position is where a listing stands after a session, by the three values
// it is ordered by. It travels as the After of the next page.
type position struct {
	UpdatedAt int64  `json:"u"`
	Node      string `json:"n"`
	Key       string `json:"k"`
}

func (p position) String() string {
	raw, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func parsePosition(text string) (position, error) {
	var p position
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	if err != nil {
		return position{}, errors.New("after is not the next of an earlier page")
	}
	return p, nil
}

// Listed is a session together with the node it is on.
type Listed struct {
	Node string
	Session
}

// like turns text into a LIKE pattern that matches it anywhere, literally.
func like(text string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(text) + "%"
}

// List returns the sessions of all nodes that match q, most recently
// updated first.
func (s *Store) List(ctx context.Context, q Query) ([]Listed, error) {
	where := []string{"deleted = 0"}
	var args []any
	if q.Node != "" {
		where = append(where, "node = ?")
		args = append(args, q.Node)
	}
	if q.Agent != "" {
		where = append(where, "agent = ?")
		args = append(args, q.Agent)
	}
	if q.Project != "" {
		where = append(where, `(project LIKE ? ESCAPE '\' OR project_path LIKE ? ESCAPE '\')`)
		args = append(args, like(q.Project), like(q.Project))
	}
	if q.Since > 0 {
		where = append(where, "updated_at >= ?")
		args = append(args, q.Since)
	}
	for _, text := range q.Text {
		where = append(where, `(title LIKE ? ESCAPE '\' OR project LIKE ? ESCAPE '\')`)
		args = append(args, like(text), like(text))
	}
	if q.After != "" {
		p, err := parsePosition(q.After)
		if err != nil {
			return nil, err
		}
		// What comes after p in the order below.
		where = append(where, `(updated_at < ? OR (updated_at = ? AND (node > ? OR (node = ? AND key > ?))))`)
		args = append(args, p.UpdatedAt, p.UpdatedAt, p.Node, p.Node, p.Key)
	}
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+`, node FROM sessions
		WHERE `+strings.Join(where, " AND ")+` ORDER BY updated_at DESC, node, key LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Listed
	for rows.Next() {
		var l Listed
		if l.Session, err = scanSession(rows.Scan, &l.Node); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Counts returns how many sessions each node has.
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node, COUNT(*) FROM sessions WHERE deleted = 0 GROUP BY node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return nil, err
		}
		out[node] = n
	}
	return out, rows.Err()
}

// Feeds returns what is known about the feed of every other node.
func (s *Store) Feeds(ctx context.Context) (map[string]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node, name, epoch, cursor, synced_at FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Feed)
	for rows.Next() {
		var node string
		var f Feed
		var synced int64
		if err := rows.Scan(&node, &f.Name, &f.Epoch, &f.Cursor, &synced); err != nil {
			return nil, err
		}
		if synced > 0 {
			f.SyncedAt = time.UnixMilli(synced)
		}
		out[node] = f
	}
	return out, rows.Err()
}

// Cached describes the copy of another node's session that is kept here.
type Cached struct {
	Rev       int64 // revision of the session the copy was made from
	Complete  bool  // false if the copy stops before the end of the session
	FetchedAt time.Time
	gen       int64
}

// Cached returns the copy of a session's messages. ok is false if there is none.
func (s *Store) Cached(ctx context.Context, node, key string) (c Cached, ok bool, err error) {
	var fetched int64
	err = s.db.QueryRowContext(ctx, `SELECT gen, rev, complete, fetched_at FROM content WHERE node = ? AND key = ?`,
		node, key).Scan(&c.gen, &c.Rev, &c.Complete, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return Cached{}, false, nil
	}
	c.FetchedAt = time.UnixMilli(fetched)
	return c, err == nil, err
}

// NewCopy starts a new copy of a session's messages beside the one in use,
// if any, and returns its number. Whatever is left of copies that were
// started and never put in use is removed first. Only one copy of a session
// may be under way at a time; the caller sees to that.
func (s *Store) NewCopy(ctx context.Context, node, key string) (gen int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var inUse int64
	err = tx.QueryRowContext(ctx, `SELECT gen FROM content WHERE node = ? AND key = ?`, node, key).Scan(&inUse)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE node = ? AND key = ? AND gen <> ?`, node, key, inUse); err != nil {
		return 0, err
	}
	return inUse + 1, tx.Commit()
}

// AddToCopy appends messages to a copy.
func (s *Store) AddToCopy(ctx context.Context, node, key string, gen int64, msgs []Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO messages(node, key, gen, id, seq, sidechain, role, ts, text, truncated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			node, key, gen, m.ID, m.Seq, m.Sidechain, m.Role, m.Time, m.Text, m.Truncated); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ErrFeedChanged means the feed a copy was being made from is no longer the
// one on record for its node, or no longer lists the session.
var ErrFeedChanged = errors.New("the node's session feed changed meanwhile")

// UseCopy makes a copy the one in use and removes the others. The copy was
// made while the node's feed had the given epoch; if the feed on record is
// another one by now, or the session is gone from it, the copy describes
// something that no longer exists and ErrFeedChanged is returned instead.
func (s *Store) UseCopy(ctx context.Context, node, key, epoch string, gen, rev int64, complete bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var listed int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions s JOIN nodes n ON n.node = s.node
		WHERE s.node = ? AND s.key = ? AND s.deleted = 0 AND n.epoch = ?`, node, key, epoch).Scan(&listed)
	if err != nil {
		return err
	}
	if listed == 0 {
		return ErrFeedChanged
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO content(node, key, gen, rev, complete, fetched_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(node, key) DO UPDATE SET gen = excluded.gen, rev = excluded.rev,
		  complete = excluded.complete, fetched_at = excluded.fetched_at`,
		node, key, gen, rev, complete, s.now().UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE node = ? AND key = ? AND gen <> ?`, node, key, gen); err != nil {
		return err
	}
	return tx.Commit()
}

// DropCopy removes a copy that will not be used.
func (s *Store) DropCopy(ctx context.Context, node, key string, gen int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE node = ? AND key = ? AND gen = ?`, node, key, gen)
	return err
}

// CachedMessages returns messages of the copy in use with an ID above
// after, in order: at most limit, and no more than fit into budget bytes,
// but always one. more says whether the copy goes on.
func (s *Store) CachedMessages(ctx context.Context, node, key string, c Cached, after int64, limit, budget int) ([]Message, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, seq, sidechain, role, ts, text, truncated FROM messages
		WHERE node = ? AND key = ? AND gen = ? AND id > ? ORDER BY id LIMIT ?`, node, key, c.gen, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Message
	size := 0
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Seq, &m.Sidechain, &m.Role, &m.Time, &m.Text, &m.Truncated); err != nil {
			return nil, false, err
		}
		size += MessageSize(m)
		if len(out) == limit || (size > budget && len(out) > 0) {
			return out, true, nil
		}
		out = append(out, m)
	}
	return out, false, rows.Err()
}

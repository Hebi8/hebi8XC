// Package wake reads the session index that Wake keeps
// (https://github.com/iAmCorey/Wake). Wake parses the session files of some
// twenty agents into one SQLite database; this package opens that database
// read-only and takes from it what package sessions needs.
//
// The database is Wake's internal format, not an interface. Everything that
// depends on it is in this package, written against Wake v0.9.2
// (crates/wake-core/src/db.rs):
//
//   - sessions holds one row per session. created_at and updated_at are Unix
//     milliseconds. host is empty for sessions of this machine and the name
//     of an SSH host for sessions Wake mirrored from elsewhere; those are
//     left out, they belong to another machine. parent_key is the key of the
//     session a child session hangs under, or empty. spawned is 1 for
//     sessions an agent started itself.
//   - messages holds the text of each message, in the order of id within a
//     session. Wake keeps the rows of an unchanged beginning and replaces
//     the rest, so ids only grow. sidechain_id is NULL on the main thread.
//
// Wake's own side readers (wake-cli, wake-mcp) open the database the same
// way: read-only, with a busy timeout, never touching the journal mode. A
// reader does not block Wake's writes.
package wake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Hebi8/hebi8XC/internal/sessions"
)

// required lists the tables and columns this package reads.
var required = map[string][]string{
	"sessions": {"key", "agent_id", "title", "project_path", "project_name", "git_branch",
		"created_at", "updated_at", "message_count", "model", "archived",
		"file_size", "file_mtime", "parent_key", "host", "spawned"},
	"messages": {"id", "session_key", "sidechain_id", "seq", "role", "ts", "text"},
}

// SchemaError means the database is there but is not a Wake index this
// package can read.
type SchemaError struct {
	Path    string
	Missing string // "table" or "table.column"
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("cannot make out the structure of the Wake index at %s: no %s (this reader was written for Wake v0.9.2)",
		e.Path, e.Missing)
}

// DefaultPath is where Wake keeps its index on this system.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var data string
	switch runtime.GOOS {
	case "windows":
		data = os.Getenv("LOCALAPPDATA")
		if data == "" {
			data = filepath.Join(home, "AppData", "Local")
		}
	case "darwin":
		data = filepath.Join(home, "Library", "Application Support")
	default:
		data = os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(data, "wake", "wake.db"), nil
}

// Index is a sessions.Source over a Wake database. The database does not
// have to exist yet: it is opened, and its structure checked, when it is
// first needed and again after any failure.
type Index struct {
	path string

	mu sync.Mutex
	db *sql.DB
}

func Open(path string) *Index {
	return &Index{path: path}
}

func (x *Index) Path() string { return x.path }

// Close releases the database.
func (x *Index) Close() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.db != nil {
		x.db.Close()
		x.db = nil
	}
}

// open returns the checked database, opening it first if necessary.
func (x *Index) open(ctx context.Context) (*sql.DB, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.db != nil {
		return x.db, nil
	}
	if _, err := os.Stat(x.path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no Wake index at %s", sessions.ErrNoIndex, x.path)
	} else if err != nil {
		return nil, err
	}
	// mode=ro never creates or changes the database. In WAL mode a reader
	// still maps the -shm file, which needs the directory to be writable
	// by this user; that is where Wake keeps it.
	dsn := (&url.URL{Scheme: "file", OmitHost: true, Path: filepath.ToSlash(x.path),
		RawQuery: "mode=ro&_pragma=busy_timeout(3000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// An idle connection holds no lock, but there is no reason to keep
	// several, or to keep one for long.
	db.SetMaxOpenConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	if err := check(ctx, db, x.path); err != nil {
		db.Close()
		return nil, err
	}
	x.db = db
	return db, nil
}

// fail forgets the open database after an error, so the next call checks
// the structure again: Wake may have been updated and have migrated it.
func (x *Index) fail(db *sql.DB) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.db == db {
		x.db.Close()
		x.db = nil
	}
}

func check(ctx context.Context, db *sql.DB, path string) error {
	for table, columns := range required {
		rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			return fmt.Errorf("reading the Wake index at %s: %w", path, err)
		}
		have := make(map[string]bool)
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			have[name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reading the Wake index at %s: %w", path, err)
		}
		if len(have) == 0 {
			return &SchemaError{Path: path, Missing: "table " + table}
		}
		for _, c := range columns {
			if !have[c] {
				return &SchemaError{Path: path, Missing: "column " + table + "." + c}
			}
		}
	}
	return nil
}

func (x *Index) Sessions(ctx context.Context) ([]sessions.Session, error) {
	db, err := x.open(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT key, agent_id, title, project_name, project_path, COALESCE(git_branch, ''),
		       COALESCE(model, ''), COALESCE(created_at, 0), COALESCE(updated_at, 0),
		       COALESCE(message_count, 0), parent_key, spawned, COALESCE(archived, 0),
		       COALESCE(file_mtime, 0), COALESCE(file_size, 0)
		FROM sessions WHERE host = ''`)
	if err != nil {
		x.fail(db)
		return nil, err
	}
	defer rows.Close()
	var out []sessions.Session
	for rows.Next() {
		var s sessions.Session
		var mtime, size int64
		if err := rows.Scan(&s.Key, &s.Agent, &s.Title, &s.Project, &s.ProjectPath, &s.Branch,
			&s.Model, &s.CreatedAt, &s.UpdatedAt, &s.Messages, &s.Parent, &s.Spawned, &s.Archived,
			&mtime, &size); err != nil {
			x.fail(db)
			return nil, err
		}
		s.Stamp = fmt.Sprintf("%d:%d", mtime, size)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		x.fail(db)
		return nil, err
	}
	return out, nil
}

func (x *Index) Messages(ctx context.Context, key string, after int64, limit, budget int) ([]sessions.Message, bool, error) {
	db, err := x.open(ctx)
	if err != nil {
		return nil, false, err
	}
	// The join keeps this to sessions of this machine, whatever key is asked for.
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.seq, COALESCE(m.sidechain_id, ''), COALESCE(m.role, ''), COALESCE(m.ts, 0),
		       substr(m.text, 1, ?), length(m.text) > ?
		FROM messages m JOIN sessions s ON s.key = m.session_key
		WHERE m.session_key = ? AND s.host = '' AND m.id > ?
		ORDER BY m.id LIMIT ?`, sessions.MaxText, sessions.MaxText, key, after, limit+1)
	if err != nil {
		x.fail(db)
		return nil, false, err
	}
	// Rows are read one at a time and reading stops at the budget: a page of
	// a long session must not pull hundreds of messages into memory first.
	defer rows.Close()
	var out []sessions.Message
	size := 0
	for rows.Next() {
		var m sessions.Message
		if err := rows.Scan(&m.ID, &m.Seq, &m.Sidechain, &m.Role, &m.Time, &m.Text, &m.Truncated); err != nil {
			x.fail(db)
			return nil, false, err
		}
		size += sessions.MessageSize(m)
		if len(out) == limit || (size > budget && len(out) > 0) {
			return out, true, nil
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		x.fail(db)
		return nil, false, err
	}
	return out, false, nil
}

// CLI runs wake-cli to keep the index current while Wake itself is closed.
type CLI struct {
	Bin string // path of wake-cli
	DB  string // the index it works on
}

// FindCLI returns the path of wake-cli: the one named, or the one on PATH.
// ok is false if there is none.
func FindCLI(named string) (path string, ok bool) {
	if named != "" {
		return named, true
	}
	path, err := exec.LookPath("wake-cli")
	return path, err == nil
}

// Refresh brings the index up to date: "wake-cli index" if there is none
// yet, "wake-cli refresh" otherwise. Both leave the index alone and still
// succeed while Wake is running, because then Wake keeps it current. The
// returned text is what wake-cli said.
func (c CLI) Refresh(ctx context.Context) (string, error) {
	command := "refresh"
	if _, err := os.Stat(c.DB); errors.Is(err, fs.ErrNotExist) {
		command = "index"
	}
	cmd := exec.CommandContext(ctx, c.Bin, "--db", c.DB, command)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	said := strings.TrimSpace(string(out))
	if err != nil {
		return said, fmt.Errorf("wake-cli %s: %w: %s", command, err, said)
	}
	return said, nil
}

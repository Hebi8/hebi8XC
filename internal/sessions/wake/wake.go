// Package wake reads the session index that Wake keeps
// (https://github.com/iAmCorey/Wake). Wake parses the session files of some
// twenty agents into one SQLite database; this package opens that database
// read-only and takes from it what package sessions needs.
//
// The database is Wake's internal format, not an interface. Everything that
// depends on it is in this package, written against Wake v0.9.2 and v0.10.2
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
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
		"file_size", "file_mtime", "parent_key", "host", "spawned", "source", "tokens_used"},
	"messages": {"id", "session_key", "sidechain_id", "seq", "role", "ts", "text"},
}

// SchemaError means the database is there but is not a Wake index this
// package can read.
type SchemaError struct {
	Path    string
	Missing string // "table" or "table.column"
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("cannot make out the structure of the Wake index at %s: no %s (this reader supports Wake v0.9.2 and v0.10.2)",
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

	mu   sync.Mutex
	db   *sql.DB
	Log  *slog.Logger
	said map[string]bool
}

func Open(path string) *Index {
	return &Index{path: path, Log: slog.Default(), said: make(map[string]bool)}
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

func (x *Index) Sessions(ctx context.Context) (result []sessions.Session, err error) {
	db, err := x.open(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() {
		tx.Rollback()
		if err != nil {
			x.fail(db)
		}
	}()
	structured, err := x.structure(ctx, tx)
	if err != nil {
		return nil, err
	}
	extra, joins := "'', '', 0, 0, 0", ""
	if structured != "" {
		extra = "coalesce(st.file_mtime,'') || ':' || coalesce(st.file_size,'') || ':' || coalesce(st.fts_format,'') || ':' || coalesce(st.rows,'') || ':' || coalesce(st.format,''), coalesce((SELECT value FROM schema_meta WHERE key='fts_format'),''), " + validStructure + ", coalesce(tc.calls,0), coalesce(tc.errors,0)"
		joins = structureJoin + " LEFT JOIN (SELECT session_key,count(*) calls,sum(is_error) errors FROM tool_calls GROUP BY session_key) tc ON tc.session_key=s.key"
	}
	usageColumns := "NULL,NULL,NULL,NULL"
	if structured == "2" {
		usageColumns = "u.input,u.output,u.cache_read,u.cache_write"
		joins += ` LEFT JOIN (SELECT e.session_key,sum(input_tokens) input,sum(output_tokens) output,
   sum(cache_read_tokens) cache_read,sum(cache_write_tokens) cache_write
   FROM message_extras e JOIN messages m ON m.session_key=e.session_key AND m.seq=e.seq
   GROUP BY e.session_key) u ON u.session_key=s.key`
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.key,agent_id,title,project_name,project_path,coalesce(git_branch,''),
 coalesce(model,''),coalesce(created_at,0),coalesce(updated_at,0),coalesce(message_count,0),parent_key,spawned,coalesce(archived,0),
 coalesce(s.file_mtime,0),coalesce(s.file_size,0),coalesce(source,''),s.tokens_used,`+usageColumns+`,`+extra+` FROM sessions s `+joins+` WHERE host=''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessions.Session
	for rows.Next() {
		var s sessions.Session
		var mtime, size, calls, errs int64
		var stamp, fts string
		var valid bool
		var total, input, output, cacheRead, cacheWrite sql.NullInt64
		if err := rows.Scan(&s.Key, &s.Agent, &s.Title, &s.Project, &s.ProjectPath, &s.Branch, &s.Model, &s.CreatedAt, &s.UpdatedAt, &s.Messages, &s.Parent, &s.Spawned, &s.Archived, &mtime, &size, &s.App, &total, &input, &output, &cacheRead, &cacheWrite, &stamp, &fts, &valid, &calls, &errs); err != nil {
			return nil, err
		}
		s.Reader = "wake"
		s.Stamp = fmt.Sprintf("%d:%d:%s:%s:%t:%s", mtime, size, stamp, fts, valid, structured)
		if valid {
			s.ToolCalls = &calls
			s.ToolErrors = &errs
			if structured == "2" {
				s.AddUsage(usage(input, output, cacheRead, cacheWrite))
				if err := sessionFiles(ctx, tx, &s); err != nil {
					return nil, err
				}
			}
		} else if structured != "" {
			x.note(s.Key, "Wake session structure is stale; showing text only")
		}
		if s.Tokens == nil && total.Valid {
			s.Tokens = &sessions.Tokens{Total: &total.Int64}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (x *Index) Messages(ctx context.Context, key string, after int64, limit, budget int) (result []sessions.Message, more bool, err error) {
	db, err := x.open(ctx)
	if err != nil {
		return nil, false, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer func() {
		tx.Rollback()
		if err != nil {
			x.fail(db)
		}
	}()
	structured, err := x.structure(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	var valid bool
	if structured != "" {
		err = tx.QueryRowContext(ctx, "SELECT "+validStructure+" FROM sessions s "+structureJoin+" WHERE s.key=? AND s.host=''", key).Scan(&valid)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		if err == nil && !valid {
			x.note(key, "Wake session structure is stale; showing text only")
		}
	}
	text, extra, join := "m.text", "0, '', '', ''", ""
	if valid {
		text = "coalesce(e.full_text,m.text)"
		extra = "0, coalesce(e.kind,'text'),coalesce(e.thinking,''),coalesce(e.model,'')"
		join = " LEFT JOIN message_extras e ON e.session_key=m.session_key AND e.seq=m.seq"
	}
	extra += ",NULL,NULL,NULL,NULL"
	if valid && structured == "2" {
		extra = "0,coalesce(e.kind,'text'),coalesce(e.thinking,''),coalesce(e.model,''),e.input_tokens,e.output_tokens,e.cache_read_tokens,e.cache_write_tokens"
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.seq,coalesce(m.sidechain_id,''),coalesce(m.role,''),coalesce(m.ts,0),`+text+","+extra+`
 FROM messages m JOIN sessions s ON s.key=m.session_key `+join+` WHERE m.session_key=? AND s.host='' AND m.id>? ORDER BY m.id LIMIT ?`, key, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []sessions.Message
	size := 0
	for rows.Next() {
		var m sessions.Message
		var input, output, cacheRead, cacheWrite sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Seq, &m.Sidechain, &m.Role, &m.Time, &m.Text, &m.Truncated, &m.Kind, &m.Thinking, &m.Model, &input, &output, &cacheRead, &cacheWrite); err != nil {
			return nil, false, err
		}
		m.Usage = usage(input, output, cacheRead, cacheWrite)
		if len(out) == limit {
			return out, true, nil
		}
		if valid {
			calls, err := tx.QueryContext(ctx, `SELECT idx,call_id,name,coalesce(input,''),coalesce(output,''),is_error,coalesce(child_id,'') FROM tool_calls WHERE session_key=? AND seq=? ORDER BY idx`, key, m.Seq)
			if err != nil {
				return nil, false, err
			}
			for calls.Next() {
				var c sessions.Call
				var idx int64
				if err := calls.Scan(&idx, &c.Call, &c.Tool, &c.Input, &c.Output, &c.Error, &c.Agent); err != nil {
					calls.Close()
					return nil, false, err
				}
				if structured == "2" {
					c.Files, err = callFiles(ctx, tx, key, m.Seq, idx)
					if err != nil {
						calls.Close()
						return nil, false, err
					}
				}
				if c.Agent != "" {
					agent, _, _ := strings.Cut(key, ":")
					c.Agent = agent + ":" + c.Agent
				}
				m.Calls = append(m.Calls, c)
			}
			err = calls.Err()
			calls.Close()
			if err != nil {
				return nil, false, err
			}
		}
		size += sessions.MessageSize(m)
		if size > budget && len(out) > 0 {
			return out, true, nil
		}
		out = append(out, m)
	}
	return out, false, rows.Err()
}

// CLI runs wake-cli to keep the index current while Wake itself is closed.
type CLI struct {
	Bin string // path of wake-cli
	DB  string // the index it works on

	// How long "wake-cli index" and "wake-cli refresh" may take; the zero
	// values mean ten minutes and two. Building an index of some thousand
	// sessions took well under a minute.
	IndexTimeout, RefreshTimeout time.Duration
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
// returned text is what wake-cli said. A wake-cli that does not finish in
// time is stopped, and that is an error like any other.
func (c CLI) Refresh(ctx context.Context) (string, error) {
	command, limit := "refresh", cmp.Or(c.RefreshTimeout, 2*time.Minute)
	if _, err := os.Stat(c.DB); errors.Is(err, fs.ErrNotExist) {
		command, limit = "index", cmp.Or(c.IndexTimeout, 10*time.Minute)
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, "--db", c.DB, command)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	said := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return said, fmt.Errorf("wake-cli %s did not finish within %v and was stopped", command, limit)
	}
	if err != nil {
		return said, fmt.Errorf("wake-cli %s: %w: %s", command, err, said)
	}
	return said, nil
}

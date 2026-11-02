// Package antigravity reads Antigravity ACP conversation databases, read-only.
// Field numbers were checked against the descriptors and history replay in
// agy_acp_server.par v1.0.0 (92521fc3cbd964bd): gemini_coder.Step and
// exa.cortex_pb.CortexStep{UserInput,PlannerResponse,Metadata}. No upstream
// implementation or generated descriptors are copied into this package.
package antigravity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/Hebi8/hebi8XC/internal/sessions"
	_ "modernc.org/sqlite"
)

const Agent = "antigravity-acp"

type Index struct {
	dir   string
	mu    sync.Mutex
	cache map[string]sessions.Session
}

func Open(dir string) *Index { return &Index{dir: dir} }

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity-acp", "conversations"), nil
}

func open(ctx context.Context, path string) (*sql.DB, *sql.Tx, error) {
	dsn := (&url.URL{Scheme: "file", OmitHost: true, Path: filepath.ToSlash(path), RawQuery: "mode=ro&_pragma=busy_timeout(3000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	// A single read transaction keeps all blob slices in the same snapshot.
	// Do not use immutable=1: committed rows can still be in a live WAL.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, tx, nil
}

// blob reads slices rather than materializing a whole step. The small cache
// covers nearby protobuf headers; a large skipped field costs no allocation.
// table and column are constants at call sites, never session keys.
func blob(ctx context.Context, tx *sql.Tx, table, column string, id any, size int64) proto {
	var cached []byte
	var start int64 = -1
	query := "SELECT substr(" + column + ", ?, ?) FROM " + table + " WHERE "
	if table == "trajectory_metadata_blob" {
		query += "id = ?"
	} else {
		query += "idx = ?"
	}
	return proto{size: size, read: func(offset, n int64) ([]byte, error) {
		if offset >= start && start >= 0 && offset+n <= start+int64(len(cached)) {
			return cached[offset-start : offset-start+n], nil
		}
		length := min(size-offset, max(n, 4096))
		var b []byte
		if err := tx.QueryRowContext(ctx, query, offset+1, length, id).Scan(&b); err != nil {
			return nil, err
		}
		if int64(len(b)) != length {
			return nil, errProto
		}
		cached, start = b, offset
		return b[:n], nil
	}}
}

// messageText follows ACP's historical replay: user_response (19.2), or
// modified_response (20.8) in preference to response (20.1). Thinking, tool
// calls and attachments are intentionally not presented as user/agent text.
func messageText(payload proto, typ int) (proto, string, error) {
	number, role := uint64(19), "user"
	if typ == 15 {
		number, role = 20, "assistant"
	}
	body, err := payload.child(number)
	if err != nil {
		return proto{}, role, err
	}
	if typ == 14 {
		text, err := body.child(2)
		return text, role, err
	}
	text, err := body.child(8)
	if err == nil && text.size == 0 {
		text, err = body.child(1)
	}
	return text, role, err
}

func (x *Index) Sessions(ctx context.Context) ([]sessions.Session, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	files, err := os.ReadDir(x.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: no Antigravity ACP directory", sessions.ErrNoIndex)
	}
	if err != nil {
		return nil, err
	}
	var out []sessions.Session
	next := make(map[string]sessions.Session)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".db") {
			continue
		}
		name := file.Name()
		s, cached := x.cache[name]
		stamp, err := fileStamp(filepath.Join(x.dir, name))
		if err == nil && (!cached || s.Stamp != stamp) {
			var fresh sessions.Session
			fresh, err = x.session(ctx, name)
			if err == nil {
				fresh.Stamp = stamp
				s = fresh
			}
		}
		// Retain the last successful read until the file leaves the directory.
		// Keep its old stamp so a failed read is retried on the next round.
		if err == nil || cached {
			next[name] = s
			out = append(out, s)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	x.cache = next
	return out, nil
}

func fileStamp(path string) (string, error) {
	var stamp string
	// Include WAL changes: the main db's stat alone misses active sessions.
	for i, file := range []string{path, path + "-wal", transcriptPath(path)} {
		st, err := os.Stat(file)
		if i > 0 && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		stamp += fmt.Sprintf("%d:%d;", st.Size(), st.ModTime().UnixNano())
	}
	return stamp, nil
}

// Read in bounded chunks, collapsing whitespace before counting characters.
func title(text proto) (string, error) {
	var out []rune
	space := false
	for text.size > 0 {
		part, _, err := text.text(256)
		if err != nil {
			return "", err
		}
		for _, r := range part {
			if unicode.IsSpace(r) {
				space = len(out) > 0
				continue
			}
			if space {
				out = append(out, ' ')
				space = false
			}
			out = append(out, r)
			if len(out) >= 80 {
				return string(out[:80]), nil
			}
		}
		text.offset += int64(len(part))
		text.size -= int64(len(part))
	}
	return string(out), nil
}

func (x *Index) session(ctx context.Context, name string) (sessions.Session, error) {
	path := filepath.Join(x.dir, name)
	s := sessions.Session{Key: Agent + ":" + strings.TrimSuffix(name, ".db"), Agent: Agent, Reader: "acp"}
	db, tx, err := open(ctx, path)
	if err != nil {
		return s, err
	}
	defer db.Close()
	defer tx.Rollback()
	structure, hasTranscript := transcript(path)
	var size int64
	err = tx.QueryRowContext(ctx, "SELECT length(data) FROM trajectory_metadata_blob WHERE id = 'main'").Scan(&size)
	if err == nil {
		s.CreatedAt, err = blob(ctx, tx, "trajectory_metadata_blob", "data", "main", size).timestamp(2)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT idx, step_type, coalesce(length(metadata),0), coalesce(length(step_payload),0) FROM steps ORDER BY idx")
	if err != nil {
		return s, err
	}
	defer rows.Close()
	var calls, errs int64
	titled := false
	for rows.Next() {
		var idx, ms, ps int64
		var typ int
		if err := rows.Scan(&idx, &typ, &ms, &ps); err != nil {
			return s, err
		}
		metadata := blob(ctx, tx, "steps", "metadata", idx, ms)
		created, err := metadata.timestamp(1)
		if err != nil {
			return s, err
		}
		completed, err := metadata.timestamp(8)
		if err != nil {
			return s, err
		}
		if created > 0 && (s.CreatedAt == 0 || created < s.CreatedAt) {
			s.CreatedAt = created
		}
		s.UpdatedAt = max(s.UpdatedAt, created, completed)
		model, err := metadata.child(24) // ModelInfo.model_name = 8; enum ids are not names.
		if err != nil {
			return s, err
		}
		model, err = model.child(8)
		if err != nil {
			return s, err
		}
		if model.size > 0 {
			s.Model, _, err = model.text(4096)
			if err != nil {
				return s, err
			}
		}
		if typ == 14 || typ == 15 {
			for _, c := range structure[idx].Calls {
				calls++
				if c.Error {
					errs++
				}
			}
			text, _, err := messageText(blob(ctx, tx, "steps", "step_payload", idx, ps), typ)
			if err != nil {
				return s, err
			}
			if text.size > 0 || len(structure[idx].Calls) > 0 {
				s.Messages++
				if typ == 14 && !titled {
					s.Title, err = title(text)
					if err != nil {
						return s, err
					}
					titled = true
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if hasTranscript {
		s.ToolCalls = &calls
		s.ToolErrors = &errs
	}
	s.UpdatedAt = max(s.UpdatedAt, s.CreatedAt)
	var meta struct {
		CWD string `json:"cwd"`
	}
	if b, err := os.ReadFile(strings.TrimSuffix(path, ".db") + ".meta"); err == nil && json.Unmarshal(b, &meta) == nil {
		s.ProjectPath = meta.CWD
		if meta.CWD != "" {
			s.Project = filepath.Base(meta.CWD)
		}
	}
	return s, nil
}

func (x *Index) Messages(ctx context.Context, key string, after int64, limit, budget int) ([]sessions.Message, bool, error) {
	id, ok := strings.CutPrefix(key, Agent+":")
	// Keys come from peers as well as our feed: never allow a path as an id.
	if !ok || id == "" || strings.ContainsAny(id, "/\\:") || id == "." || id == ".." {
		return nil, false, sessions.ErrNotFound
	}
	path := filepath.Join(x.dir, id+".db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, sessions.ErrNotFound
	}
	db, tx, err := open(ctx, path)
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	defer tx.Rollback()
	structure, hasTranscript := transcript(path)
	limit = max(1, min(limit, sessions.MaxPage))
	budget = min(budget, sessions.PageBudget)
	rows, err := tx.QueryContext(ctx, `SELECT idx,step_type,coalesce(length(metadata),0),coalesce(length(step_payload),0)
 FROM steps WHERE idx >= ? AND step_type IN (14,15) ORDER BY idx`, max(after, 0))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []sessions.Message
	size := 0
	for rows.Next() {
		var idx, ms, ps int64
		var typ int
		if err := rows.Scan(&idx, &typ, &ms, &ps); err != nil {
			return nil, false, err
		}
		text, role, err := messageText(blob(ctx, tx, "steps", "step_payload", idx, ps), typ)
		if err != nil {
			return nil, false, err
		}
		if text.size == 0 && len(structure[idx].Calls) == 0 {
			continue
		}
		if len(out) == limit {
			return out, true, nil
		}
		m := structure[idx]
		m.ID, m.Seq, m.Role = idx+1, idx, role
		if hasTranscript {
			m.Kind = "text"
			model, err := blob(ctx, tx, "steps", "metadata", idx, ms).child(24)
			if err != nil {
				return nil, false, err
			}
			model, err = model.child(8)
			if err != nil {
				return nil, false, err
			}
			m.Model, _, err = model.text(4096)
			if err != nil {
				return nil, false, err
			}
		}
		m.Time, err = blob(ctx, tx, "steps", "metadata", idx, ms).timestamp(1)
		if err != nil {
			return nil, false, err
		}
		textLimit := sessions.MaxText
		if hasTranscript {
			textLimit = int(text.size)
		}
		m.Text, m.Truncated, err = text.text(textLimit)
		if err != nil {
			return nil, false, err
		}
		size += sessions.MessageSize(m)
		if size > budget && len(out) > 0 {
			return out, true, nil
		}
		out = append(out, m)
	}
	return out, false, rows.Err()
}

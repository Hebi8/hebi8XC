package wake

import (
	"context"
	"database/sql"

	"github.com/Hebi8/hebi8XC/internal/sessions"
)

// Both the stamp and the rows are compared: a stock writer can reparse a
// source without changing its size or modification time.
const structureJoin = ` LEFT JOIN structure_sessions st ON st.session_key=s.key `
const validStructure = `coalesce(st.format=(SELECT value FROM schema_meta WHERE key='structure_format') AND st.file_mtime=s.file_mtime AND st.file_size=s.file_size
 AND st.fts_format=(SELECT value FROM schema_meta WHERE key='fts_format') AND st.rows=(SELECT count(*) FROM messages m WHERE m.session_key=s.key),0)`

var structureColumns = map[string][]string{
	"schema_meta":        {"key", "value"},
	"tool_calls":         {"session_key", "seq", "idx", "call_id", "name", "input", "output", "is_error", "child_id"},
	"message_extras":     {"session_key", "seq", "kind", "thinking", "model", "full_text"},
	"structure_sessions": {"session_key", "file_mtime", "file_size", "fts_format", "rows", "format"},
}

func (x *Index) note(key, message string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.said[key] {
		x.said[key] = true
		if key != "" {
			x.Log.Info(message, "session", key)
		} else {
			x.Log.Info(message)
		}
	}
}

func (x *Index) structure(ctx context.Context, tx *sql.Tx) (string, error) {
	supported, err := hasColumns(ctx, tx, structureColumns)
	if err != nil {
		return "", err
	}
	var format string
	if supported {
		if err := tx.QueryRowContext(ctx, "SELECT coalesce((SELECT value FROM schema_meta WHERE key='structure_format'),'')").Scan(&format); err != nil {
			return "", err
		}
		switch format {
		case "1":
			return format, nil
		case "2":
			supported, err = hasColumns(ctx, tx, map[string][]string{
				"message_extras": {"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"},
				"tool_files":     {"session_key", "seq", "idx", "path", "op"},
			})
			if err != nil {
				return "", err
			}
			if supported {
				return format, nil
			}
		}
	}
	x.note("", "Wake index has no supported structure; showing text only")
	return "", nil
}

func hasColumns(ctx context.Context, tx *sql.Tx, tables map[string][]string) (bool, error) {
	for table, columns := range tables {
		rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			return false, err
		}
		have := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return false, err
			}
			have[name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		for _, c := range columns {
			if !have[c] {
				return false, nil
			}
		}
	}
	return true, nil
}

func usage(input, output, cacheRead, cacheWrite sql.NullInt64) *sessions.Usage {
	if !input.Valid && !output.Valid && !cacheRead.Valid && !cacheWrite.Valid {
		return nil
	}
	return &sessions.Usage{Input: input.Int64, Output: output.Int64, CacheRead: cacheRead.Int64, CacheWrite: cacheWrite.Int64}
}

func sessionFiles(ctx context.Context, tx *sql.Tx, s *sessions.Session) error {
	rows, err := tx.QueryContext(ctx, `SELECT f.path,f.op,c.is_error
 FROM tool_files f JOIN tool_calls c ON c.session_key=f.session_key AND c.seq=f.seq AND c.idx=f.idx
 JOIN messages m ON m.session_key=f.session_key AND m.seq=f.seq
 WHERE f.session_key=? ORDER BY f.seq,f.idx,f.path,f.op`, s.Key)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f sessions.File
		var failed bool
		if err := rows.Scan(&f.Path, &f.Op, &failed); err != nil {
			return err
		}
		s.AddFiles([]sessions.File{f}, !failed)
		if len(s.Files) == sessions.MaxSessionFiles {
			break
		}
	}
	return rows.Err()
}

func callFiles(ctx context.Context, tx *sql.Tx, key string, seq, idx int64) ([]sessions.File, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path,op FROM tool_files WHERE session_key=? AND seq=? AND idx=? ORDER BY path,op`, key, seq, idx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []sessions.File
	for rows.Next() {
		var f sessions.File
		if err := rows.Scan(&f.Path, &f.Op); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

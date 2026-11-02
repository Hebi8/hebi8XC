package wake

import (
	"context"
	"database/sql"
)

// Both the stamp and the rows are compared: a stock writer can reparse a
// source without changing its size or modification time.
const structureJoin = ` LEFT JOIN structure_sessions st ON st.session_key=s.key `
const validStructure = `coalesce(st.format='1' AND st.file_mtime=s.file_mtime AND st.file_size=s.file_size
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

func (x *Index) structure(ctx context.Context, tx *sql.Tx) (bool, error) {
	for table, columns := range structureColumns {
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
				x.note("", "Wake index has no supported structure; showing text only")
				return false, nil
			}
		}
	}
	var format string
	err := tx.QueryRowContext(ctx, "SELECT coalesce((SELECT value FROM schema_meta WHERE key='structure_format'),'')").Scan(&format)
	if err != nil {
		return false, err
	}
	if format != "1" {
		x.note("", "Wake index has no supported structure; showing text only")
		return false, nil
	}
	return true, nil
}

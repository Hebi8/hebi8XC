package waketest

import (
	"strings"

	"github.com/Hebi8/hebi8XC/internal/sessions"
)

// StructureSchemaV1 preserves the original fork schema for compatibility tests.
const StructureSchemaV1 = `
CREATE TABLE IF NOT EXISTS tool_calls (
 session_key TEXT NOT NULL,seq INTEGER NOT NULL,idx INTEGER NOT NULL,call_id TEXT NOT NULL,
 name TEXT NOT NULL,input TEXT,output TEXT,is_error INTEGER NOT NULL,child_id TEXT,
 PRIMARY KEY(session_key,seq,idx));
CREATE TABLE IF NOT EXISTS message_extras (
 session_key TEXT NOT NULL,seq INTEGER NOT NULL,kind TEXT NOT NULL,thinking TEXT,model TEXT,full_text TEXT,
 PRIMARY KEY(session_key,seq));
CREATE TABLE IF NOT EXISTS structure_sessions (
 session_key TEXT PRIMARY KEY,file_mtime INTEGER NOT NULL,file_size INTEGER NOT NULL,
 fts_format TEXT NOT NULL,rows INTEGER NOT NULL,format TEXT NOT NULL);
INSERT OR REPLACE INTO schema_meta VALUES ('structure_format','1');
`

// StructureSchema is fork format 2, including nullable usage and file arguments.
var StructureSchema = strings.Replace(StructureSchemaV1, "full_text TEXT,", `full_text TEXT,
 input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER,`, 1) + `
CREATE TABLE IF NOT EXISTS tool_files (
 session_key TEXT NOT NULL,seq INTEGER NOT NULL,idx INTEGER NOT NULL,path TEXT NOT NULL,op TEXT NOT NULL,
 PRIMARY KEY(session_key,seq,idx,path,op));
INSERT OR REPLACE INTO schema_meta VALUES ('structure_format','2');
`

// Structure adds the extras for an indexed message. Text is full_text; leave
// it empty for a row whose indexed text was not cut.
func (d *DB) Structure(key string, seq int64, m sessions.Message) {
	// Upgrade a format-1 fixture without changing its source rows or stamp.
	var columns int
	if err := d.db.QueryRow("SELECT count(*) FROM pragma_table_info('message_extras')").Scan(&columns); err != nil {
		d.t.Fatal(err)
	}
	if columns == 6 {
		for _, column := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"} {
			d.Exec("ALTER TABLE message_extras ADD COLUMN " + column + " INTEGER")
		}
	}
	d.Exec(StructureSchema)
	var input, output, cacheRead, cacheWrite any
	if m.Usage != nil {
		input, output, cacheRead, cacheWrite = m.Usage.Input, m.Usage.Output, m.Usage.CacheRead, m.Usage.CacheWrite
	}
	d.Exec(`INSERT OR REPLACE INTO message_extras VALUES (?,?,?,?,?,?,?,?,?,?)`, key, seq, "text", null(m.Thinking), null(m.Model), null(m.Text), input, output, cacheRead, cacheWrite)
	d.Exec(`DELETE FROM tool_files WHERE session_key=? AND seq=?`, key, seq)
	d.Exec(`DELETE FROM tool_calls WHERE session_key=? AND seq=?`, key, seq)
	for i, c := range m.Calls {
		d.Exec(`INSERT INTO tool_calls VALUES (?,?,?,?,?,?,?,?,?)`, key, seq, i, c.Call, c.Tool, null(c.Input), null(c.Output), c.Error, null(c.Agent))
		for _, f := range c.Files {
			d.Exec(`INSERT OR IGNORE INTO tool_files VALUES (?,?,?,?,?)`, key, seq, i, f.Path, f.Op)
		}
	}
	d.SealStructure(key)
}

func (d *DB) SealStructure(key string) {
	d.Exec(`INSERT OR REPLACE INTO structure_sessions
 SELECT key,file_mtime,file_size,(SELECT value FROM schema_meta WHERE key='fts_format'),
 (SELECT count(*) FROM messages WHERE session_key=?),(SELECT value FROM schema_meta WHERE key='structure_format') FROM sessions WHERE key=?`, key, key)
}

// StructureV1 writes only the original fields, without Phase 2 columns.
func (d *DB) StructureV1(key string, seq int64, m sessions.Message) {
	d.Exec(StructureSchemaV1)
	d.Exec(`INSERT OR REPLACE INTO message_extras(session_key,seq,kind,thinking,model,full_text) VALUES (?,?,?,?,?,?)`, key, seq, "text", null(m.Thinking), null(m.Model), null(m.Text))
	d.Exec(`DELETE FROM tool_calls WHERE session_key=? AND seq=?`, key, seq)
	for i, c := range m.Calls {
		d.Exec(`INSERT INTO tool_calls VALUES (?,?,?,?,?,?,?,?,?)`, key, seq, i, c.Call, c.Tool, null(c.Input), null(c.Output), c.Error, null(c.Agent))
	}
	d.SealStructure(key)
}

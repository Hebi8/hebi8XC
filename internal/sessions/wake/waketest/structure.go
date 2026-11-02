package waketest

import "github.com/Hebi8/hebi8XC/internal/sessions"

// StructureSchema is the optional fork format 1, beside the stock tables.
const StructureSchema = `
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

// Structure adds the extras for an indexed message. Text is full_text; leave
// it empty for a row whose indexed text was not cut.
func (d *DB) Structure(key string, seq int64, m sessions.Message) {
	d.Exec(StructureSchema)
	d.Exec(`INSERT OR REPLACE INTO message_extras VALUES (?,?,?,?,?,?)`, key, seq, "text", null(m.Thinking), null(m.Model), null(m.Text))
	d.Exec(`DELETE FROM tool_calls WHERE session_key=? AND seq=?`, key, seq)
	for i, c := range m.Calls {
		d.Exec(`INSERT INTO tool_calls VALUES (?,?,?,?,?,?,?,?,?)`, key, seq, i, c.Call, c.Tool, null(c.Input), null(c.Output), c.Error, null(c.Agent))
	}
	d.SealStructure(key)
}

func (d *DB) SealStructure(key string) {
	d.Exec(`INSERT OR REPLACE INTO structure_sessions
 SELECT key,file_mtime,file_size,(SELECT value FROM schema_meta WHERE key='fts_format'),
 (SELECT count(*) FROM messages WHERE session_key=?),'1' FROM sessions WHERE key=?`, key, key)
}

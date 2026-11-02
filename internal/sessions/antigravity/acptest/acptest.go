// Package acptest makes fictional ACP databases. It never opens real sessions.
package acptest

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type TB interface {
	Helper()
	Fatal(...any)
	Cleanup(func())
}

type DB struct {
	Path string
	SQL  *sql.DB
	t    TB
}

func New(t TB, dir, id string) *DB {
	t.Helper()
	path := filepath.Join(dir, id+".db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	d := &DB{path, db, t}
	t.Cleanup(func() { db.Close() })
	d.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY,step_type INTEGER NOT NULL DEFAULT 0,status INTEGER NOT NULL DEFAULT 0,metadata BLOB,step_payload BLOB);
 CREATE TABLE trajectory_metadata_blob (id TEXT PRIMARY KEY DEFAULT 'main',data BLOB);
 INSERT INTO trajectory_metadata_blob VALUES ('main',X'');`)
	return d
}
func (d *DB) Exec(q string, args ...any) {
	d.t.Helper()
	if _, err := d.SQL.Exec(q, args...); err != nil {
		d.t.Fatal(err)
	}
}
func (d *DB) CWD(path string) {
	d.t.Helper()
	b, _ := json.Marshal(map[string]string{"cwd": path})
	if err := os.WriteFile(d.Path[:len(d.Path)-3]+".meta", b, 0600); err != nil {
		d.t.Fatal(err)
	}
}
func Bytes(number uint64, b []byte) []byte {
	out := binary.AppendUvarint(nil, number<<3|2)
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}
func Int(number, value uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, number<<3), value)
}
func Join(parts ...[]byte) []byte {
	var b []byte
	for _, part := range parts {
		b = append(b, part...)
	}
	return b
}
func Timestamp(ms int64) []byte {
	return Join(Int(1, uint64(ms/1000)), Int(2, uint64(ms%1000)*1_000_000))
}

func (d *DB) Put(idx int64, role, text, modified, model string, ms int64) {
	typ, number := 14, uint64(19)
	body := Bytes(2, []byte(text))
	if role == "assistant" {
		typ, number = 15, 20
		body = Join(Bytes(1, []byte(text)), Bytes(8, []byte(modified)), Bytes(3, []byte("private thinking")))
	}
	meta := Join(Bytes(1, Timestamp(ms)), Bytes(8, Timestamp(ms+50)), Bytes(24, Bytes(8, []byte(model))))
	payload := Join(Int(1, uint64(typ)), Bytes(5, meta), Bytes(number, body))
	d.Exec("INSERT OR REPLACE INTO steps VALUES (?, ?, 3, ?, ?)", idx, typ, meta, payload)
}

// Transcript writes fictional step records beside the conversation database.
func (d *DB) Transcript(steps ...any) {
	d.t.Helper()
	path := filepath.Join(filepath.Dir(filepath.Dir(d.Path)), "brain", filepath.Base(d.Path[:len(d.Path)-3]), ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		d.t.Fatal(err)
	}
	var data []byte
	for _, step := range steps {
		b, err := json.Marshal(step)
		if err != nil {
			d.t.Fatal(err)
		}
		data = append(data, b...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		d.t.Fatal(err)
	}
}

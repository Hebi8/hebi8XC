package sessions

import (
	"context"
	"encoding/json"
	"strings"
)

// Search storage is additive. Triggers also run when a Phase 2 binary writes
// after a rollback; meta.schema stays 2 and no existing table changes.
func (s *Store) migrateSearch() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='search_docs'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	_, err = tx.Exec(`
 CREATE TABLE search_docs (rowid INTEGER PRIMARY KEY,node TEXT,key TEXT,gen INTEGER,id INTEGER,field TEXT,call INTEGER,text TEXT);
 CREATE INDEX search_docs_message ON search_docs(node,key,gen,id);
 CREATE VIRTUAL TABLE search_fts USING fts5(text,content='search_docs',content_rowid='rowid',tokenize='trigram case_sensitive 0');
 CREATE TABLE search_files (node TEXT,key TEXT,gen INTEGER,id INTEGER,call INTEGER,path TEXT,op TEXT,error INTEGER);
 CREATE INDEX search_files_message ON search_files(node,key,gen,id);
 CREATE INDEX search_files_path ON search_files(path);
 CREATE TRIGGER search_docs_insert AFTER INSERT ON search_docs BEGIN
 INSERT INTO search_fts(rowid,text) VALUES(new.rowid,new.text); END;
 CREATE TRIGGER search_docs_delete AFTER DELETE ON search_docs BEGIN
 INSERT INTO search_fts(search_fts,rowid,text) VALUES('delete',old.rowid,old.text); END;
 `)
	if err != nil {
		return err
	}
	// SQLite substr(blob) caps bytes; casting drops at most a trailing partial
	// rune at the Go reader, so each independent indexed value stays at 4 KiB.
	insert := func(from, where string) string {
		return `
 INSERT INTO search_docs(node,key,gen,id,field,call,text)
 SELECT m.node,m.key,m.gen,m.id,'text',-1,cast(substr(cast(m.text AS blob),1,4096) AS text) FROM ` + from + ` m ` + where + `;
 INSERT INTO search_docs(node,key,gen,id,field,call,text)
 SELECT m.node,m.key,m.gen,m.id,'thinking',-1,cast(substr(cast(m.thinking AS blob),1,4096) AS text) FROM ` + from + ` m ` + where + `;
 INSERT INTO search_docs(node,key,gen,id,field,call,text)
 SELECT m.node,m.key,m.gen,m.id,'input',c.key,cast(substr(cast(coalesce(json_extract(c.value,'$.input'),'') AS blob),1,4096) AS text)
 FROM ` + from + ` m, json_each(m.calls) c ` + where + `;
 INSERT INTO search_files(node,key,gen,id,call,path,op,error)
 SELECT m.node,m.key,m.gen,m.id,c.key,json_extract(f.value,'$.path'),json_extract(f.value,'$.op'),coalesce(json_extract(c.value,'$.error'),0)
 FROM ` + from + ` m,json_each(m.calls) c,json_each(c.value,'$.files') f ` + where + `;`
	}
	remove := `DELETE FROM search_docs WHERE node=old.node AND key=old.key AND gen=old.gen AND id=old.id;
 DELETE FROM search_files WHERE node=old.node AND key=old.key AND gen=old.gen AND id=old.id;`
	// BEFORE INSERT handles INSERT OR REPLACE even with recursive_triggers off.
	_, err = tx.Exec(`CREATE TRIGGER search_messages_replace BEFORE INSERT ON messages BEGIN
 DELETE FROM search_docs WHERE node=new.node AND key=new.key AND gen=new.gen AND id=new.id;
 DELETE FROM search_files WHERE node=new.node AND key=new.key AND gen=new.gen AND id=new.id; END;
 CREATE TRIGGER search_messages_insert AFTER INSERT ON messages BEGIN ` + insert("messages", "WHERE m.node=new.node AND m.key=new.key AND m.gen=new.gen AND m.id=new.id") + ` END;
 CREATE TRIGGER search_messages_delete AFTER DELETE ON messages BEGIN ` + remove + ` END;
 CREATE TRIGGER search_messages_update AFTER UPDATE ON messages BEGIN ` + remove + insert("messages", "WHERE m.node=new.node AND m.key=new.key AND m.gen=new.gen AND m.id=new.id") + ` END;` + insert("messages", ""))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SearchCopies(ctx context.Context, node string, q SearchQuery) (LocalSearchResult, bool, error) {
	out := LocalSearchResult{}
	var count, partial int
	err := s.db.QueryRowContext(ctx, `SELECT count(*),coalesce(max(c.format=0 OR c.complete=0),0) FROM content c JOIN sessions s ON s.node=c.node AND s.key=c.key WHERE c.node=? AND s.deleted=0`, node).Scan(&count, &partial)
	if err != nil || count == 0 {
		return out, count > 0, err
	}
	out.Partial = partial != 0
	where := []string{"m.node=?", "s.deleted=0"}
	args := []any{node}
	if q.Agent != "" {
		where = append(where, "s.agent=?")
		args = append(args, q.Agent)
	}
	if q.Project != "" {
		where = append(where, `(s.project LIKE ? ESCAPE '\' OR s.project_path LIKE ? ESCAPE '\')`)
		args = append(args, like(q.Project), like(q.Project))
	}
	if q.Since > 0 {
		where = append(where, "coalesce(nullif(m.ts,0),s.updated_at)>=?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		where = append(where, "coalesce(nullif(m.ts,0),s.updated_at)<=?")
		args = append(args, q.Until)
	}
	var indexed []string
	for _, t := range q.Terms {
		if len([]rune(t)) >= 3 {
			indexed = append(indexed, t)
		}
	}
	if len(q.Terms) > 0 {
		textWhere := []string{"d.node=m.node", "d.key=m.key", "d.gen=m.gen", "d.id=m.id"}
		if len(indexed) > 0 {
			textWhere = append(textWhere, "d.rowid IN (SELECT rowid FROM search_fts WHERE search_fts MATCH ?)")
			args = append(args, FTSPhrase(indexed))
		}
		for _, t := range q.Terms {
			textWhere = append(textWhere, `d.text LIKE ? ESCAPE '\'`)
			args = append(args, like(t))
		}
		where = append(where, "EXISTS (SELECT 1 FROM search_docs d WHERE "+strings.Join(textWhere, " AND ")+")")
	}
	if q.File != "" {
		// Boundary-aware suffixes are checked again by MatchMessage. LIKE only
		// narrows the candidates, and remains literal even for wildcard paths.
		where = append(where, `EXISTS (SELECT 1 FROM search_files f WHERE f.node=m.node AND f.key=m.key AND f.gen=m.gen AND f.id=m.id AND replace(f.path,char(92),'/') LIKE ? ESCAPE '\')`)
		args = append(args, like(strings.ReplaceAll(q.File, `\`, "/")))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.seq,m.ts,m.text,m.kind,m.thinking,m.calls,s.key,s.title,s.project,s.updated_at,c.rev,c.format
 FROM messages m JOIN content c ON c.node=m.node AND c.key=m.key AND c.gen=m.gen
 JOIN sessions s ON s.node=m.node AND s.key=m.key WHERE `+strings.Join(where, " AND ")+` ORDER BY coalesce(nullif(m.ts,0),s.updated_at) DESC,s.key,m.id LIMIT 10001`, args...)
	if err != nil {
		return out, true, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if n > SearchCandidates {
			return out, true, ErrSearchBound
		}
		var m Message
		var sess Session
		var rev int64
		var format int
		var calls string
		if err := rows.Scan(&m.ID, &m.Seq, &m.Time, &m.Text, &m.Kind, &m.Thinking, &calls, &sess.Key, &sess.Title, &sess.Project, &sess.UpdatedAt, &rev, &format); err != nil {
			return out, true, err
		}
		if err := json.Unmarshal([]byte(calls), &m.Calls); err != nil {
			return out, true, err
		}
		if format == 0 && q.NeedsStructure() {
			continue
		}
		m.Text, _ = cutBytes(m.Text, 4096)
		m.Thinking, _ = cutBytes(m.Thinking, 4096)
		for i := range m.Calls {
			m.Calls[i].Input, _ = cutBytes(m.Calls[i].Input, 4096)
		}
		hits := MatchMessage(q, sess, m)
		for i := range hits {
			hits[i].Rev = rev
		}
		out.Hits = append(out.Hits, hits...)
		if !q.BySession && len(out.Hits) > q.Limit {
			break
		}
	}
	return FinishSearch(out, q), true, rows.Err()
}

package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"
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
	var version string
	tx.QueryRow("SELECT v FROM meta WHERE k='search_schema'").Scan(&version)
	if version == "2" {
		return nil
	}
	if exists == 0 {
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
	}
	// SQLite substr(blob) caps bytes; casting drops at most a trailing partial
	// rune at the Go reader, so each independent indexed value stays at 4 KiB.
	insert := func(from, where string) string {
		return `
 INSERT INTO search_docs(node,key,gen,id,field,call,text)
 SELECT m.node,m.key,m.gen,m.id,'text',-1,cast(substr(cast(m.text AS blob),1,4096) AS text) FROM ` + from + ` m ` + where + ` AND m.text<>'';
 INSERT INTO search_docs(node,key,gen,id,field,call,text)
 SELECT m.node,m.key,m.gen,m.id,'thinking',-1,cast(substr(cast(m.thinking AS blob),1,4096) AS text) FROM ` + from + ` m ` + where + ` AND m.thinking<>'';
 INSERT INTO search_docs(node,key,gen,id,field,call,text)
 SELECT m.node,m.key,m.gen,m.id,'input',c.key,cast(substr(cast(coalesce(json_extract(c.value,'$.input'),'') AS blob),1,4096) AS text)
 FROM ` + from + ` m, json_each(m.calls) c ` + where + ` AND c.type='object' AND coalesce(json_extract(c.value,'$.input'),'')<>'';
 INSERT INTO search_files(node,key,gen,id,call,path,op,error)
 SELECT m.node,m.key,m.gen,m.id,c.key,json_extract(f.value,'$.path'),json_extract(f.value,'$.op'),coalesce(json_extract(c.value,'$.error'),0)
 FROM ` + from + ` m,json_each(m.calls) c,json_each(c.value,'$.files') f ` + where + `;`
	}
	remove := `DELETE FROM search_docs WHERE node=old.node AND key=old.key AND gen=old.gen AND id=old.id;
 DELETE FROM search_files WHERE node=old.node AND key=old.key AND gen=old.gen AND id=old.id;`
	// BEFORE INSERT handles INSERT OR REPLACE even with recursive_triggers off.
	_, err = tx.Exec(`DROP TRIGGER IF EXISTS search_messages_replace; DROP TRIGGER IF EXISTS search_messages_insert; DROP TRIGGER IF EXISTS search_messages_delete; DROP TRIGGER IF EXISTS search_messages_update;`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`CREATE TRIGGER search_messages_replace BEFORE INSERT ON messages BEGIN
 DELETE FROM search_docs WHERE node=new.node AND key=new.key AND gen=new.gen AND id=new.id;
 DELETE FROM search_files WHERE node=new.node AND key=new.key AND gen=new.gen AND id=new.id; END;
 CREATE TRIGGER search_messages_insert AFTER INSERT ON messages BEGIN ` + insert("messages", "WHERE m.node=new.node AND m.key=new.key AND m.gen=new.gen AND m.id=new.id") + ` END;
 CREATE TRIGGER search_messages_delete AFTER DELETE ON messages BEGIN ` + remove + ` END;
 CREATE TRIGGER search_messages_update AFTER UPDATE ON messages BEGIN ` + remove + insert("messages", "WHERE m.node=new.node AND m.key=new.key AND m.gen=new.gen AND m.id=new.id") + ` END;`)
	if err != nil {
		return err
	}
	if exists == 0 {
		_, err = tx.Exec(insert("messages", "WHERE 1"))
	} else {
		_, err = tx.Exec("DELETE FROM search_docs WHERE text='' OR text IS NULL")
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO meta(k,v) VALUES ('search_schema','2')"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SearchCopies(ctx context.Context, node string, q SearchQuery) (out LocalSearchResult, held bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var through int64
	defer func() {
		if SearchInterrupted(ctx, err) {
			out.Bound(through, !errors.Is(err, ErrSearchBound))
			err = nil
		}
		if err != nil {
			var code interface{ Code() int }
			if errors.As(err, &code) && (code.Code()&255) == 5 {
				out.Partial = true
				out.Note = JoinNote(out.Note, "copy search waited for a writer for 5 seconds; results are incomplete; retry after the copy finishes")
				err = nil
			}
		}
		out = FinishSearch(out, q)
	}()
	// A dedicated read connection avoids monopolizing the store's write/feed
	// connection. One read snapshot covers both coverage checks and results.
	dsn := (&url.URL{Scheme: "file", OmitHost: true, Path: filepath.ToSlash(s.path), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return out, false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback()
	var count int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM content WHERE node=?", node).Scan(&count)
	held = count > 0
	if err != nil || !held {
		return out, held, err
	}
	where := []string{"s.node=?", "s.deleted=0"}
	args := []any{node}
	if q.Agent != "" {
		where = append(where, "s.agent=?")
		args = append(args, q.Agent)
	}
	if q.Project != "" {
		where = append(where, `(s.project LIKE ? ESCAPE '\' OR s.project_path LIKE ? ESCAPE '\')`)
		args = append(args, Like(q.Project), Like(q.Project))
	}
	scopeWhere := append([]string{}, where...)
	scopeArgs := append([]any{}, args...)
	if q.Since > 0 {
		scopeWhere = append(scopeWhere, "s.updated_at>=?")
		scopeArgs = append(scopeArgs, q.Since)
	}
	if q.Until > 0 {
		scopeWhere = append(scopeWhere, "s.created_at<=?")
		scopeArgs = append(scopeArgs, q.Until)
	}
	if q.Since > 0 {
		where = append(where, "coalesce(nullif(m.ts,0),s.updated_at)>=?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		where = append(where, "coalesce(nullif(m.ts,0),s.updated_at)<=?")
		args = append(args, q.Until)
	}
	from := ` FROM messages m JOIN content c ON c.node=m.node AND c.key=m.key AND c.gen=m.gen JOIN sessions s ON s.node=m.node AND s.key=m.key LEFT JOIN copy_structure cs ON cs.node=c.node AND cs.key=c.key AND cs.gen=c.gen AND cs.rev=c.rev `
	// An incomplete copy can hide later assistant rows even if its retained
	// prefix contains only users or lies before the message time window.
	affectedSQL := `SELECT s.key FROM content c JOIN sessions s ON s.node=c.node AND s.key=c.key WHERE ` + strings.Join(scopeWhere, " AND ") + ` AND c.complete=0`
	affectedArgs := append([]any{}, scopeArgs...)
	missing := []string{"c.complete=0"}
	if q.NeedsStructure() {
		structural := []string{"m.kind=''"}
		if q.FileFilter() {
			structural = append(structural, "coalesce(cs.level,0)<2")
		}
		if q.CallFilter() {
			structural = append(structural, "m.calls_dropped>0")
		}
		condition := "(" + strings.Join(structural, " OR ") + ")"
		if q.CallFilter() {
			condition = "(m.role<>'user' AND " + condition + ")"
		}
		missing = append(missing, condition)
	}
	affectedSQL += " UNION SELECT s.key" + from + " WHERE " + strings.Join(where, " AND ") + " AND (" + strings.Join(missing, " OR ") + ")"
	affectedArgs = append(affectedArgs, args...)
	var affected int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM ("+affectedSQL+")", affectedArgs...).Scan(&affected)
	if err != nil {
		return out, held, err
	}
	reason := "incomplete copies"
	if q.NeedsStructure() {
		reason = "message structure or complete copies unavailable"
	}
	if q.FileFilter() {
		reason = "file records or complete copies unavailable"
	}
	out.Missing(affected, reason)
	if q.NeedsStructure() {
		where = append(where, "m.kind<>''")
	}
	if q.Kind != "" {
		where = append(where, "m.kind=?")
		args = append(args, q.Kind)
	}
	callWhere := []string{"1"}
	var callArgs []any
	if q.Tool != "" {
		callWhere = append(callWhere, "json_extract(tc.value,'$.tool')=?")
		callArgs = append(callArgs, q.Tool)
	}
	if q.Errors {
		callWhere = append(callWhere, "json_extract(tc.value,'$.error')=1")
	}
	if q.FileFilter() {
		file, fa := FileSQL("f.path", q.File)
		op, oa := q.OpSQL("f.op")
		cf := []string{"f.node=m.node", "f.key=m.key", "f.gen=m.gen", "f.id=m.id", "f.call=tc.key", file, op}
		if !q.Failed && !q.Errors {
			cf = append(cf, "f.error=0")
		}
		callWhere = append(callWhere, "EXISTS(SELECT 1 FROM search_files f WHERE "+strings.Join(cf, " AND ")+")")
		callArgs = append(callArgs, fa...)
		callArgs = append(callArgs, oa...)
	}
	if q.CallFilter() {
		where = append(where, "EXISTS(SELECT 1 FROM json_each(m.calls) tc WHERE "+strings.Join(callWhere, " AND ")+")")
		args = append(args, callArgs...)
	}
	if len(q.Terms) > 0 {
		docs := []string{"d.node=m.node", "d.key=m.key", "d.gen=m.gen", "d.id=m.id"}
		var tri []string
		for _, t := range q.Terms {
			if len([]rune(t)) >= 3 {
				tri = append(tri, t)
			}
		}
		if len(tri) > 0 {
			docs = append(docs, "+d.rowid IN (SELECT rowid FROM search_fts WHERE search_fts MATCH ?)")
			args = append(args, FTSPhrase(tri))
		}
		for _, t := range q.Terms {
			if len([]rune(t)) < 3 {
				docs = append(docs, `d.text LIKE ? ESCAPE '\'`)
				args = append(args, Like(t))
			}
		}
		where = append(where, "EXISTS(SELECT 1 FROM search_docs d WHERE "+strings.Join(docs, " AND ")+")")
	}
	if q.BySession {
		// Aggregate hit counts inside SQLite; no full message/call blobs cross into
		// Go for grouped searches. The 10001st candidate exposes the work cutoff.
		base := `WITH base AS MATERIALIZED (SELECT m.*,s.title,s.project,s.updated_at,c.rev` + from + " WHERE " + strings.Join(where, " AND ") + ` ORDER BY coalesce(nullif(m.ts,0),s.updated_at) DESC,s.key,m.id LIMIT 10001), candidates AS (SELECT * FROM base LIMIT 10000), hits AS (`
		ga := append([]any{}, args...)
		docMatch := func(field string) (string, []any) {
			if len(q.Terms) == 0 {
				return "1", nil
			}
			d := []string{"d.node=m.node", "d.key=m.key", "d.gen=m.gen", "d.id=m.id", field}
			var a []any
			var tri []string
			for _, t := range q.Terms {
				if len([]rune(t)) >= 3 {
					tri = append(tri, t)
				}
			}
			if len(tri) > 0 {
				d = append(d, "+d.rowid IN (SELECT rowid FROM search_fts WHERE search_fts MATCH ?)")
				a = append(a, FTSPhrase(tri))
			}
			for _, t := range q.Terms {
				if len([]rune(t)) < 3 {
					d = append(d, `d.text LIKE ? ESCAPE '\'`)
					a = append(a, Like(t))
				}
			}
			return "EXISTS(SELECT 1 FROM search_docs d WHERE " + strings.Join(d, " AND ") + ")", a
		}
		var branches []string
		if !q.CallFilter() {
			cond, a := docMatch("d.field IN ('text','thinking')")
			branches = append(branches, "SELECT m.key,m.id,m.title,m.project,m.updated_at,m.rev FROM candidates m WHERE "+cond)
			ga = append(ga, a...)
		}
		if q.CallFilter() || len(q.Terms) > 0 {
			cond, a := docMatch("d.field='input' AND d.call=tc.key")
			if q.CallFilter() {
				body, ba := docMatch("d.field IN ('text','thinking')")
				cond = "(" + cond + " OR " + body + ")"
				a = append(a, ba...)
			}
			branches = append(branches, "SELECT m.key,m.id,m.title,m.project,m.updated_at,m.rev FROM candidates m,json_each(m.calls) tc WHERE tc.type='object' AND "+strings.Join(callWhere, " AND ")+" AND "+cond)
			ga = append(ga, callArgs...)
			ga = append(ga, a...)
		}
		rows, e := tx.QueryContext(ctx, base+strings.Join(branches, " UNION ALL ")+`) SELECT key,title,project,updated_at,rev,count(*),min(id),max(id),(SELECT count(*) FROM base)>10000 FROM hits GROUP BY key UNION ALL SELECT '', '', '',0,0,0,0,0,1 WHERE (SELECT count(*) FROM base)>10000 AND NOT EXISTS(SELECT 1 FROM hits) ORDER BY updated_at DESC,key LIMIT ?`, append(ga, q.Limit+1)...)
		if e != nil {
			return out, held, e
		}
		defer rows.Close()
		for rows.Next() {
			var g SearchSession
			var partial bool
			if e = rows.Scan(&g.Key, &g.Title, &g.Project, &g.UpdatedAt, &g.Rev, &g.Hits, &g.First, &g.Last, &partial); e != nil {
				return out, held, e
			}
			if partial {
				out.Bound(0, false)
			}
			if g.Hits > 0 {
				out.Sessions = append(out.Sessions, g)
			}
		}
		return out, held, rows.Err()
	}
	// One statement observes one SQLite snapshot even if another generation
	// replaces the copy during this search. No OFFSET can skip or repeat rows.
	rows, e := tx.QueryContext(ctx, `SELECT m.id,m.seq,m.ts,m.text,m.kind,m.thinking,m.calls,s.key,s.title,s.project,s.project_path,s.agent,s.updated_at,c.rev`+from+" WHERE "+strings.Join(where, " AND ")+` ORDER BY coalesce(nullif(m.ts,0),s.updated_at) DESC,s.key,m.id LIMIT 10001`, args...)
	if e != nil {
		return out, held, e
	}
	defer rows.Close()
	read := 0
	for rows.Next() {
		if read >= SearchCandidates {
			out.Bound(through, false)
			break
		}
		var m Message
		var sess Session
		var rev int64
		var calls string
		if e = rows.Scan(&m.ID, &m.Seq, &m.Time, &m.Text, &m.Kind, &m.Thinking, &calls, &sess.Key, &sess.Title, &sess.Project, &sess.ProjectPath, &sess.Agent, &sess.UpdatedAt, &rev); e != nil {
			return out, held, e
		}
		if e = json.Unmarshal([]byte(calls), &m.Calls); e != nil {
			return out, held, e
		}
		m.Text, _ = cutBytes(m.Text, 4096)
		m.Thinking, _ = cutBytes(m.Thinking, 4096)
		for i := range m.Calls {
			m.Calls[i].Input, _ = cutBytes(m.Calls[i].Input, 4096)
		}
		through = m.Time
		if through == 0 {
			through = sess.UpdatedAt
		}
		hits := MatchMessage(q, sess, m)
		for i := range hits {
			hits[i].Rev = rev
		}
		out.Hits = append(out.Hits, hits...)
		read++
		if len(out.Hits) > q.Limit {
			break
		}
	}
	return out, held, rows.Err()
}

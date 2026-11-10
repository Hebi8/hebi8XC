package wake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/Hebi8/hebi8XC/internal/sessions"
)

func ftsDDL(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	var ddl string
	err := tx.QueryRowContext(ctx, `SELECT coalesce(sql,'') FROM sqlite_master WHERE name=? AND type='table'`, name).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ddl = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		if r == '"' {
			return '\''
		}
		return unicode.ToLower(r)
	}, ddl)
	return ddl, nil
}

// sideScanMessages bounds unindexed body/side-table work independently of the
// indexed body results. Recent rows are visited first; the cutoff is reported.
const sideScanMessages = 20000
const searchMessageJoin = ` FROM messages m JOIN scope s ON s.key=m.session_key `

func (x *Index) Search(ctx context.Context, q sessions.SearchQuery) (out sessions.LocalSearchResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var through int64
	defer func() {
		if sessions.SearchInterrupted(ctx, err) {
			out.Bound(through, !errors.Is(err, sessions.ErrSearchBound))
			err = nil
		}
		out = sessions.FinishSearch(out, q)
	}()
	db, err := x.open(ctx)
	if err != nil {
		return out, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	format, err := x.structure(ctx, tx)
	if err != nil {
		return out, err
	}
	var tri, cjk []string
	for _, t := range q.Terms {
		if len([]rune(t)) >= 3 {
			tri = append(tri, t)
		} else if sessions.CJKPair(t) {
			cjk = append(cjk, t)
		}
	}
	if len(tri) > 0 {
		ddl, e := ftsDDL(ctx, tx, "messages_fts")
		if e != nil {
			return out, e
		}
		if !strings.Contains(ddl, "usingfts5(text,content='messages',content_rowid='id',tokenize='trigramcase_sensitive0')") {
			return out, errors.New("Wake search requires the trigram external-content messages_fts table; rebuild the Wake index")
		}
	}
	if len(cjk) > 0 {
		ddl, e := ftsDDL(ctx, tx, "messages_cjk")
		if e != nil {
			return out, e
		}
		var ready bool
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_meta WHERE key='cjk_format' AND value='1') AND NOT EXISTS(SELECT 1 FROM schema_meta WHERE key='cjk_backfill')`).Scan(&ready)
		if e != nil {
			return out, e
		}
		if !ready || !strings.Contains(ddl, "usingfts5(text,content='',contentless_delete=1,tokenize='ascii')") {
			cjk = nil
		}
	}
	cte, args := searchScope(q, format)
	from := searchMessageJoin
	where := []string{"1"}
	if q.Since > 0 {
		where = append(where, "coalesce(nullif(m.ts,0),s.updated_at)>=?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		where = append(where, "coalesce(nullif(m.ts,0),s.updated_at)<=?")
		args = append(args, q.Until)
	}
	if q.NeedsStructure() {
		missing := "s.valid=0"
		if q.FileFilter() && format != "2" {
			missing = "1"
		}
		var affected int
		relevant := ""
		if q.CallFilter() {
			relevant = " AND coalesce(m.role,'')<>'user'"
		}
		err = tx.QueryRowContext(ctx, cte+`SELECT count(DISTINCT m.session_key)`+from+` WHERE `+strings.Join(where, " AND ")+relevant+` AND (`+missing+`)`, args...).Scan(&affected)
		if err != nil {
			return out, err
		}
		reason := "message structure unavailable"
		if q.FileFilter() {
			reason = "file records unavailable"
		}
		out.Missing(affected, reason)
		where = append(where, "s.valid=1")
		if q.FileFilter() && format != "2" {
			where = append(where, "0")
		}
	}
	if q.Kind != "" && format != "" {
		where = append(where, "coalesce((SELECT kind FROM message_extras e WHERE e.session_key=m.session_key AND e.seq=m.seq),'text')=?")
		args = append(args, q.Kind)
	}
	if q.CallFilter() {
		if format == "" {
			where = append(where, "0")
		} else {
			calls := []string{"c.session_key=m.session_key", "c.seq=m.seq"}
			if q.Tool != "" {
				calls = append(calls, "c.name=?")
				args = append(args, q.Tool)
			}
			if q.Errors {
				calls = append(calls, "c.is_error=1")
			}
			if q.FileFilter() && format == "2" {
				if !q.Failed && !q.Errors {
					calls = append(calls, "c.is_error=0")
				}
				file, fa := sessions.FileSQL("f.path", q.File)
				op, oa := q.OpSQL("f.op")
				args = append(args, fa...)
				args = append(args, oa...)
				calls = append(calls, "EXISTS(SELECT 1 FROM tool_files f WHERE f.session_key=c.session_key AND f.seq=c.seq AND f.idx=c.idx AND "+file+" AND "+op+")")
			}
			where = append(where, "EXISTS(SELECT 1 FROM tool_calls c WHERE "+strings.Join(calls, " AND ")+")")
		}
	}
	type candidate struct {
		id    int64
		valid bool
		at    int64
	}
	seen := map[int64]bool{}
	load := func(candidates []candidate, stopAtLimit bool) error {
		batchSize := 128
		if stopAtLimit && !q.BySession {
			batchSize = 16
		}
		for start := 0; start < len(candidates); start += batchSize {
			batch := candidates[start:min(start+batchSize, len(candidates))]
			ids := make([]int64, 0, len(batch))
			valids := map[int64]bool{}
			for _, c := range batch {
				if !seen[c.id] {
					ids = append(ids, c.id)
					valids[c.id] = c.valid
					seen[c.id] = true
				}
				through = c.at
			}
			if len(ids) == 0 {
				continue
			}
			hits, e := x.searchBatch(ctx, tx, ids, valids, format, q)
			out.Hits = append(out.Hits, hits...)
			if stopAtLimit && !q.BySession && len(out.Hits) > q.Limit {
				out.More = true
				return e
			}
			if e != nil {
				return e
			}
		}
		return nil
	}
	// Indexed body first, so slower side-table work cannot erase its results.
	if len(q.Terms) == 0 || len(tri) > 0 || len(cjk) > 0 {
		bw := append([]string{}, where...)
		ba := append([]any{}, args...)
		if len(tri) > 0 {
			bw = append(bw, "m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)")
			ba = append(ba, sessions.FTSPhrase(tri))
		}
		if len(cjk) > 0 {
			column := "m.id"
			if len(tri) > 0 {
				column = "+m.id"
			}
			bw = append(bw, column+" IN (SELECT rowid FROM messages_cjk WHERE messages_cjk MATCH ?)")
			ba = append(ba, sessions.FTSPhrase(cjk))
		}
		for _, t := range q.Terms {
			if len([]rune(t)) < 3 && !(len(cjk) > 0 && sessions.CJKPair(t)) {
				bw = append(bw, `m.text LIKE ? ESCAPE '\'`)
				ba = append(ba, sessions.Like(t))
			}
		}
		limit := sessions.SearchCandidates
		if !q.BySession {
			limit = q.Limit + 1
		}
		ba = append(ba, limit+1)
		rows, e := tx.QueryContext(ctx, bodyCandidateQuery(cte, from, bw, len(tri) > 0 || len(cjk) > 0), ba...)
		if e != nil {
			return out, e
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if e = rows.Scan(&c.id, &c.valid, &c.at); e != nil {
				break
			}
			candidates = append(candidates, c)
		}
		re := rows.Err()
		rows.Close()
		if len(candidates) > limit {
			candidates = candidates[:limit]
			if q.BySession {
				out.Bound(candidates[len(candidates)-1].at, false)
			} else {
				out.More = true
			}
		}
		if e != nil {
			return out, e
		}
		if re != nil {
			return out, re
		}
		if e = load(candidates, true); e != nil {
			return out, e
		}
	}
	if len(q.Terms) > 0 {
		// Bound before LIKE, including short-word body scans. This range already
		// includes every metadata, time and structural predicate above.
		recent := cte[:len(cte)-1] + `, recent AS MATERIALIZED (SELECT m.id,s.valid,coalesce(nullif(m.ts,0),s.updated_at) at` + from + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY coalesce(nullif(m.ts,0),s.updated_at) DESC,m.id DESC LIMIT ` + fmt.Sprint(sideScanMessages+1) + `) `
		sa := append([]any{}, args...)
		or := []string{}
		if len(tri) == 0 && len(cjk) == 0 {
			cond, a := termsSQL("m.text", q.Terms)
			or = append(or, cond)
			sa = append(sa, a...)
		}
		if format != "" {
			for _, field := range []string{"thinking", "input"} {
				alias, table := "e", "message_extras"
				if field == "input" {
					alias, table = "c", "tool_calls"
				}
				cond, a := termsSQL(alias+"."+field, q.Terms)
				sa = append(sa, a...)
				or = append(or, "(r.valid=1 AND EXISTS(SELECT 1 FROM "+table+" "+alias+" WHERE "+alias+".session_key=m.session_key AND "+alias+".seq=m.seq AND "+cond+"))")
			}
		}
		if len(or) > 0 {
			// The materialized recent range is evaluated once. Keep its cursor open
			// while loading matched batches on the same read transaction.
			rows, e := tx.QueryContext(ctx, recent+`SELECT r.id,r.valid,r.at,(`+strings.Join(or, " OR ")+`) FROM recent r JOIN messages m ON m.id=r.id ORDER BY r.at DESC,r.id DESC`, sa...)
			if e != nil {
				return out, e
			}
			defer rows.Close()
			batch := make([]candidate, 0, 128)
			read := 0
			cut := false
			var cutoff int64
			flush := func() error { e := load(batch, false); batch = batch[:0]; through = cutoff; return e }
			for rows.Next() {
				var c candidate
				var match bool
				if e = rows.Scan(&c.id, &c.valid, &c.at, &match); e != nil {
					return out, e
				}
				read++
				if read > sideScanMessages {
					cut = true
					break
				}
				cutoff = c.at
				through = cutoff
				if match {
					// Once real hits establish a full result page, older side matches cannot
					// displace it. Still scan the range to report the coverage cutoff.
					skip := false
					if !q.BySession && len(out.Hits) > q.Limit {
						sessions.SortHits(out.Hits)
						h := out.Hits[q.Limit-1]
						at := h.Time
						if at == 0 {
							at = h.UpdatedAt
						}
						skip = c.at < at
						if skip {
							out.More = true
						}
					}
					if !skip {
						batch = append(batch, c)
					}
				}
				if len(batch) == 128 {
					if e = flush(); e != nil {
						return out, e
					}
				}
			}
			if e = rows.Err(); e != nil {
				return out, e
			}
			rows.Close()
			if e = flush(); e != nil {
				return out, e
			}
			if cut {
				if len(tri) == 0 && len(cjk) == 0 {
					out.Bound(cutoff, false)
				}
				if format != "" {
					out.SideCoverage(cutoff)
				}
			}
		}
	}
	return out, nil
}

func termsSQL(field string, terms []string) (string, []any) {
	var where []string
	var args []any
	for _, t := range terms {
		where = append(where, field+` LIKE ? ESCAPE '\'`)
		args = append(args, sessions.Like(t))
	}
	return strings.Join(where, " AND "), args
}

// Two queries load a candidate batch, rather than reopening the source schema
// and scanning side tables separately for every message.
func (x *Index) searchBatch(ctx context.Context, tx *sql.Tx, ids []int64, valid map[int64]bool, format string, q sessions.SearchQuery) ([]sessions.SearchHit, error) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args[i] = id
	}
	in := strings.Join(marks, ",")
	join, extra := "", "'', ''"
	if format != "" {
		join = " LEFT JOIN message_extras e ON e.session_key=m.session_key AND e.seq=m.seq"
		extra = "coalesce(e.kind,'text'),coalesce(e.thinking,'')"
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.seq,coalesce(m.ts,0),m.text,`+extra+`,s.key,s.title,s.project_name,s.project_path,s.agent_id,s.updated_at FROM messages m JOIN sessions s ON s.key=m.session_key`+join+` WHERE m.id IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		m sessions.Message
		s sessions.Session
	}
	items := map[int64]*row{}
	for rows.Next() {
		r := new(row)
		if err = rows.Scan(&r.m.ID, &r.m.Seq, &r.m.Time, &r.m.Text, &r.m.Kind, &r.m.Thinking, &r.s.Key, &r.s.Title, &r.s.Project, &r.s.ProjectPath, &r.s.Agent, &r.s.UpdatedAt); err != nil {
			break
		}
		if !valid[r.m.ID] {
			r.m.Kind, r.m.Thinking = "", ""
		}
		items[r.m.ID] = r
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if re != nil {
		return nil, re
	}
	if format != "" {
		files := "'[]'"
		if format == "2" {
			files = `(SELECT coalesce(json_group_array(json_object('path',f.path,'op',f.op)),'[]') FROM tool_files f WHERE f.session_key=c.session_key AND f.seq=c.seq AND f.idx=c.idx)`
		}
		rows, err = tx.QueryContext(ctx, `SELECT m.id,c.call_id,c.name,coalesce(c.input,''),c.is_error,`+files+` FROM messages m JOIN tool_calls c ON c.session_key=m.session_key AND c.seq=m.seq WHERE m.id IN (`+in+`) ORDER BY m.id,c.idx`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var c sessions.Call
			var raw string
			if err = rows.Scan(&id, &c.Call, &c.Tool, &c.Input, &c.Error, &raw); err != nil {
				break
			}
			if valid[id] {
				if err = json.Unmarshal([]byte(raw), &c.Files); err != nil {
					break
				}
				items[id].m.Calls = append(items[id].m.Calls, c)
			}
		}
		re = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if re != nil {
			return nil, re
		}
	}
	var hits []sessions.SearchHit
	for _, id := range ids {
		if r := items[id]; r != nil {
			hits = append(hits, sessions.MatchMessage(q, r.s, r.m)...)
		}
	}
	return hits, nil
}

// searchScope uses the published allow-list as the sole identity filter. A
// long chain of redundant exclusions changes SQLite cardinality estimates.
func searchScope(q sessions.SearchQuery, format string) (string, []any) {
	scope := []string{"s.host=''"}
	var args []any
	if q.Agent != "" {
		scope = append(scope, "s.agent_id=?")
		args = append(args, q.Agent)
	}
	if q.Project != "" {
		scope = append(scope, `(s.project_name LIKE ? ESCAPE '\' OR s.project_path LIKE ? ESCAPE '\')`)
		args = append(args, sessions.Like(q.Project), sessions.Like(q.Project))
	}
	if q.AllowedKeys != nil {
		keys := make([]string, 0, len(q.AllowedKeys))
		for k := range q.AllowedKeys {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		raw, _ := json.Marshal(keys)
		scope = append(scope, "s.key IN (SELECT value FROM json_each(?))")
		args = append(args, string(raw))
	}
	valid, join := "0", ""
	if format != "" {
		valid, join = validStructure, structureJoin
	}
	cte := `WITH scope AS MATERIALIZED (SELECT s.*,` + valid + ` valid FROM sessions s ` + join + ` WHERE ` + strings.Join(scope, " AND ") + `) `
	return cte, args
}

// With a large LIMIT SQLite can reverse this join even after exclusions
// are removed. Indexed body candidates must drive the materialized scope.
func bodyCandidateQuery(cte, from string, where []string, indexed bool) string {
	if indexed {
		from = strings.Replace(from, "JOIN scope s", "CROSS JOIN scope s", 1)
	}
	return cte + `SELECT m.id,s.valid,coalesce(nullif(m.ts,0),s.updated_at)` + from + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY coalesce(nullif(m.ts,0),s.updated_at) DESC,s.key,m.id ASC LIMIT ?`
}

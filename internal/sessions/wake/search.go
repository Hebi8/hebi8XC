package wake

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
func (x *Index) Search(ctx context.Context, q sessions.SearchQuery) (sessions.LocalSearchResult, error) {
	out := sessions.LocalSearchResult{}
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
	ddl, err := ftsDDL(ctx, tx, "messages_fts")
	if err != nil {
		return out, err
	}
	if len(tri) > 0 && !strings.Contains(ddl, "usingfts5(text,content='messages',content_rowid='id',tokenize='trigramcase_sensitive0')") {
		return out, errors.New("Wake search requires the trigram external-content messages_fts table; rebuild the Wake index")
	}
	if len(cjk) > 0 {
		ddl, e := ftsDDL(ctx, tx, "messages_cjk")
		if e != nil {
			return out, e
		}
		var version string
		e = tx.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='cjk_format'`).Scan(&version)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
		if version != "1" || !strings.Contains(ddl, "usingfts5(text,content='',contentless_delete=1,tokenize='ascii')") {
			cjk = nil
		}
	}
	scoped := []string{"s.host=''"}
	args := []any{}
	if q.Agent != "" {
		scoped = append(scoped, "s.agent_id=?")
		args = append(args, q.Agent)
	}
	if q.Project != "" {
		scoped = append(scoped, `(s.project_name LIKE ? ESCAPE '\' OR s.project_path LIKE ? ESCAPE '\')`)
		args = append(args, sessions.LikeLiteral(q.Project), sessions.LikeLiteral(q.Project))
	}
	for _, key := range q.ExcludeKeys {
		scoped = append(scoped, "s.key<>?")
		args = append(args, key)
	}
	valid, join := "0", ""
	if format != "" {
		valid = validStructure
		join = structureJoin
	}
	cte := `WITH scope AS MATERIALIZED (SELECT s.*,` + valid + ` AS valid FROM sessions s ` + join + ` WHERE ` + strings.Join(scoped, " AND ") + `) `
	if q.NeedsStructure() {
		missing := "valid=0"
		if q.File != "" && format != "2" {
			missing = "1"
		}
		var count int
		if err := tx.QueryRowContext(ctx, cte+`SELECT count(*) FROM scope WHERE `+missing, args...).Scan(&count); err != nil {
			return out, err
		}
		out.Partial = count > 0
	}
	// Each branch has the same metadata/time/structure scope. Indexed body
	// candidates are selected by rowid; side-table LIKE never widens that scope.
	base := []string{"1"}
	if q.Since > 0 {
		base = append(base, "coalesce(nullif(m.ts,0),s.updated_at)>=?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		base = append(base, "coalesce(nullif(m.ts,0),s.updated_at)<=?")
		args = append(args, q.Until)
	}
	if q.NeedsStructure() {
		base = append(base, "s.valid=1")
	}
	if q.File != "" && format != "2" {
		base = append(base, "0")
	}
	if q.Tool != "" || q.Errors || q.File != "" {
		if format == "" {
			base = append(base, "0")
		} else {
			call := []string{"c.session_key=m.session_key", "c.seq=m.seq"}
			if q.Tool != "" {
				call = append(call, "c.name=?")
				args = append(args, q.Tool)
			}
			if q.Errors {
				call = append(call, "c.is_error=1")
			}
			if q.File != "" && format == "2" {
				if !q.Failed {
					call = append(call, "c.is_error=0")
				}
				file := []string{"f.session_key=c.session_key", "f.seq=c.seq", "f.idx=c.idx", `replace(f.path,char(92),'/') LIKE ? ESCAPE '\'`}
				args = append(args, sessions.LikeLiteral(strings.ReplaceAll(q.File, `\`, "/")))
				if q.Op == "" {
					file = append(file, "f.op IN ('change','delete')")
				} else if q.Op != "any" {
					file = append(file, "f.op=?")
					args = append(args, q.Op)
				}
				call = append(call, "EXISTS (SELECT 1 FROM tool_files f WHERE "+strings.Join(file, " AND ")+")")
			}
			base = append(base, "EXISTS (SELECT 1 FROM tool_calls c WHERE "+strings.Join(call, " AND ")+")")
		}
	}
	if q.Kind != "" && format != "" {
		base = append(base, "coalesce((SELECT kind FROM message_extras e WHERE e.session_key=m.session_key AND e.seq=m.seq),'text')=?")
		args = append(args, q.Kind)
	}
	// Materialize only IDs and metadata; full texts are loaded for candidate rows.
	prefix := cte[:len(cte)-1] + `, bounded AS MATERIALIZED (SELECT m.id,m.seq,m.session_key FROM messages m JOIN scope s ON s.key=m.session_key WHERE ` + strings.Join(base, " AND ") + `) `
	body := []string{}
	if len(tri) > 0 {
		body = append(body, "m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)")
		args = append(args, sessions.FTSPhrase(tri))
	}
	if len(cjk) > 0 {
		body = append(body, "m.id IN (SELECT rowid FROM messages_cjk WHERE messages_cjk MATCH ?)")
		args = append(args, sessions.FTSPhrase(cjk))
	}
	for _, t := range q.Terms {
		if len([]rune(t)) >= 3 || (len(cjk) > 0 && sessions.CJKPair(t)) {
			continue
		}
		body = append(body, `m.text LIKE ? ESCAPE '\'`)
		args = append(args, sessions.LikeLiteral(t))
	}
	if len(body) == 0 {
		body = append(body, "1")
	}
	// Unary + keeps SQLite driving indexed body candidates from FTS rather
	// than doing a primary-key lookup for every id in the materialized scope.
	branches := []string{`SELECT m.id FROM messages m WHERE ` + strings.Join(body, " AND ") + ` AND +m.id IN (SELECT id FROM bounded)`}
	if format != "" && len(q.Terms) > 0 {
		for _, field := range []string{"e.thinking", "c.input"} {
			cond := []string{"s.valid=1"}
			for _, t := range q.Terms {
				cond = append(cond, field+` LIKE ? ESCAPE '\'`)
				args = append(args, sessions.LikeLiteral(t))
			}
			table, alias := "message_extras", "e"
			if field == "c.input" {
				table, alias = "tool_calls", "c"
			}
			branches = append(branches, `SELECT b.id FROM bounded b JOIN scope s ON s.key=b.session_key JOIN `+table+` `+alias+` ON `+alias+`.session_key=b.session_key AND `+alias+`.seq=b.seq WHERE `+strings.Join(cond, " AND "))
		}
	}
	filters := []string{"b.id IN (" + strings.Join(branches, " UNION ") + ")"}
	rows, err := tx.QueryContext(ctx, prefix+`SELECT b.id,s.key,s.title,s.project_name,s.updated_at FROM bounded b JOIN scope s ON s.key=b.session_key JOIN messages m ON m.id=b.id WHERE `+strings.Join(filters, " AND ")+` ORDER BY coalesce(nullif(m.ts,0),s.updated_at) DESC,s.key,m.id LIMIT 10001`, args...)
	if err != nil {
		return out, err
	}
	type candidate struct {
		id int64
		s  sessions.Session
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.s.Key, &c.s.Title, &c.s.Project, &c.s.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if q.BySession && len(candidates) > sessions.SearchCandidates {
		return out, sessions.ErrSearchBound
	}
	for _, c := range candidates {
		msgs, _, err := x.messages(ctx, tx, c.s.Key, c.id-1, 1, sessions.PageBudget, true)
		if err != nil {
			return out, err
		}
		if len(msgs) == 1 {
			out.Hits = append(out.Hits, sessions.MatchMessage(q, c.s, msgs[0])...)
			if !q.BySession && len(out.Hits) > q.Limit {
				break
			}
		}
	}
	if len(candidates) > sessions.SearchCandidates && len(out.Hits) <= q.Limit {
		return sessions.LocalSearchResult{}, sessions.ErrSearchBound
	}
	return sessions.FinishSearch(out, q), nil
}

package sessions

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SearchQuery contains literal terms, never FTS syntax. Since/Until are message
// timestamps (milliseconds); messages without a timestamp use session UpdatedAt.
type SearchQuery struct {
	LocalSessions                              []Session        // service metadata snapshot avoids reopening ACP databases for search listing
	AllowedKeys                                map[string]int64 // local feed snapshot; nil means unrestricted
	Terms                                      []string
	File, Op, Tool, Kind, Agent, Project, Node string
	Failed, Errors, BySession                  bool
	Since, Until                               int64
	Limit                                      int
}

const SearchCandidates = 10000

var ErrSearchBound = errors.New("search exceeded its work or time limit; narrow the project, file or time range")

func (q *SearchQuery) Validate() error {
	q.File = NormalizePath(q.File)
	if q.Errors && q.FileFilter() {
		q.Failed = true
	}
	if len(q.Terms) > 32 {
		return errors.New("search accepts at most 32 terms; narrow the query")
	}
	size := 0
	for _, t := range q.Terms {
		size += len(t)
		if t == "" || strings.ContainsRune(t, 0) {
			return errors.New("search terms must be nonempty text")
		}
	}
	if size > 4096 {
		return errors.New("search terms exceed 4096 bytes")
	}
	if q.Op != "" && q.Op != "change" && q.Op != "delete" && q.Op != "read" && q.Op != "any" {
		return errors.New("op must be change, delete, read or any")
	}
	if q.Since < 0 || q.Until < 0 || (q.Until > 0 && q.Since > q.Until) {
		return errors.New("invalid search time range")
	}
	if q.Limit < 1 {
		q.Limit = 50
	}
	q.Limit = min(q.Limit, MaxPage)
	return nil
}

func ParseSearch(v url.Values) (SearchQuery, error) {
	q := SearchQuery{Terms: v["q"], File: v.Get("file"), Op: v.Get("op"), Tool: v.Get("tool"), Kind: v.Get("kind"), Agent: v.Get("agent"), Project: v.Get("project"), Node: v.Get("node"), Limit: limitParam(v.Get("limit"), 50)}
	for name, dst := range map[string]*int64{"since": &q.Since, "until": &q.Until} {
		if v.Has(name) {
			n, e := strconv.ParseInt(v.Get(name), 10, 64)
			if e != nil {
				return q, fmt.Errorf("%s must be Unix milliseconds", name)
			}
			*dst = n
		}
	}
	for name, dst := range map[string]*bool{"failed": &q.Failed, "error": &q.Errors, "by_session": &q.BySession} {
		if v.Has(name) {
			b, e := strconv.ParseBool(v.Get(name))
			if e != nil {
				return q, fmt.Errorf("%s must be boolean", name)
			}
			*dst = b
		}
	}
	return q, q.Validate()
}
func (q SearchQuery) Values() url.Values {
	v := url.Values{}
	if len(q.Terms) > 0 {
		v["q"] = q.Terms
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	for k, x := range map[string]string{"file": q.File, "op": q.Op, "tool": q.Tool, "kind": q.Kind, "agent": q.Agent, "project": q.Project} {
		if x != "" {
			v.Set(k, x)
		}
	}
	for k, x := range map[string]int64{"since": q.Since, "until": q.Until} {
		if x != 0 {
			v.Set(k, strconv.FormatInt(x, 10))
		}
	}
	if q.Failed {
		v.Set("failed", "true")
	}
	if q.Errors {
		v.Set("error", "true")
	}
	if q.BySession {
		v.Set("by_session", "true")
	}
	return v
}

// FTS phrase quoting is the only way terms enter MATCH.
func FTSPhrase(terms []string) string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(out, " AND ")
}
func CJKPair(t string) bool {
	r := []rune(t)
	if len(r) != 2 {
		return false
	}
	for _, c := range r {
		if !CJKRune(c) {
			return false
		}
	}
	return true
}

// CJKRune follows Wake cjk_format=1, including its alphabetic-only ranges.
func CJKRune(c rune) bool {
	return unicode.IsLetter(c) && ((c >= 0x3040 && c <= 0x30ff) || (c >= 0x3400 && c <= 0x4dbf) || (c >= 0x4e00 && c <= 0x9fff) || (c >= 0xac00 && c <= 0xd7af) || (c >= 0xf900 && c <= 0xfaff) || (c >= 0x20000 && c <= 0x323af))
}
func NormalizePath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	if len(p) > 1 && p[1] == ':' {
		p = strings.ToLower(p[:1]) + p[1:]
	}
	return p
}
func FileMatch(path, query string) bool {
	path, query = NormalizePath(path), NormalizePath(query)
	absolute := strings.HasPrefix(query, "/") || (len(query) > 1 && query[1] == ':')
	return path == query || (!absolute && strings.HasSuffix(path, "/"+query))
}

// Like escapes a literal substring; it is shared by the two SQL readers.
func Like(t string) string { return "%" + EscapeLike(t) + "%" }
func EscapeLike(t string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(t)
}

// FileSQL uses separator-aligned suffixes. Only the Windows drive letter folds.
func FileSQL(column, path string) (string, []any) {
	if path == "" {
		return "1", nil
	}
	path = NormalizePath(path)
	norm := "replace(" + column + ",char(92),'/')"
	norm = "(CASE WHEN substr(" + norm + ",2,1)=':' THEN lower(substr(" + norm + ",1,1))||substr(" + norm + ",2) ELSE " + norm + " END)"
	if strings.HasPrefix(path, "/") || (len(path) > 1 && path[1] == ':') {
		return norm + "=?", []any{path}
	}
	return "(" + norm + "=? OR substr(" + norm + ",-(length(?)+1))='/'||?)", []any{path, path, path}
}
func (q SearchQuery) CallFilter() bool { return q.FileFilter() || q.Tool != "" || q.Errors }
func (q SearchQuery) OpSQL(column string) (string, []any) {
	if q.Op == "any" {
		return "1", nil
	}
	if q.Op == "" {
		return column + " IN ('change','delete')", nil
	}
	return column + "=?", []any{q.Op}
}

const searchTimeNote = "search time budget exhausted; results are incomplete; add --project or a narrower --since/--until range"
const searchCountNote = "search candidate limit reached; results are incomplete; add --project or a narrower --since/--until range"

func (r *LocalSearchResult) Bound(through int64, timedOut bool) {
	r.Partial = true
	note := searchCountNote
	if timedOut {
		note = searchTimeNote
	}
	if through > 0 {
		note = "messages examined through " + time.UnixMilli(through).UTC().Format(time.RFC3339Nano) + "; " + note
	}
	r.Note = JoinNote(r.Note, note)
}
func (r *LocalSearchResult) SideCoverage(since int64) {
	r.ScannedSince = since
	t := time.UnixMilli(since).UTC()
	note := "thinking and full tool inputs searched since " + t.Format("2006-01-02 15:04:05 UTC") + "; add --since " + t.Format(time.RFC3339Nano) + " to narrow this scan"
	if !r.Partial {
		note = "message text searched completely; " + note
	}
	r.Note = JoinNote(r.Note, note)
}
func (r *LocalSearchResult) Missing(count int, reason string) {
	if count == 0 {
		return
	}
	r.Partial = true
	r.Note = JoinNote(r.Note, fmt.Sprintf("%s in %d matching session(s); refresh the source/copies or narrow --agent, --project, --since or --until", reason, count))
}
func (q SearchQuery) FileFilter() bool { return q.File != "" || q.Op != "" }
func JoinNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || strings.Contains(a, b) {
		return a
	}
	return a + "; " + b
}
func SearchInterrupted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrSearchBound)
}
func (q SearchQuery) SessionMatch(s Session) bool {
	if q.AllowedKeys != nil {
		if _, ok := q.AllowedKeys[s.Key]; !ok {
			return false
		}
	}
	return (q.Agent == "" || s.Agent == q.Agent) && (q.Project == "" || strings.Contains(strings.ToLower(s.Project+"\n"+s.ProjectPath), strings.ToLower(q.Project)))
}
func (q SearchQuery) TimeMatch(m Message, s Session) bool {
	t := m.Time
	if t == 0 {
		t = s.UpdatedAt
	}
	return (q.Since == 0 || t >= q.Since) && (q.Until == 0 || t <= q.Until)
}
func (q SearchQuery) NeedsStructure() bool {
	return q.FileFilter() || q.Tool != "" || q.Kind != "" || q.Errors
}
func (q SearchQuery) textMatch(text string) bool {
	text = strings.ToLower(text)
	for _, t := range q.Terms {
		if !strings.Contains(text, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

// SearchHit always names a message; Call is the zero-based call index when set.
type SearchHit struct {
	Agent       string `json:"agent,omitempty"`
	ProjectPath string `json:"project_path,omitempty"`
	Ref         string `json:"ref"`
	Key         string `json:"key"`
	Node        string `json:"node"`
	NodeID      string `json:"node_id"`
	ID          int64  `json:"id"`
	Rev         int64  `json:"rev"`
	Call        *int   `json:"call,omitempty"`
	Time        int64  `json:"time"`
	Kind        string `json:"kind,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Snippet     string `json:"snippet"`
	Files       []File `json:"files,omitempty"`
	Title       string `json:"title"`
	Project     string `json:"project"`
	UpdatedAt   int64  `json:"updated_at"`
}
type SearchSession struct {
	Key       string `json:"key"`
	Ref       string `json:"ref"`
	Node      string `json:"node"`
	NodeID    string `json:"node_id"`
	Title     string `json:"title"`
	Project   string `json:"project"`
	Rev       int64  `json:"rev"`
	Hits      int    `json:"hits"`
	First     int64  `json:"first"`
	Last      int64  `json:"last"`
	UpdatedAt int64  `json:"updated_at"`
}
type SearchNode struct {
	ScannedSince int64  `json:"scanned_since,omitempty"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Searched     string `json:"searched"`
	Partial      bool   `json:"partial,omitempty"`
	Note         string `json:"note,omitempty"`
}
type SearchResult struct {
	Hits     []SearchHit     `json:"hits"`
	Sessions []SearchSession `json:"sessions,omitempty"`
	Nodes    []SearchNode    `json:"nodes"`
	More     bool            `json:"more"`
}
type LocalSearchResult struct {
	ScannedSince int64           `json:"scanned_since,omitempty"`
	Sessions     []SearchSession `json:"sessions,omitempty"`
	Hits         []SearchHit     `json:"hits"`
	Partial      bool            `json:"partial,omitempty"`
	More         bool            `json:"more"`
	Note         string          `json:"note,omitempty"`
}
type SearchSource interface {
	Search(context.Context, SearchQuery) (LocalSearchResult, error)
}

// Snippet windows around the first literal match, in characters rather than bytes.
func Snippet(text string, terms []string) string {
	r := []rune(text)
	start := 0
	lower := strings.ToLower(text)
	for _, t := range terms {
		if i := strings.Index(lower, strings.ToLower(t)); i >= 0 {
			start = max(0, utf8.RuneCountInString(lower[:i])-80)
			break
		}
	}
	return string(r[start:min(len(r), start+300)])
}
func MatchMessage(q SearchQuery, s Session, m Message) []SearchHit {
	if !q.TimeMatch(m, s) || (q.Kind != "" && m.Kind != q.Kind) {
		return nil
	}
	base := SearchHit{Agent: s.Agent, ProjectPath: s.ProjectPath, Key: s.Key, ID: m.ID, Time: m.Time, Kind: m.Kind, Title: s.Title, Project: s.Project, UpdatedAt: s.UpdatedAt}
	body := q.textMatch(m.Text)
	thinking := m.Thinking != "" && q.textMatch(m.Thinking)
	callFilter := q.CallFilter()
	var out []SearchHit
	if !callFilter && (body || thinking) {
		h := base
		text := m.Text
		if !body {
			text = m.Thinking
		}
		h.Snippet = Snippet(text, q.Terms)
		out = append(out, h)
	}
	for i, c := range m.Calls {
		if q.Tool != "" && c.Tool != q.Tool || q.Errors && !c.Error {
			continue
		}
		if q.FileFilter() {
			if c.Error && !q.Failed && !q.Errors {
				continue
			}
			found := false
			for _, f := range c.Files {
				op := q.Op
				ok := op == "any" || op == f.Op || (op == "" && (f.Op == "change" || f.Op == "delete"))
				if ok && (q.File == "" || FileMatch(f.Path, q.File)) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		input := q.textMatch(c.Input)
		if !(input || (callFilter && (body || thinking))) {
			continue
		}
		// With no call filter an empty query lists messages, not every call too.
		if !callFilter && len(q.Terms) == 0 {
			continue
		}
		h := base
		h.Call = &i
		h.Tool = c.Tool
		h.Files = c.Files
		text := c.Input
		if !input {
			text = m.Text
			if !body {
				text = m.Thinking
			}
		}
		h.Snippet = Snippet(text, q.Terms)
		out = append(out, h)
	}
	return out
}
func SortHits(h []SearchHit) {
	slices.SortFunc(h, func(a, b SearchHit) int {
		at, bt := a.Time, b.Time
		if at == 0 {
			at = a.UpdatedAt
		}
		if bt == 0 {
			bt = b.UpdatedAt
		}
		if x := cmp.Compare(bt, at); x != 0 {
			return x
		}
		if x := cmp.Compare(a.NodeID, b.NodeID); x != 0 {
			return x
		}
		if x := cmp.Compare(a.Key, b.Key); x != 0 {
			return x
		}
		if x := cmp.Compare(a.ID, b.ID); x != 0 {
			return x
		}
		ai, bi := -1, -1
		if a.Call != nil {
			ai = *a.Call
		}
		if b.Call != nil {
			bi = *b.Call
		}
		return cmp.Compare(ai, bi)
	})
}
func FinishSearch(r LocalSearchResult, q SearchQuery) LocalSearchResult {
	if q.BySession {
		if len(r.Hits) > 0 {
			r.Sessions = append(r.Sessions, groupHits(r.Hits)...)
		}
		merged := make([]SearchSession, 0, len(r.Sessions))
		positions := map[string]int{}
		for _, g := range r.Sessions {
			key := g.NodeID + "/" + g.Key
			if i, ok := positions[key]; ok {
				v := &merged[i]
				v.Hits += g.Hits
				v.First = min(v.First, g.First)
				v.Last = max(v.Last, g.Last)
			} else {
				positions[key] = len(merged)
				merged = append(merged, g)
			}
		}
		r.Sessions = merged
		slices.SortFunc(r.Sessions, func(a, b SearchSession) int {
			return cmp.Or(cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.Key, b.Key))
		})
		if len(r.Sessions) > q.Limit {
			r.Sessions = r.Sessions[:q.Limit]
			r.More = true
		}
		if n := fit(r.Sessions); n < len(r.Sessions) {
			r.Sessions = r.Sessions[:n]
			r.More = true
		}
		r.Hits = []SearchHit{}
		return r
	}
	SortHits(r.Hits)
	if len(r.Hits) > q.Limit {
		r.More = true
		r.Hits = r.Hits[:q.Limit]
	}
	if n := fit(r.Hits); n < len(r.Hits) {
		r.More = true
		r.Hits = r.Hits[:n]
	}
	if r.Hits == nil {
		r.Hits = []SearchHit{}
	}
	return r
}

// ScanSearch scans recent ACP sessions without storing local message rows.
func ScanSearch(ctx context.Context, source Source, q SearchQuery) (out LocalSearchResult, err error) {
	var through int64
	missing := map[string]bool{}
	defer func() {
		if SearchInterrupted(ctx, err) {
			out.Bound(through, !errors.Is(err, ErrSearchBound))
			err = nil
		}
		out.Missing(len(missing), "message structure unavailable")
		out = FinishSearch(out, q)
	}()
	list := q.LocalSessions
	if list == nil {
		list, err = source.Sessions(ctx)
		if err != nil {
			return out, err
		}
	}
	list = slices.Clone(list)
	slices.SortFunc(list, func(a, b Session) int {
		return cmp.Or(cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.Key, b.Key))
	})
	count := 0
	for _, s := range list {
		if !q.SessionMatch(s) || s.Messages == 0 || (q.Since > 0 && s.UpdatedAt < q.Since) || (q.Until > 0 && s.CreatedAt > q.Until) {
			continue
		}
		var after int64
		for {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			msgs, more, e := source.Messages(ctx, s.Key, after, MaxPage, PageBudget)
			err = e
			for _, m := range msgs {
				count++
				if count > SearchCandidates {
					return out, ErrSearchBound
				}
				through = m.Time
				if through == 0 {
					through = s.UpdatedAt
				}
				level := m.Structure
				if level == 0 && m.Kind != "" {
					level = s.Structure
				}
				if q.TimeMatch(m, s) && (!q.CallFilter() || m.Role != "user") && ((m.Kind == "" && q.NeedsStructure()) || (level < 2 && q.FileFilter())) {
					missing[s.Key] = true
				}
				out.Hits = append(out.Hits, MatchMessage(q, s, m)...)
				after = m.ID
			}
			if err != nil {
				return out, err
			}
			if !more || len(msgs) == 0 {
				break
			}
		}
	}
	return out, nil
}

func searchSource(ctx context.Context, src Source, q SearchQuery) (LocalSearchResult, error) {
	if x, ok := src.(SearchSource); ok {
		return x.Search(ctx, q)
	}
	return ScanSearch(ctx, src, q)
}

func (c *Combined) Search(ctx context.Context, q SearchQuery) (LocalSearchResult, error) {
	type result struct {
		r LocalSearchResult
		e error
	}
	ch := make(chan result, 2)
	count := 0
	for i, src := range []Source{c.Wake, c.ACP} {
		if src == nil {
			continue
		}
		count++
		go func(i int, src Source) {
			sourceQuery := q
			if i == 1 && q.LocalSessions != nil {
				sourceQuery.LocalSessions = []Session{}
				for _, sess := range q.LocalSessions {
					if strings.HasPrefix(sess.Key, "antigravity-acp:") {
						sourceQuery.LocalSessions = append(sourceQuery.LocalSessions, sess)
					}
				}
			}
			sc, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			r, e := searchSource(sc, src, sourceQuery)
			ch <- result{r, e}
		}(i, src)
	}
	out := LocalSearchResult{}
	available := false
	for range count {
		v := <-ch
		if errors.Is(v.e, ErrNoIndex) {
			continue
		}
		available = true
		if v.e != nil {
			if SearchInterrupted(ctx, v.e) {
				v.r.Bound(0, !errors.Is(v.e, ErrSearchBound))
			} else {
				v.r.Partial = true
				v.r.Note = JoinNote(v.r.Note, "local source unavailable; narrow the query or retry: "+v.e.Error())
			}
		}
		out.Hits = append(out.Hits, v.r.Hits...)
		out.Sessions = append(out.Sessions, v.r.Sessions...)
		out.Partial = out.Partial || v.r.Partial
		out.More = out.More || v.r.More
		out.ScannedSince = max(out.ScannedSince, v.r.ScannedSince)
		out.Note = JoinNote(out.Note, v.r.Note)
	}
	if !available {
		return out, ErrNoIndex
	}
	return FinishSearch(out, q), nil
}

func groupHits(hits []SearchHit) []SearchSession {
	var out []SearchSession
	groups := map[string]int{}
	for _, h := range hits {
		key := h.NodeID + "/" + h.Key
		i, ok := groups[key]
		if !ok {
			i = len(out)
			groups[key] = i
			out = append(out, SearchSession{Key: h.Key, Ref: h.Ref, Node: h.Node, NodeID: h.NodeID, Title: h.Title, Project: h.Project, Rev: h.Rev, First: h.ID, Last: h.ID, UpdatedAt: h.UpdatedAt})
		}
		g := &out[i]
		g.Hits++
		g.First = min(g.First, h.ID)
		g.Last = max(g.Last, h.ID)
	}
	return out
}

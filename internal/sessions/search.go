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
	"unicode"
	"unicode/utf8"
)

// SearchQuery contains literal terms, never FTS syntax. Since/Until are message
// timestamps (milliseconds); messages without a timestamp use session UpdatedAt.
type SearchQuery struct {
	ExcludeKeys                                []string // Combined suppresses native ACP duplicates before limiting Wake hits.
	Terms                                      []string
	File, Op, Tool, Kind, Agent, Project, Node string
	Failed, Errors, BySession                  bool
	Since, Until                               int64
	Limit                                      int
}

const SearchCandidates = 10000

var ErrSearchBound = errors.New("search exceeded its work or time limit; narrow the project, file or time range")

func (q *SearchQuery) Validate() error {
	q.File = strings.TrimPrefix(strings.ReplaceAll(q.File, `\`, "/"), "./")
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
	v := url.Values{"q": q.Terms, "limit": {strconv.Itoa(q.Limit)}}
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
func LikeLiteral(t string) string { return like(t) }
func FileMatch(path, query string) bool {
	path = strings.ReplaceAll(path, `\`, "/")
	query = strings.ReplaceAll(query, `\`, "/")
	absolute := strings.HasPrefix(query, "/") || (len(query) > 1 && query[1] == ':')
	return path == query || (!absolute && strings.HasSuffix(path, "/"+strings.TrimPrefix(query, "./")))
}
func (q SearchQuery) SessionMatch(s Session) bool {
	return !slices.Contains(q.ExcludeKeys, s.Key) && (q.Agent == "" || s.Agent == q.Agent) && (q.Project == "" || strings.Contains(strings.ToLower(s.Project+"\n"+s.ProjectPath), strings.ToLower(q.Project)))
}
func (q SearchQuery) TimeMatch(m Message, s Session) bool {
	t := m.Time
	if t == 0 {
		t = s.UpdatedAt
	}
	return (q.Since == 0 || t >= q.Since) && (q.Until == 0 || t <= q.Until)
}
func (q SearchQuery) NeedsStructure() bool {
	return q.File != "" || q.Tool != "" || q.Kind != "" || q.Errors
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
	Ref       string `json:"ref"`
	Key       string `json:"key"`
	Node      string `json:"node"`
	NodeID    string `json:"node_id"`
	ID        int64  `json:"id"`
	Rev       int64  `json:"rev"`
	Call      *int   `json:"call,omitempty"`
	Time      int64  `json:"time"`
	Kind      string `json:"kind,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Snippet   string `json:"snippet"`
	Files     []File `json:"files,omitempty"`
	Title     string `json:"title"`
	Project   string `json:"project"`
	UpdatedAt int64  `json:"updated_at"`
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Searched string `json:"searched"`
	Partial  bool   `json:"partial,omitempty"`
	Note     string `json:"note,omitempty"`
}
type SearchResult struct {
	Hits     []SearchHit     `json:"hits"`
	Sessions []SearchSession `json:"sessions,omitempty"`
	Nodes    []SearchNode    `json:"nodes"`
	More     bool            `json:"more"`
}
type LocalSearchResult struct {
	Sessions []SearchSession `json:"sessions,omitempty"`
	Hits     []SearchHit     `json:"hits"`
	Partial  bool            `json:"partial,omitempty"`
	More     bool            `json:"more"`
	Note     string          `json:"note,omitempty"`
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
	base := SearchHit{Key: s.Key, ID: m.ID, Time: m.Time, Kind: m.Kind, Title: s.Title, Project: s.Project, UpdatedAt: s.UpdatedAt}
	body := q.textMatch(m.Text)
	thinking := m.Thinking != "" && q.textMatch(m.Thinking)
	callFilter := q.File != "" || q.Tool != "" || q.Errors
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
		if q.File != "" {
			if c.Error && !q.Failed {
				continue
			}
			found := false
			for _, f := range c.Files {
				op := q.Op
				ok := op == "any" || op == f.Op || (op == "" && (f.Op == "change" || f.Op == "delete"))
				if ok && FileMatch(f.Path, q.File) {
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

// ScanSearch is the ACP path: no persisted local messages, bounded traversal.
func ScanSearch(ctx context.Context, source Source, q SearchQuery) (LocalSearchResult, error) {
	list, err := source.Sessions(ctx)
	if err != nil {
		return LocalSearchResult{}, err
	}
	out := LocalSearchResult{}
	count := 0
	for _, s := range list {
		if !q.SessionMatch(s) {
			continue
		}
		if (s.Structure < 1 && q.NeedsStructure()) || (s.Structure < 2 && q.File != "") {
			out.Partial = true
		}
		var after int64
		for {
			if err := ctx.Err(); err != nil {
				return out, ErrSearchBound
			}
			msgs, more, err := source.Messages(ctx, s.Key, after, MaxPage, PageBudget)
			if err != nil {
				return out, err
			}
			for _, m := range msgs {
				count++
				if count > SearchCandidates {
					return out, ErrSearchBound
				}
				out.Hits = append(out.Hits, MatchMessage(q, s, m)...)
				after = m.ID
			}
			if !more || len(msgs) == 0 {
				break
			}
		}
	}
	return FinishSearch(out, q), nil
}

func searchSource(ctx context.Context, src Source, q SearchQuery) (LocalSearchResult, error) {
	if x, ok := src.(SearchSource); ok {
		return x.Search(ctx, q)
	}
	return ScanSearch(ctx, src, q)
}

func (c *Combined) Search(ctx context.Context, q SearchQuery) (LocalSearchResult, error) {
	out := LocalSearchResult{}
	available := false
	native := map[string]bool{}
	// ACP's identities suppress Wake duplicates even when their text did not match.
	if c.ACP != nil {
		list, e := c.ACP.Sessions(ctx)
		if e == nil {
			for _, s := range list {
				native[strings.TrimPrefix(s.Key, "antigravity-acp:")] = true
			}
		}
	}
	for sourceIndex, src := range []Source{c.Wake, c.ACP} {
		if src == nil {
			continue
		}
		sourceQuery := q
		if sourceIndex == 0 {
			for id := range native {
				sourceQuery.ExcludeKeys = append(sourceQuery.ExcludeKeys, "antigravity:"+id)
			}
		}
		r, e := searchSource(ctx, src, sourceQuery)
		if errors.Is(e, ErrNoIndex) {
			continue
		}
		if e == nil {
			available = true
		}
		if e != nil {
			out.Partial = true
			out.Note = "a local source could not complete search: " + e.Error()
			r = LocalSearchResult{}
		}
		out.Partial = out.Partial || r.Partial
		out.More = out.More || r.More
		for _, g := range r.Sessions {
			id, ok := strings.CutPrefix(g.Key, "antigravity:")
			if ok && native[id] {
				continue
			}
			out.Sessions = append(out.Sessions, g)
		}
		for _, h := range r.Hits {
			id, ok := strings.CutPrefix(h.Key, "antigravity:")
			if ok && native[id] {
				continue
			}
			out.Hits = append(out.Hits, h)
		}
	}
	if !available && !out.Partial {
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

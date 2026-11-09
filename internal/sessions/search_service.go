package sessions

import (
	"cmp"
	"context"
	"slices"
	"time"
)

func (s *Service) LocalSearch(ctx context.Context, q SearchQuery) (out LocalSearchResult, err error) {
	defer func() {
		if err != nil && SearchInterrupted(ctx, err) {
			out.Bound(0)
			out = FinishSearch(out, q)
			err = nil
		}
	}()
	if s.Source == nil {
		return LocalSearchResult{}, ErrNoIndex
	}
	// Snapshot the published key/revision set before limiting either source.
	rows, err := s.Store.db.QueryContext(ctx, "SELECT "+sessionColumns+",rev FROM sessions WHERE node=? AND deleted=0", s.Self.ID)
	if err != nil {
		return LocalSearchResult{}, err
	}
	q.AllowedKeys = map[string]int64{}
	q.LocalSessions = []Session{}
	for rows.Next() {
		var rev int64
		sess, e := scanSession(rows.Scan, &rev)
		if e != nil {
			err = e
			break
		}
		q.AllowedKeys[sess.Key] = rev
		q.LocalSessions = append(q.LocalSessions, sess)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return LocalSearchResult{}, err
	}
	if re != nil {
		return LocalSearchResult{}, re
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	r, err := searchSource(ctx, s.Source, q)
	if err != nil {
		if SearchInterrupted(ctx, err) {
			r.Bound(0)
			err = nil
		} else {
			return r, err
		}
	}
	for i := range r.Hits {
		h := &r.Hits[i]
		h.Rev = q.AllowedKeys[h.Key]
		h.NodeID = s.Self.ID
		h.Node = s.Self.Name
		h.Ref = s.Self.ID + "/" + h.Key
	}
	for i := range r.Sessions {
		g := &r.Sessions[i]
		g.Rev = q.AllowedKeys[g.Key]
		g.NodeID = s.Self.ID
		g.Node = s.Self.Name
		g.Ref = s.Self.ID + "/" + g.Key
	}
	return FinishSearch(r, q), nil
}

// Search takes a validated query, fans out in parallel, then searches held copies.
// PeerSearchContext is injectable so tests can expire deadlines without sleeping.
func (s *Service) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := s.nodes(ctx)
	if err != nil {
		if SearchInterrupted(ctx, err) {
			r := LocalSearchResult{}
			r.Bound(0)
			return SearchResult{Hits: []SearchHit{}, Nodes: []SearchNode{{ID: s.Self.ID, Name: s.Self.Name, Searched: "none", Partial: true, Note: "could not read the node list; " + r.Note}}}, nil
		}
		return SearchResult{}, err
	}

	if q.Node != "" {
		n, e := resolve(nodes, q.Node)
		if e != nil {
			return SearchResult{}, e
		}
		q.Node = n.ID
	}
	peers := map[string]Peer{}
	for _, p := range s.Peers() {
		peers[p.ID] = p
	}
	type answer struct {
		node SearchNode
		r    LocalSearchResult
	}
	answers := make(chan answer, len(nodes))
	for _, n := range nodes {
		if q.Node != "" && n.ID != q.Node {
			continue
		}
		go func(n *Node) {
			a := answer{node: SearchNode{ID: n.ID, Name: n.Name, Searched: "none"}}
			if n.Self {
				r, e := s.LocalSearch(ctx, q)
				a.r = r
				if e == nil {
					a.node.Searched = "live"
				} else {
					a.node.Partial = true
					a.node.Note = e.Error()
				}
			} else {
				copyCtx, stopCopy := context.WithTimeout(ctx, 8*time.Second)
				defer stopCopy()
				type copyAnswer struct {
					r    LocalSearchResult
					held bool
					err  error
				}
				copies := make(chan copyAnswer, 1)
				go func() { r, held, e := s.Store.SearchCopies(copyCtx, n.ID, q); copies <- copyAnswer{r, held, e} }()
				p := peers[n.ID]
				live := false
				if p.Online && p.Addr != "" && p.Protocol == s.Protocol {
					timeout := s.PeerSearchContext
					if timeout == nil {
						timeout = context.WithTimeout
					}
					pc, stop := timeout(ctx, 5*time.Second)
					e := s.Fetch(pc, p, "/v1/sessions/search?"+q.Values().Encode(), &a.r)
					stop()
					live = e == nil && len(a.r.Hits) <= MaxPage && len(a.r.Sessions) <= MaxPage
					if live {
						a.node.Searched = "live"
					} else {
						a.node.Note = "peer search unavailable"
					}
				}
				if !live {
					copied := <-copies
					r, held, e := copied.r, copied.held, copied.err
					a.r = r
					if held {
						a.node.Searched = "copies"
						a.node.Note = JoinNote(a.node.Note, "searched held copies")
					}
					if e != nil {
						a.node.Partial = true
						a.node.Note = e.Error()
						if SearchInterrupted(ctx, e) {
							a.r.Bound(0)
						}
					}
				}
			}
			a.node.Partial = a.node.Partial || a.r.Partial
			if a.r.Note != "" {
				a.node.Note = a.r.Note
			}
			for i := range a.r.Hits {
				h := &a.r.Hits[i]
				h.NodeID = n.ID
				h.Node = n.Name
				h.Ref = ref(n, nodes, h.Key)
				h.Snippet = Snippet(h.Snippet, nil)
			}
			for i := range a.r.Sessions {
				g := &a.r.Sessions[i]
				g.NodeID = n.ID
				g.Node = n.Name
				g.Ref = ref(n, nodes, g.Key)
			}
			answers <- a
		}(n)
	}
	out := SearchResult{Hits: []SearchHit{}, Nodes: []SearchNode{}}
	pending := map[string]*Node{}
	for _, n := range nodes {
		if q.Node == "" || q.Node == n.ID {
			pending[n.ID] = n
		}
	}
	for len(pending) > 0 {
		select {
		case a := <-answers:
			delete(pending, a.node.ID)
			out.Nodes = append(out.Nodes, a.node)
			out.Hits = append(out.Hits, a.r.Hits...)
			out.Sessions = append(out.Sessions, a.r.Sessions...)
			out.More = out.More || a.r.More
		case <-ctx.Done():
			for _, n := range pending {
				out.Nodes = append(out.Nodes, SearchNode{ID: n.ID, Name: n.Name, Searched: "none", Partial: true, Note: "search time budget reached; results are incomplete; narrow the project, file or time range"})
			}
			pending = nil
		}
	}
	SortHits(out.Hits)
	if len(out.Hits) > q.Limit {
		out.Hits = out.Hits[:q.Limit]
		out.More = true
	}
	if n := fit(out.Hits); n < len(out.Hits) {
		out.Hits = out.Hits[:n]
		out.More = true
	}
	slices.SortFunc(out.Nodes, func(a, b SearchNode) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	if q.BySession {
		slices.SortFunc(out.Sessions, func(a, b SearchSession) int {
			return cmp.Or(cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.NodeID, b.NodeID), cmp.Compare(a.Key, b.Key))
		})
		if len(out.Sessions) > q.Limit {
			out.Sessions = out.Sessions[:q.Limit]
			out.More = true
		}
		if n := fit(out.Sessions); n < len(out.Sessions) {
			out.Sessions = out.Sessions[:n]
			out.More = true
		}
	}

	return out, nil
}

// Around walks bounded pages, retaining only the requested preceding window.
// ID is a cursor, not an array index: Wake IDs may contain large gaps.
func (s *Service) Around(ctx context.Context, node, key string, id int64, before, after int, rev *int64) (Reading, error) {
	window := []Message{}
	tail := []Message{}
	var cursor int64
	var firstID int64
	wantRev := int64(0)
	if rev != nil {
		wantRev = *rev
	}
	found, moved := false, false
	remaining := after
	var firstRev int64
	out := Reading{Messages: []Message{}}
	finish := func(partial bool) (Reading, error) {
		if !found && len(window) == 0 {
			window = tail
		}
		out.Messages = window
		out.Partial = partial
		out.HasBefore = len(window) > 0 && firstID != 0 && window[0].ID != firstID
		out.Moved = moved || (!found && !partial) || (rev != nil && out.Rev != 0 && *rev != out.Rev)
		if partial {
			out.Note = JoinNote(out.Note, "anchor scan stopped at its budget; the returned window is partial; choose a nearer message id or read ordinary pages without --around")
		}
		return fitAround(out, id), nil
	}
	for scanned := 0; scanned < SearchCandidates; {
		pg, err := s.read(ctx, node, key, cursor, MaxPage, wantRev)
		if err != nil {
			if ctx.Err() != nil {
				return finish(true)
			}
			return out, err
		}
		out = pg
		if cursor == 0 {
			firstRev = pg.Rev
		} else if firstRev != pg.Rev {
			moved = true
		}
		for _, m := range pg.Messages {
			if scanned >= SearchCandidates {
				out.Next = cursor
				return finish(true)
			}
			scanned++
			if firstID == 0 {
				firstID = m.ID
			}
			cursor = m.ID
			tail = append(tail, m)
			if len(tail) > max(1, before+after+1) {
				tail = tail[1:]
			}
			if !found && m.ID < id {
				window = append(window, m)
				if len(window) > before {
					window = window[1:]
				}
				continue
			}
			if !found {
				found = true
				moved = moved || m.ID != id
				window = append(window, m)
				continue
			}
			if remaining == 0 {
				out.Next = window[len(window)-1].ID
				return finish(false)
			}
			window = append(window, m)
			remaining--
		}
		if pg.Next == 0 {
			out.Next = 0
			return finish(false)
		}
		if ctx.Err() != nil {
			return finish(true)
		}
	}
	return finish(true)
}

// Keep the anchor when the byte budget cannot hold the entire requested window.
func fitAround(r Reading, id int64) Reading {
	if r.Messages == nil {
		r.Messages = []Message{}
	}
	size := 0
	anchor := len(r.Messages) - 1
	for i, m := range r.Messages {
		size += MessageSize(m)
		if m.ID >= id && anchor == len(r.Messages)-1 {
			anchor = i
		}
	}
	for size > PageBudget && anchor > 0 {
		size -= MessageSize(r.Messages[0])
		r.Messages = r.Messages[1:]
		r.HasBefore = true
		anchor--
	}
	if n := fit(r.Messages); n < len(r.Messages) {
		r.Messages = r.Messages[:n]
		r.Next = r.Messages[n-1].ID
	}
	return r
}

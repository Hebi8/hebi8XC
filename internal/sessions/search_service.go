package sessions

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

func (s *Service) LocalSearch(ctx context.Context, q SearchQuery) (LocalSearchResult, error) {
	if s.Source == nil {
		return LocalSearchResult{}, ErrNoIndex
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	r, err := searchSource(ctx, s.Source, q)
	if err != nil {
		return LocalSearchResult{}, err
	}
	// A search never asks a peer or reads copies through this route.
	hits := r.Hits[:0]
	for _, h := range r.Hits {
		_, rev, ok, e := s.Store.Lookup(parent, s.Self.ID, h.Key)
		if e != nil {
			return LocalSearchResult{}, e
		}
		if !ok {
			r.Partial = true
			continue
		}
		h.Rev = rev
		h.NodeID = s.Self.ID
		h.Node = s.Self.Name
		h.Ref = s.Self.ID + "/" + h.Key
		hits = append(hits, h)
	}
	r.Hits = hits
	groups := r.Sessions[:0]
	for _, g := range r.Sessions {
		_, rev, ok, e := s.Store.Lookup(parent, s.Self.ID, g.Key)
		if e != nil {
			return LocalSearchResult{}, e
		}
		if !ok {
			r.Partial = true
			continue
		}
		g.Rev = rev
		g.NodeID = s.Self.ID
		g.Node = s.Self.Name
		g.Ref = s.Self.ID + "/" + g.Key
		groups = append(groups, g)
	}
	r.Sessions = groups
	return r, nil
}

// Search takes a validated query, fans out in parallel, then searches held copies.
// PeerSearchContext is injectable so tests can expire deadlines without sleeping.
func (s *Service) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := s.nodes(ctx)
	if err != nil {
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
	var wg sync.WaitGroup
	for _, n := range nodes {
		if q.Node != "" && n.ID != q.Node {
			continue
		}
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()
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
						a.node.Note = "peer search unavailable; searched held copies"
					}
				}
				if !live {
					r, held, e := s.Store.SearchCopies(ctx, n.ID, q)
					a.r = r
					if held {
						a.node.Searched = "copies"
					}
					if e != nil {
						a.node.Partial = true
						a.node.Note = e.Error()
						a.r = LocalSearchResult{}
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
	wg.Wait()
	close(answers)
	out := SearchResult{Hits: []SearchHit{}, Nodes: []SearchNode{}}
	for a := range answers {
		out.Nodes = append(out.Nodes, a.node)
		out.Hits = append(out.Hits, a.r.Hits...)
		out.Sessions = append(out.Sessions, a.r.Sessions...)
		out.More = out.More || a.r.More
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
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var window []Message
	var cursor int64
	found := false
	moved := false
	var firstRev int64
	remaining := after
	var out Reading
	for scanned := 0; scanned < SearchCandidates; {
		pg, err := s.Read(ctx, node, key, cursor, MaxPage)
		if err != nil {
			return out, err
		}
		out = pg
		if cursor == 0 {
			firstRev = pg.Rev
		} else if firstRev != pg.Rev {
			moved = true
		}
		for _, m := range pg.Messages {
			scanned++
			cursor = m.ID
			if !found && m.ID < id {
				window = append(window, m)
				if len(window) > before {
					window = window[len(window)-before:]
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
				out.Messages = window
				out.Moved = moved || (rev != nil && *rev != out.Rev)
				return fitAround(out, id), nil
			}
			window = append(window, m)
			remaining--
		}
		if pg.Next == 0 {
			out.Messages = window
			out.Next = 0
			out.Moved = moved || !found || (rev != nil && *rev != out.Rev)
			return fitAround(out, id), nil
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
	}
	return out, errors.New("anchor exceeds the 10000-message read bound; use --after to narrow the read")
}

// Keep the anchor when the byte budget cannot hold the entire requested window.
func fitAround(r Reading, id int64) Reading {
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
		anchor--
	}
	if n := fit(r.Messages); n < len(r.Messages) {
		r.Messages = r.Messages[:n]
		r.Next = r.Messages[n-1].ID
	}
	return r
}

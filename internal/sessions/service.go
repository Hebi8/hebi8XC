package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Interval is the time between two rounds of the service: look at the
	// sessions of this machine, then read what is new in the feeds of the
	// peers that are online.
	Interval = time.Minute
	// refreshEvery is how often the index of this machine is brought up to
	// date before it is looked at.
	refreshEvery = 2 * time.Minute
)

// Peer is another node as the service needs to know it.
type Peer struct {
	ID       string
	Name     string
	Online   bool
	Addr     string // where it answered last; empty if it was never reached
	Protocol int    // the protocol version it speaks
	Version  string // the build it runs, if it said
}

// Service is the session module of a node.
type Service struct {
	Self  Peer // this node; ID, Name and Version are used
	Store *Store
	// Protocol is the protocol version this node speaks. Sessions are only
	// exchanged with peers that speak the same one.
	Protocol int
	// Source is the session index of this machine, nil if it has none.
	Source Source
	// Refresh brings the index up to date and returns what the tool that
	// does it had to say. Nil if there is no such tool.
	Refresh func(ctx context.Context) (string, error)
	// Peers lists the other nodes.
	Peers func() []Peer
	// Fetch gets path from the peer at addr and decodes the JSON answer
	// into out. It fails if the node that answers is not the peer.
	Fetch func(ctx context.Context, peer Peer, path string, out any) error
	Now   func() time.Time
	Log   *slog.Logger

	mu        sync.Mutex
	unshared  string // why the sessions of this machine are not shared; "" if they are
	refreshed time.Time
	said      map[string]string // last message logged per subject
	pulling   sync.Mutex        // one feed is read at a time
	copying   map[string]*copyLock
}

// copyLock makes the copies of one session take turns.
type copyLock struct {
	sync.Mutex
	waiting int
}

// lockCopy waits until no other copy of the session is under way and
// returns the function that lets the next one in.
func (s *Service) lockCopy(node, key string) (unlock func()) {
	name := node + "\x00" + key
	s.mu.Lock()
	if s.copying == nil {
		s.copying = make(map[string]*copyLock)
	}
	l := s.copying[name]
	if l == nil {
		l = new(copyLock)
		s.copying[name] = l
	}
	l.waiting++
	s.mu.Unlock()

	l.Lock()
	return func() {
		l.Unlock()
		s.mu.Lock()
		if l.waiting--; l.waiting == 0 {
			delete(s.copying, name)
		}
		s.mu.Unlock()
	}
}

// say logs a message about a subject unless it is the one logged last.
func (s *Service) say(level slog.Level, subject, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.said == nil {
		s.said = make(map[string]string)
	}
	if s.said[subject] == msg {
		return
	}
	s.said[subject] = msg
	if msg != "" {
		s.Log.Log(context.Background(), level, msg)
	}
}

// Round does what is due: SyncLocal, then Pull for every peer that is
// online and can be reached.
func (s *Service) Round(ctx context.Context) {
	s.SyncLocal(ctx)
	for _, p := range s.Peers() {
		if !p.Online || p.Addr == "" || p.Protocol != s.Protocol || ctx.Err() != nil {
			continue
		}
		if err := s.Pull(ctx, p); err != nil {
			s.say(slog.LevelWarn, "pull "+p.ID, fmt.Sprintf("sessions of %s not updated: %v", p.Name, err))
		} else {
			s.say(slog.LevelInfo, "pull "+p.ID, "")
		}
	}
}

// localNote returns why the sessions of this machine are not shared, or "".
func (s *Service) localNote() string {
	if s.Source == nil {
		return ErrNoIndex.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unshared
}

func (s *Service) setLocalNote(note string) {
	s.mu.Lock()
	s.unshared = note
	s.mu.Unlock()
}

// SyncLocal makes the feed of this machine match its index, after bringing
// the index up to date if that is due. Without an index it does nothing.
// A failure leaves the feed as it was: an index that cannot be read right
// now is no reason to tell the peers that every session is gone.
func (s *Service) SyncLocal(ctx context.Context) {
	if s.Source == nil {
		return
	}
	if s.Refresh != nil && s.dueRefresh() {
		if said, err := s.Refresh(ctx); err != nil {
			s.say(slog.LevelWarn, "refresh", "session index not refreshed: "+err.Error())
		} else {
			s.say(slog.LevelInfo, "refresh", "")
			s.Log.Debug("session index refreshed", "said", said)
		}
	}
	current, err := s.Source.Sessions(ctx)
	if err != nil {
		level := slog.LevelWarn
		if errors.Is(err, ErrNoIndex) {
			level = slog.LevelInfo // normal on a machine without the indexer
		}
		s.say(level, "source", "sessions of this machine are not shared: "+err.Error())
		s.setLocalNote(err.Error())
		return
	}
	changed, err := s.Store.SetLocal(ctx, s.Self.ID, current)
	if err != nil {
		s.say(slog.LevelWarn, "source", "session feed not updated: "+err.Error())
		s.setLocalNote(err.Error())
		return
	}
	s.setLocalNote("")
	s.say(slog.LevelInfo, "source", fmt.Sprintf("sharing the sessions of this machine (%d)", len(current)))
	if changed > 0 {
		s.Log.Debug("session feed updated", "sessions", len(current), "changed", changed)
	}
}

func (s *Service) dueRefresh() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if !s.refreshed.IsZero() && now.Sub(s.refreshed) < refreshEvery {
		return false
	}
	s.refreshed = now
	return true
}

// changesPage is the answer of GET /v1/sessions between nodes.
type changesPage struct {
	Epoch   string   `json:"epoch"`
	Changes []Change `json:"changes"`
	More    bool     `json:"more"`
}

// messagesPage is the answer of GET /v1/sessions/messages between nodes.
// Epoch and Rev say which feed, and which revision of the session in it,
// the messages belong to. Epoch was added later and may be missing.
type messagesPage struct {
	Epoch    string    `json:"epoch"`
	Rev      int64     `json:"rev"`
	Messages []Message `json:"messages"`
	More     bool      `json:"more"`
}

// fit returns how many of the items fit into PageBudget bytes of JSON, at
// least one.
func fit[T any](items []T) int {
	size := 0
	for n, item := range items {
		raw, _ := json.Marshal(item)
		size += len(raw) + 1
		if size > PageBudget && n > 0 {
			return n
		}
	}
	return len(items)
}

// page cuts items, which were read with one more than limit to see whether
// there are more, down to limit and to PageBudget.
func page[T any](items []T, limit int) ([]T, bool) {
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	if n := fit(items); n < len(items) {
		return items[:n], true
	}
	return items, more
}

// limitParam reads a page size, between 1 and MaxPage.
func limitParam(text string, fallback int) int {
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 {
		n = fallback
	}
	return min(n, MaxPage)
}

// LocalChanges answers a peer that reads this node's feed: the entries
// above revision after.
func (s *Service) LocalChanges(ctx context.Context, after int64, limit int) (changesPage, error) {
	epoch, err := s.Store.Epoch()
	if err != nil {
		return changesPage{}, err
	}
	changes, err := s.Store.Changes(ctx, s.Self.ID, after, limit+1)
	if err != nil {
		return changesPage{}, err
	}
	changes, more := page(changes, limit)
	if changes == nil {
		changes = []Change{}
	}
	return changesPage{Epoch: epoch, Changes: changes, More: more}, nil
}

// LocalMessages answers a peer that reads a session of this machine. Only
// sessions that are in the feed can be read.
func (s *Service) LocalMessages(ctx context.Context, key string, after int64, limit int) (messagesPage, error) {
	if s.Source == nil {
		return messagesPage{}, ErrNotFound
	}
	epoch, err := s.Store.Epoch()
	if err != nil {
		return messagesPage{}, err
	}
	_, rev, ok, err := s.Store.Lookup(ctx, s.Self.ID, key)
	if err != nil {
		return messagesPage{}, err
	}
	if !ok {
		return messagesPage{}, ErrNotFound
	}
	msgs, more, err := s.Source.Messages(ctx, key, after, limit, PageBudget)
	if err != nil {
		return messagesPage{}, err
	}
	if msgs == nil {
		msgs = []Message{}
	}
	return messagesPage{Epoch: epoch, Rev: rev, Messages: msgs, More: more}, nil
}

// maxPullPages bounds one Pull. A feed longer than that is continued in
// the next round.
const maxPullPages = 1000

// Pull reads what is new in the feed of a peer into the store.
func (s *Service) Pull(ctx context.Context, p Peer) error {
	s.pulling.Lock()
	defer s.pulling.Unlock()
	feed, err := s.Store.Feed(ctx, p.ID)
	if err != nil {
		return err
	}
	cursor, epoch := feed.Cursor, feed.Epoch
	for range maxPullPages {
		var pg changesPage
		path := "/v1/sessions?after=" + strconv.FormatInt(cursor, 10) + "&limit=" + strconv.Itoa(MaxPage)
		if err := s.Fetch(ctx, p, path, &pg); err != nil {
			return err
		}
		if pg.Epoch == "" || len(pg.Changes) > MaxPage {
			return errors.New("the answer is not a page of a session feed")
		}
		if pg.Epoch != epoch {
			// Another feed than the one on record: what was read so far
			// does not apply. ApplyRemote drops it; start over from 0.
			if cursor != 0 {
				cursor, epoch = 0, pg.Epoch
				if err := s.Store.ApplyRemote(ctx, p.ID, p.Name, epoch, nil, 0, false); err != nil {
					return err
				}
				continue
			}
			epoch = pg.Epoch
		}
		next := cursor
		for _, c := range pg.Changes {
			if c.Rev <= next {
				return errors.New("the feed does not go forward")
			}
			next = c.Rev
		}
		// An answer with nothing in it that promises more would never end.
		done := !pg.More || len(pg.Changes) == 0
		if err := s.Store.ApplyRemote(ctx, p.ID, p.Name, epoch, pg.Changes, next, done); err != nil {
			return err
		}
		cursor = next
		if done {
			return nil
		}
	}
	return nil
}

// Node is a node as a listing shows it.
type Node struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Self     bool       `json:"self,omitempty"`
	Online   bool       `json:"online"`
	SyncedAt *time.Time `json:"synced_at,omitempty"` // when its sessions were last read; not set for this node
	Sessions int        `json:"sessions"`
	Protocol int        `json:"protocol,omitempty"` // the protocol version it speaks, if it is a peer now
	Version  string     `json:"version,omitempty"`  // the build it runs, if it said
	// Incompatible means it speaks another protocol than this node, so its
	// sessions are no longer read; what was read before is still shown.
	Incompatible bool `json:"incompatible,omitempty"`
	// Note says, for this node, why the sessions of its machine are not
	// shared, if they are not.
	Note string `json:"note,omitempty"`
}

// Entry is one session in a listing.
type Entry struct {
	Ref          string    `json:"ref"` // what h8c show takes
	NodeID       string    `json:"node_id"`
	Node         string    `json:"node"`
	Online       bool      `json:"online"`
	Incompatible bool      `json:"incompatible,omitempty"`
	Key          string    `json:"key"`
	Agent        string    `json:"agent"`
	Title        string    `json:"title"`
	Project      string    `json:"project"`
	ProjectPath  string    `json:"project_path"`
	Branch       string    `json:"branch,omitempty"`
	Model        string    `json:"model,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Messages     int64     `json:"messages"`
	Parent       string    `json:"parent,omitempty"`
	Spawned      bool      `json:"spawned,omitempty"`
	Archived     bool      `json:"archived,omitempty"`
}

// Listing is the answer to h8c sessions.
type Listing struct {
	Nodes    []Node  `json:"nodes"`
	Sessions []Entry `json:"sessions"`
	// Next continues the listing when passed as "after"; it is empty on
	// the last page.
	Next string `json:"next,omitempty"`
}

// nodes returns this node and every node whose sessions are known or that
// is a peer, by id.
func (s *Service) nodes(ctx context.Context) (map[string]*Node, error) {
	counts, err := s.Store.Counts(ctx)
	if err != nil {
		return nil, err
	}
	feeds, err := s.Store.Feeds(ctx)
	if err != nil {
		return nil, err
	}
	nodes := map[string]*Node{
		s.Self.ID: {ID: s.Self.ID, Name: s.Self.Name, Self: true, Online: true, Protocol: s.Protocol,
			Version: s.Self.Version, Note: s.localNote()},
	}
	for id, f := range feeds {
		if id == s.Self.ID {
			continue
		}
		n := &Node{ID: id, Name: f.Name}
		if !f.SyncedAt.IsZero() {
			at := f.SyncedAt.UTC()
			n.SyncedAt = &at
		}
		nodes[id] = n
	}
	for _, p := range s.Peers() {
		n := nodes[p.ID]
		if n == nil {
			n = &Node{ID: p.ID}
			nodes[p.ID] = n
		}
		n.Name, n.Online = p.Name, p.Online
		n.Protocol, n.Version, n.Incompatible = p.Protocol, p.Version, p.Protocol != s.Protocol
	}
	for id, n := range nodes {
		n.Sessions = counts[id]
	}
	return nodes, nil
}

// ref is how a session is named on the command line: the node, a slash,
// the key. The node's name is used where it says which node is meant.
func ref(n *Node, nodes map[string]*Node, key string) string {
	label := n.Name
	same := 0
	for _, other := range nodes {
		if other.Name == n.Name || other.ID == n.Name {
			same++
		}
	}
	if label == "" || same != 1 || strings.Contains(label, "/") {
		label = n.ID
	}
	return label + "/" + key
}

// resolve finds the node a user means by its id or its name.
func resolve(nodes map[string]*Node, name string) (*Node, error) {
	if n := nodes[name]; n != nil {
		return n, nil
	}
	var found *Node
	for _, n := range nodes {
		if n.Name == name {
			if found != nil {
				return nil, fmt.Errorf("several nodes are called %q; use the node id", name)
			}
			found = n
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no node %q", name)
	}
	return found, nil
}

// List returns the sessions of all nodes that match q. q.Node may be a
// node's id or name.
func (s *Service) List(ctx context.Context, q Query) (Listing, error) {
	nodes, err := s.nodes(ctx)
	if err != nil {
		return Listing{}, err
	}
	if q.Node != "" {
		n, err := resolve(nodes, q.Node)
		if err != nil {
			return Listing{}, err
		}
		q.Node = n.ID
	}
	if q.Limit < 1 {
		q.Limit = 50
	}
	q.Limit = min(q.Limit, MaxPage)
	q.Limit++ // one more, to know whether the listing goes on
	listed, err := s.Store.List(ctx, q)
	if err != nil {
		return Listing{}, err
	}
	next := ""
	if len(listed) == q.Limit {
		listed = listed[:q.Limit-1]
		last := listed[len(listed)-1]
		next = position{UpdatedAt: last.UpdatedAt, Node: last.Node, Key: last.Key}.String()
	}
	out := Listing{Nodes: make([]Node, 0, len(nodes)), Sessions: make([]Entry, 0, len(listed)), Next: next}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, *n)
	}
	sortNodes(out.Nodes)
	for _, l := range listed {
		n := nodes[l.Node]
		if n == nil { // sessions of a node that is neither a peer nor on record
			n = &Node{ID: l.Node}
		}
		out.Sessions = append(out.Sessions, entry(l.Session, n, nodes))
	}
	return out, nil
}

func entry(sess Session, n *Node, nodes map[string]*Node) Entry {
	return Entry{
		Ref: ref(n, nodes, sess.Key), NodeID: n.ID, Node: n.Name, Online: n.Online, Incompatible: n.Incompatible,
		Key: sess.Key, Agent: sess.Agent, Title: sess.Title, Project: sess.Project,
		ProjectPath: sess.ProjectPath, Branch: sess.Branch, Model: sess.Model,
		CreatedAt: time.UnixMilli(sess.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(sess.UpdatedAt).UTC(),
		Messages: sess.Messages, Parent: sess.Parent, Spawned: sess.Spawned, Archived: sess.Archived,
	}
}

func sortNodes(nodes []Node) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && lessNode(nodes[j], nodes[j-1]); j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}
}

func lessNode(a, b Node) bool {
	if a.Self != b.Self {
		return a.Self
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.ID < b.ID
}

// Reading is a page of a session, with where it came from.
type Reading struct {
	Session  Entry     `json:"session"`
	Messages []Message `json:"messages"`
	// Next is the --after value of the next page, 0 on the last one.
	Next int64 `json:"next"`
	// From is "local" for a session of this machine, "peer" when the
	// messages were just read from the node the session is on, and "cache"
	// when they are the copy kept from an earlier reading.
	From string `json:"from"`
	// CachedAt is when the copy was made, for "peer" and "cache".
	CachedAt *time.Time `json:"cached_at,omitempty"`
	// Stale means the session has changed since the copy was made, as far
	// as this node knows. Incomplete means the copy stops before the end.
	Stale      bool   `json:"stale,omitempty"`
	Incomplete bool   `json:"incomplete,omitempty"`
	Note       string `json:"note,omitempty"`
}

// NotCachedError means a session of a node that cannot be reached now was
// never read on this machine, so there is no copy to show.
type NotCachedError struct {
	Node string
	Why  string
	Code string // the word for Why, if it has one: see Code
}

func (e *NotCachedError) Error() string {
	return fmt.Sprintf("%s %s and this session has not been read on this machine before, so there is no copy of it here",
		e.Node, e.Why)
}

// ErrorHeader is a header of an answer that says a session cannot be read.
// The answer itself is a sentence for a person, in English; the header
// gives the reason as one word, so that a program can say it in its own
// words. It is not sent for a reason that has no word.
const ErrorHeader = "H8c-Error"

// The words for why a session cannot be read.
const (
	CodeNotFound      = "not-found"             // no such session, or no longer
	CodeOffline       = "offline-not-read"      // its node is offline, and there is no copy
	CodeIncompatible  = "incompatible-not-read" // its node speaks another protocol, and there is no copy
	CodeFeedRestarted = "feed-restarted"        // its node started its feed over; a moment later it can be read
)

// Code returns the word for a failure of Read, or "".
func Code(err error) string {
	var notCached *NotCachedError
	switch {
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrFeedChanged):
		return CodeFeedRestarted
	case errors.As(err, &notCached):
		return notCached.Code
	}
	return ""
}

// Read returns a page of the session key on the node named node: messages
// with an ID above after, at most limit.
func (s *Service) Read(ctx context.Context, node, key string, after int64, limit int) (Reading, error) {
	nodes, err := s.nodes(ctx)
	if err != nil {
		return Reading{}, err
	}
	n, err := resolve(nodes, node)
	if err != nil {
		return Reading{}, err
	}
	sess, rev, ok, err := s.Store.Lookup(ctx, n.ID, key)
	if err != nil {
		return Reading{}, err
	}
	if !ok {
		return Reading{}, fmt.Errorf("%w %q on %s", ErrNotFound, key, n.Name)
	}
	out := Reading{Session: entry(sess, n, nodes)}

	var msgs []Message
	var more bool
	if n.Self {
		if s.Source == nil {
			return Reading{}, ErrNoIndex
		}
		out.From = "local"
		if msgs, more, err = s.Source.Messages(ctx, key, after, limit, PageBudget); err != nil {
			return Reading{}, err
		}
	} else {
		cp, err := s.copyOf(ctx, n, key, rev, &out)
		if err != nil {
			return Reading{}, err
		}
		if msgs, more, err = s.Store.CachedMessages(ctx, n.ID, key, cp, after, limit, PageBudget); err != nil {
			return Reading{}, err
		}
	}
	if msgs == nil {
		msgs = []Message{}
	}
	out.Messages = msgs
	if more {
		out.Next = msgs[len(msgs)-1].ID
	}
	return out, nil
}

// copyOf returns the copy of another node's session to read from. If the
// node can be reached and there is no copy, or the session has changed
// since it was made, a new one is made first. If that is not possible, an
// older copy is still better than nothing, and out says so.
func (s *Service) copyOf(ctx context.Context, n *Node, key string, rev int64, out *Reading) (Cached, error) {
	// One copy of a session at a time. Whoever waited may find the copy
	// that was just made.
	defer s.lockCopy(n.ID, key)()

	have, cached, err := s.Store.Cached(ctx, n.ID, key)
	if err != nil {
		return Cached{}, err
	}
	// A copy that stops short of the end was cut at the size limit. While
	// the session is the same, making it again would end the same way.
	fresh := cached && have.Rev >= rev

	why, code := "is offline", CodeOffline
	if !fresh {
		for _, p := range s.Peers() {
			if p.ID != n.ID || !p.Online {
				continue
			}
			if p.Protocol != s.Protocol {
				why, code = fmt.Sprintf("speaks protocol %d while this node speaks %d", p.Protocol, s.Protocol), CodeIncompatible
				break
			}
			if p.Addr == "" {
				why, code = "cannot be reached from here", ""
				break
			}
			made, err := s.fetchCopy(ctx, p, key)
			if err == nil {
				out.From = "peer"
				at := made.FetchedAt.UTC()
				out.CachedAt = &at
				out.Incomplete = !made.Complete
				return made, nil
			}
			if errors.Is(err, ErrFeedChanged) {
				// What is on record about this node is being replaced; an
				// older copy went with it.
				return Cached{}, fmt.Errorf("%s restarted its session feed; try again in a moment: %w", n.Name, err)
			}
			why, code = "did not answer ("+err.Error()+")", ""
		}
	}
	if !cached {
		return Cached{}, &NotCachedError{Node: n.Name, Why: why, Code: code}
	}
	out.From = "cache"
	at := have.FetchedAt.UTC()
	out.CachedAt = &at
	out.Stale = have.Rev < rev
	out.Incomplete = !have.Complete
	if !fresh {
		out.Note = fmt.Sprintf("%s %s; this is the copy from %s", n.Name, why, at.Local().Format(time.DateTime))
	}
	return have, nil
}

// fetchCopy reads a whole session from the peer into a new copy and puts
// it in use. The copy is written page by page, so its size in memory is
// one page. On disk it stops at maxCopyBytes, or after maxCopyPages
// answers, and is then marked as not reaching the end. A copy that is not
// put in use, for whatever reason, is removed again.
func (s *Service) fetchCopy(ctx context.Context, p Peer, key string) (made Cached, err error) {
	// The copy belongs to the feed as it is on record now. If the node
	// starts a new feed while the copy is under way, revisions mean
	// something else and the copy must not be kept.
	feed, err := s.Store.Feed(ctx, p.ID)
	if err != nil {
		return Cached{}, err
	}
	gen, err := s.Store.NewCopy(ctx, p.ID, key)
	if err != nil {
		return Cached{}, err
	}
	defer func() {
		if err != nil {
			s.Store.DropCopy(context.WithoutCancel(ctx), p.ID, key, gen)
		}
	}()

	var rev, after int64
	size, complete := 0, false
pages:
	for n := 0; n < maxCopyPages; n++ {
		var pg messagesPage
		path := "/v1/sessions/messages?key=" + url.QueryEscape(key) +
			"&after=" + strconv.FormatInt(after, 10) + "&limit=" + strconv.Itoa(MaxPage)
		if err := s.Fetch(ctx, p, path, &pg); err != nil {
			return Cached{}, err
		}
		// A build from before the answer carried the epoch leaves it out.
		// Then this check is skipped; UseCopy still compares the feed on
		// record with the one the copy was started under.
		if pg.Epoch != "" && pg.Epoch != feed.Epoch {
			return Cached{}, ErrFeedChanged
		}
		if len(pg.Messages) > MaxPage {
			return Cached{}, errors.New("the answer is not a page of a session")
		}
		if n == 0 {
			rev = pg.Rev
		}
		keep := pg.Messages[:0]
		for _, m := range pg.Messages {
			if m.ID <= after {
				return Cached{}, errors.New("the session does not go forward")
			}
			after = m.ID
			m = clampMessage(m)
			if size += MessageSize(m); size > maxCopyBytes {
				if err := s.Store.AddToCopy(ctx, p.ID, key, gen, keep); err != nil {
					return Cached{}, err
				}
				break pages
			}
			keep = append(keep, m)
		}
		if err := s.Store.AddToCopy(ctx, p.ID, key, gen, keep); err != nil {
			return Cached{}, err
		}
		if !pg.More || len(pg.Messages) == 0 {
			complete = true
			break
		}
	}
	if err := s.Store.UseCopy(ctx, p.ID, key, feed.Epoch, gen, rev, complete); err != nil {
		return Cached{}, err
	}
	made, _, err = s.Store.Cached(ctx, p.ID, key)
	return made, err
}

// clampMessage cuts a message received from a peer to what one may hold.
func clampMessage(m Message) Message {
	if len(m.Text) > MaxText { // bytes; only then count characters
		if r := []rune(m.Text); len(r) > MaxText {
			m.Text, m.Truncated = string(r[:MaxText]), true
		}
	}
	for _, f := range []*string{&m.Role, &m.Sidechain} {
		if len(*f) > maxField {
			*f = strings.ToValidUTF8((*f)[:maxField], "")
		}
	}
	return m
}

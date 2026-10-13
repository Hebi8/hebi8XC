package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

// memSource is a session index in memory.
type memSource struct {
	sessions map[string]Session
	messages map[string][]Message
	nextID   int64
	err      error // if set, the index cannot be read
	read     int   // messages handed out
}

func newSource() *memSource {
	return &memSource{sessions: make(map[string]Session), messages: make(map[string][]Message)}
}

// put adds or replaces a session; texts become its messages.
func (m *memSource) put(key, title string, updated int64, texts ...string) {
	agent, _, _ := strings.Cut(key, ":")
	m.sessions[key] = Session{Key: key, Agent: agent, Title: title, Project: "proj", ProjectPath: "/home/alice/proj",
		UpdatedAt: updated, CreatedAt: updated - 5}
	m.messages[key] = nil
	m.add(key, texts...)
}

func (m *memSource) add(key string, texts ...string) {
	for _, text := range texts {
		m.nextID++
		m.messages[key] = append(m.messages[key], Message{ID: m.nextID, Seq: int64(len(m.messages[key]) + 1),
			Role: "user", Text: text})
	}
	s := m.sessions[key]
	s.Messages = int64(len(m.messages[key]))
	s.Stamp = fmt.Sprint(m.nextID)
	m.sessions[key] = s
}

func (m *memSource) remove(key string) {
	delete(m.sessions, key)
	delete(m.messages, key)
}

func (m *memSource) Sessions(context.Context) ([]Session, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []Session
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out, nil
}

func (m *memSource) Messages(_ context.Context, key string, after int64, limit, budget int) ([]Message, bool, error) {
	if m.err != nil {
		return nil, false, m.err
	}
	var out []Message
	size := 0
	for _, msg := range m.messages[key] {
		if msg.ID <= after {
			continue
		}
		if r := []rune(msg.Text); len(r) > MaxText {
			msg.Text, msg.Truncated = string(r[:MaxText]), true
		}
		size += MessageSize(msg)
		if len(out) == limit || (size > budget && len(out) > 0) {
			return out, true, nil
		}
		m.read++
		out = append(out, msg)
	}
	return out, false, nil
}

// machine is a node's session service with an index in memory. Machines
// reach each other through net, without a network: a request is answered
// by the other machine's service and passed through JSON on the way, as it
// is between real nodes.
type machine struct {
	*Service
	t        *testing.T
	src      *memSource
	net      *network
	dir      string
	online   bool
	protocol int // the protocol version this machine speaks
}

type network struct {
	t        *testing.T
	now      time.Time
	machines map[string]*machine
	requests []string          // every path asked of a peer
	before   func(path string) // called before a request is answered
}

func newNetwork(t *testing.T) *network {
	return &network{t: t, now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC), machines: make(map[string]*machine)}
}

func (n *network) machine(id, name string) *machine {
	n.t.Helper()
	m := &machine{t: n.t, src: newSource(), net: n, dir: n.t.TempDir(), online: true, protocol: 1}
	n.machines[id] = m
	m.open(id, name)
	return m
}

// open starts the machine's service on the store in its directory.
func (m *machine) open(id, name string) {
	m.t.Helper()
	store, err := OpenStore(filepath.Join(m.dir, "sessions.db"), func() time.Time { return m.net.now })
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { store.Close() })
	m.Service = &Service{
		Self:     Peer{ID: id, Name: name},
		Protocol: m.protocol,
		Store:    store,
		Source:   m.src,
		Peers:    func() []Peer { return m.net.peersOf(id) },
		Fetch:    m.net.fetch,
		Now:      func() time.Time { return m.net.now },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (n *network) peersOf(id string) []Peer {
	var out []Peer
	for other, m := range n.machines {
		if other != id {
			out = append(out, Peer{ID: other, Name: m.Self.Name, Online: m.online, Addr: other, Protocol: m.protocol})
		}
	}
	return out
}

var errOffline = errors.New("connection refused")

func (n *network) fetch(ctx context.Context, p Peer, path string, out any) error {
	n.requests = append(n.requests, path)
	if n.before != nil {
		n.before(path)
	}
	m := n.machines[p.ID]
	if !m.online {
		return errOffline
	}
	u, err := url.Parse(path)
	if err != nil {
		return err
	}
	q := u.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit := limitParam(q.Get("limit"), MaxPage)
	var answer any
	switch u.Path {
	case "/v1/sessions":
		answer, err = m.LocalChanges(ctx, after, limit)
	case "/v1/sessions/messages":
		answer, err = m.LocalMessages(ctx, q.Get("key"), after, limit)
	default:
		err = fmt.Errorf("no route %s", u.Path)
	}
	if err != nil {
		return err
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	if len(raw) > MaxReply {
		return fmt.Errorf("answer of %d bytes is over MaxReply", len(raw))
	}
	return json.Unmarshal(raw, out)
}

// sync lets every machine that is online look at its own sessions, and
// then runs a round on each, in which it reads the feeds of the others.
func (n *network) sync() {
	for _, m := range n.machines {
		if m.online {
			m.SyncLocal(ctx)
		}
	}
	for _, m := range n.machines {
		if m.online {
			m.Round(ctx)
		}
	}
}

func (m *machine) list(q Query) Listing {
	m.t.Helper()
	l, err := m.List(ctx, q)
	if err != nil {
		m.t.Fatal(err)
	}
	return l
}

// titles returns "node/key=title" for every listed session, sorted.
func titles(l Listing) []string {
	var out []string
	for _, s := range l.Sessions {
		out = append(out, s.Ref+"="+s.Title)
	}
	slices.Sort(out)
	return out
}

// readAll reads a session page by page and returns the texts.
func (m *machine) readAll(node, key string, pageSize int) (texts []string, last Reading, err error) {
	after := int64(0)
	for {
		r, err := m.Read(ctx, node, key, after, pageSize)
		if err != nil {
			return nil, Reading{}, err
		}
		for _, msg := range r.Messages {
			texts = append(texts, msg.Text)
		}
		if r.Next == 0 {
			return texts, r, nil
		}
		after = r.Next
	}
}

func pair(t *testing.T) (desk, laptop *machine, net *network) {
	net = newNetwork(t)
	return net.machine("d1", "desk"), net.machine("l1", "laptop"), net
}

func TestSessionsOfAllMachinesAreListed(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "Fix the parser", 3000, "hello", "world")
	desk.src.put("codex:b", "Write the docs", 1000)
	laptop.src.put("claude:c", "Plan the trip", 2000, "tickets")
	net.sync()

	for _, m := range []*machine{desk, laptop} {
		got := m.list(Query{})
		want := []string{"desk/claude:a=Fix the parser", "desk/codex:b=Write the docs", "laptop/claude:c=Plan the trip"}
		if !slices.Equal(titles(got), want) {
			t.Errorf("%s lists %v", m.Self.Name, titles(got))
		}
		// Newest first.
		if got.Sessions[0].Key != "claude:a" || got.Sessions[2].Key != "codex:b" {
			t.Errorf("%s: order is %v", m.Self.Name, got.Sessions)
		}
	}

	l := laptop.list(Query{})
	if len(l.Nodes) != 2 || !l.Nodes[0].Self || l.Nodes[0].Name != "laptop" || l.Nodes[0].Sessions != 1 {
		t.Fatalf("nodes = %+v", l.Nodes)
	}
	if n := l.Nodes[1]; n.Name != "desk" || !n.Online || n.Sessions != 2 || n.SyncedAt == nil || !n.SyncedAt.Equal(net.now) {
		t.Errorf("desk as laptop sees it = %+v", n)
	}
	first := l.Sessions[0]
	if first.NodeID != "d1" || first.Node != "desk" || !first.Online || first.Agent != "claude" ||
		first.Messages != 2 || !first.UpdatedAt.Equal(time.UnixMilli(3000)) || first.ProjectPath != "/home/alice/proj" {
		t.Errorf("entry = %+v", first)
	}
}

func TestFeedGivesEachChangeARevision(t *testing.T) {
	desk, _, _ := pair(t)
	revs := func() map[string]int64 {
		changes, err := desk.Store.Changes(ctx, "d1", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]int64)
		for _, c := range changes {
			out[c.Key] = c.Rev
			if c.Deleted {
				out[c.Key] = -c.Rev
			}
		}
		return out
	}
	desk.src.put("claude:a", "A", 1)
	desk.src.put("claude:b", "B", 2)
	desk.SyncLocal(ctx)
	first := revs()
	if len(first) != 2 || first["claude:a"]+first["claude:b"] != 3 {
		t.Fatalf("first revisions = %v", first)
	}

	desk.SyncLocal(ctx)
	if got := revs(); got["claude:a"] != first["claude:a"] || got["claude:b"] != first["claude:b"] {
		t.Errorf("revisions moved without a change: %v", got)
	}

	desk.src.add("claude:a", "more")
	desk.src.remove("claude:b")
	desk.SyncLocal(ctx)
	got := revs()
	if got["claude:a"] <= 2 || got["claude:b"] >= -2 || got["claude:a"] == -got["claude:b"] {
		t.Errorf("after an update and a removal: %v, want new revisions 3 and 4, b marked gone", got)
	}

	desk.src.put("claude:b", "B is back", 9)
	desk.SyncLocal(ctx)
	if got := revs(); got["claude:b"] != 5 {
		t.Errorf("a session that came back: %v", got)
	}
}

// An index that cannot be read is not an index without sessions.
func TestUnreadableIndexLeavesTheFeedAlone(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1, "text")
	net.sync()

	desk.src.err = errors.New("database is locked")
	net.sync()
	if got := titles(laptop.list(Query{})); !slices.Equal(got, []string{"desk/claude:a=A"}) {
		t.Errorf("laptop lists %v after desk's index failed", got)
	}
	desk.src.err = nil
	net.sync()
	if got := titles(laptop.list(Query{})); len(got) != 1 {
		t.Errorf("laptop lists %v after desk's index recovered", got)
	}
}

func TestFeedIsReadInPagesWithoutGapsOrRepeats(t *testing.T) {
	desk, laptop, net := pair(t)
	const total = 2*MaxPage + 203
	for i := range total {
		desk.src.put(fmt.Sprintf("claude:%04d", i), fmt.Sprintf("Session %d", i), int64(i))
	}
	desk.SyncLocal(ctx)

	// While laptop is between two pages, desk changes a session laptop has
	// already read, removes another and adds a new one.
	pages := 0
	net.before = func(path string) {
		if !strings.HasPrefix(path, "/v1/sessions?") {
			return
		}
		pages++
		if pages == 2 {
			desk.src.put("claude:0000", "Renamed", 1)
			desk.src.remove("claude:0001")
			desk.src.put("claude:new", "Brand new", 99999)
			desk.SyncLocal(ctx)
		}
	}
	if err := laptop.Pull(ctx, Peer{ID: "d1", Name: "desk", Online: true, Addr: "d1"}); err != nil {
		t.Fatal(err)
	}
	net.before = nil
	if pages != 3 { // 500, 500, and the rest together with the three changes
		t.Errorf("the feed was read in %d requests, want 3", pages)
	}

	got := laptop.list(Query{Node: "desk", Limit: MaxPage})
	if got.Nodes[1].Sessions != total {
		t.Fatalf("laptop has %d sessions of desk, want %d", got.Nodes[1].Sessions, total)
	}
	seen := make(map[string]string)
	after := int64(0)
	for { // walk laptop's copy of the feed, which List cannot show in one go
		changes, err := laptop.Store.Changes(ctx, "d1", after, 300)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) == 0 {
			break
		}
		for _, c := range changes {
			if _, dup := seen[c.Key]; dup {
				t.Fatalf("%s is in the copy twice", c.Key)
			}
			seen[c.Key] = c.Title
			after = c.Rev
		}
	}
	if len(seen) != total || seen["claude:0000"] != "Renamed" || seen["claude:new"] != "Brand new" {
		t.Errorf("copy has %d sessions, 0000=%q new=%q", len(seen), seen["claude:0000"], seen["claude:new"])
	}
	if _, kept := seen["claude:0001"]; kept {
		t.Error("the removed session is still in the copy")
	}

	// Nothing changed: the next round asks once and gets nothing.
	net.requests = nil
	net.sync()
	if len(net.requests) != 2 { // one request each way
		t.Errorf("an idle round made requests %v", net.requests)
	}
}

func TestUpdatedSessionReachesThePeerAndReplacesItsCopy(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "Fix the parser", 1000, "one", "two")
	net.sync()

	texts, r, err := laptop.readAll("desk", "claude:a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(texts, []string{"one", "two"}) || r.From != "peer" || r.Session.Title != "Fix the parser" {
		t.Fatalf("first reading: %v from %q", texts, r.From)
	}
	// Again: the copy is current, desk is not asked.
	net.requests = nil
	if _, r, _ = laptop.readAll("desk", "claude:a", 10); r.From != "cache" || r.Stale || r.Note != "" || len(net.requests) != 0 {
		t.Errorf("second reading: from %q, stale %v, note %q, requests %v", r.From, r.Stale, r.Note, net.requests)
	}

	desk.src.put("claude:a", "Fix the parser, again", 2000, "one", "two", "three")
	net.sync()
	if got := titles(laptop.list(Query{})); !slices.Equal(got, []string{"desk/claude:a=Fix the parser, again"}) {
		t.Errorf("laptop lists %v", got)
	}
	texts, r, err = laptop.readAll("desk", "claude:a", 10)
	if err != nil || !slices.Equal(texts, []string{"one", "two", "three"}) || r.From != "peer" || r.Session.Messages != 3 {
		t.Errorf("reading after the update: %v from %q, %v", texts, r.From, err)
	}
}

func TestRemovedSessionLeavesThePeersCache(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "Stays", 1000, "kept")
	desk.src.put("claude:b", "Goes", 2000, "secret text")
	net.sync()
	if _, _, err := laptop.readAll("desk", "claude:b", 10); err != nil {
		t.Fatal(err)
	}

	desk.src.remove("claude:b")
	net.sync()
	if got := titles(laptop.list(Query{})); !slices.Equal(got, []string{"desk/claude:a=Stays"}) {
		t.Errorf("laptop lists %v", got)
	}
	if _, _, err := laptop.readAll("desk", "claude:b", 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading the removed session: err = %v, want ErrNotFound", err)
	}
	var left int
	if err := laptop.Store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE key = 'claude:b') +
		(SELECT COUNT(*) FROM content WHERE key = 'claude:b')`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows of the removed session are still cached (%v)", left, err)
	}

	// Removed while laptop was away: it still learns of it.
	desk.src.put("claude:c", "Short-lived", 3000)
	net.sync()
	laptop.online = false
	desk.src.remove("claude:c")
	net.sync()
	laptop.online = true
	net.sync()
	if got := titles(laptop.list(Query{})); !slices.Equal(got, []string{"desk/claude:a=Stays"}) {
		t.Errorf("laptop lists %v after coming back", got)
	}
}

// A node that lost its store starts a new feed. Its peers notice and read
// it from the start, once.
func TestNewFeedIsReadFromTheStart(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1)
	desk.src.put("claude:b", "B", 2)
	net.sync()

	desk.Store.Close()
	if err := os.Remove(filepath.Join(desk.dir, "sessions.db")); err != nil {
		t.Fatal(err)
	}
	desk.open("d1", "desk")
	desk.src.remove("claude:a")
	desk.src.put("claude:z", "Z", 3)
	net.sync()
	if got := titles(laptop.list(Query{})); !slices.Equal(got, []string{"desk/claude:b=B", "desk/claude:z=Z"}) {
		t.Errorf("laptop lists %v after desk's new feed", got)
	}

	for range 3 {
		net.requests = nil
		net.sync()
		if len(net.requests) != 2 {
			t.Fatalf("with nothing new, a round made requests %v", net.requests)
		}
	}
}

func TestOfflineNodeIsListedAndReadFromTheCopy(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:read", "Was read", 1000, "one", "two", "three")
	desk.src.put("claude:unread", "Never read", 2000, "four")
	net.sync()
	synced := net.now
	if _, _, err := laptop.readAll("desk", "claude:read", 2); err != nil {
		t.Fatal(err)
	}

	desk.online = false
	net.now = net.now.Add(3 * time.Hour)
	net.sync()

	l := laptop.list(Query{})
	if len(l.Sessions) != 2 || l.Sessions[0].Online || l.Sessions[1].Online {
		t.Fatalf("laptop lists %+v", l.Sessions)
	}
	if n := l.Nodes[1]; n.Online || n.SyncedAt == nil || !n.SyncedAt.Equal(synced) || n.Sessions != 2 {
		t.Errorf("desk while offline = %+v, want offline, as of %v", n, synced)
	}

	texts, r, err := laptop.readAll("desk", "claude:read", 2)
	if err != nil || !slices.Equal(texts, []string{"one", "two", "three"}) {
		t.Fatalf("reading the copy: %v, %v", texts, err)
	}
	if r.From != "cache" || r.CachedAt == nil || !r.CachedAt.Equal(synced) || r.Stale {
		t.Errorf("reading = from %q, cached at %v, stale %v", r.From, r.CachedAt, r.Stale)
	}

	_, _, err = laptop.readAll("desk", "claude:unread", 2)
	var notCached *NotCachedError
	if !errors.As(err, &notCached) || !strings.Contains(err.Error(), "desk is offline") ||
		!strings.Contains(err.Error(), "has not been read on this machine") || Code(err) != CodeOffline {
		t.Errorf("reading what was never read: err = %v, code %q", err, Code(err))
	}

	// Asked over the control routes, the failure is the same sentence as
	// ever, and a header gives the reason as a word.
	mux := http.NewServeMux()
	laptop.ControlRoutes(mux)
	ask := func(key string) (int, string, string) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/messages?node=desk&key="+key, nil))
		return w.Code, w.Header().Get(ErrorHeader), strings.TrimSpace(w.Body.String())
	}
	if status, code, said := ask("claude:unread"); status != http.StatusServiceUnavailable || code != "offline-not-read" || said != err.Error() {
		t.Errorf("what was never read: %d, code %q, %q", status, code, said)
	}
	if status, code, said := ask("claude:gone"); status != http.StatusNotFound || code != "not-found" || !strings.Contains(said, "no such session") {
		t.Errorf("what does not exist: %d, code %q, %q", status, code, said)
	}
	if status, code, _ := ask("claude:read"); status != http.StatusOK || code != "" {
		t.Errorf("what can be read: %d, code %q", status, code)
	}
	// A failure without a word of its own has no header.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/messages?node=nobody&key=claude:read", nil))
	if w.Code != http.StatusBadRequest || w.Header().Get(ErrorHeader) != "" {
		t.Errorf("an unknown node: %d, code %q", w.Code, w.Header().Get(ErrorHeader))
	}
}

// The copy is older than the session and the node cannot be asked: the
// copy is shown, and the reader is told.
func TestStaleCopyIsShownAndMarked(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1000, "one")
	net.sync()
	if _, _, err := laptop.readAll("desk", "claude:a", 10); err != nil {
		t.Fatal(err)
	}
	desk.src.add("claude:a", "two")
	net.sync() // laptop's index knows of the change, its copy does not have it

	desk.src.err = errors.New("index is being rebuilt") // desk answers, but cannot read
	texts, r, err := laptop.readAll("desk", "claude:a", 10)
	if err != nil || !slices.Equal(texts, []string{"one"}) {
		t.Fatalf("reading: %v, %v", texts, err)
	}
	if r.From != "cache" || !r.Stale || !strings.Contains(r.Note, "did not answer") {
		t.Errorf("reading = from %q, stale %v, note %q", r.From, r.Stale, r.Note)
	}
	if r.Session.Messages != 2 {
		t.Errorf("the session is described with %d messages, want the 2 the index knows of", r.Session.Messages)
	}

	desk.src.err = nil
	if texts, r, _ = laptop.readAll("desk", "claude:a", 10); !slices.Equal(texts, []string{"one", "two"}) || r.Stale {
		t.Errorf("after desk recovered: %v, stale %v", texts, r.Stale)
	}
}

// A copy that fails half way does not replace the one in use.
func TestFailedCopyKeepsTheOldOne(t *testing.T) {
	desk, laptop, net := pair(t)
	// Enough text for three answers: a message is at most MaxText long.
	var texts []string
	for range 3 * PageBudget / MaxText {
		texts = append(texts, strings.Repeat("x", MaxText))
	}
	texts = append(texts, "end")
	desk.src.put("claude:a", "A", 1000, texts...)
	net.sync()
	if _, _, err := laptop.readAll("desk", "claude:a", 10); err != nil {
		t.Fatal(err)
	}
	desk.src.add("claude:a", "newer")
	net.sync()

	asked := 0
	net.before = func(path string) {
		if strings.HasPrefix(path, "/v1/sessions/messages") {
			if asked++; asked == 2 {
				desk.online = false // desk goes away between two pages
			}
		}
	}
	got, r, err := laptop.readAll("desk", "claude:a", 10)
	if err != nil || len(got) != len(texts) || got[len(got)-1] != "end" || !r.Stale || r.From != "cache" {
		t.Fatalf("reading during the failure: %d messages, stale %v, from %q, %v", len(got), r.Stale, r.From, err)
	}
	if asked < 2 {
		t.Fatalf("the copy was made in %d requests; the test needs it to take several", asked)
	}
	var copies int
	if err := laptop.Store.db.QueryRow(`SELECT COUNT(DISTINCT gen) FROM messages`).Scan(&copies); err != nil || copies != 1 {
		t.Errorf("%d copies are stored, want only the one in use (%v)", copies, err)
	}

	net.before, desk.online = nil, true
	if got, _, _ = laptop.readAll("desk", "claude:a", 10); len(got) != len(texts)+1 || got[len(got)-1] != "newer" {
		t.Errorf("after desk came back: %d messages", len(got))
	}
}

func TestLongSessionsAndLongMessagesStayWithinTheLimits(t *testing.T) {
	desk, laptop, net := pair(t)
	long := strings.Repeat("é", MaxText+10)
	var texts []string
	for i := range 40 {
		texts = append(texts, strings.Repeat(string(rune('a'+i%26)), PageBudget/8))
	}
	texts = append(texts, long, "<end & done>")
	desk.src.put("claude:big", "Big", 1000, texts...)
	net.sync()

	net.requests = nil
	got, r, err := laptop.readAll("desk", "claude:big", MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(texts) || got[41] != "<end & done>" || got[0] != texts[0] {
		t.Fatalf("read %d messages, want %d", len(got), len(texts))
	}
	if n := len([]rune(got[40])); n != MaxText {
		t.Errorf("the long message has %d characters, want it cut to %d", n, MaxText)
	}
	if r.Incomplete {
		t.Error("the copy is marked incomplete")
	}
	// 40 messages of an eighth of the budget each cannot travel in one answer.
	if net.messageRequests() < 6 {
		t.Errorf("the session travelled in %d answers", net.messageRequests())
	}
	// And the local machine pages its own reading the same way.
	local, err := desk.Read(ctx, "desk", "claude:big", 0, MaxPage)
	if err != nil || len(local.Messages) >= len(texts) || local.Next == 0 || local.From != "local" {
		t.Errorf("local reading: %d messages, next %d, from %q, %v", len(local.Messages), local.Next, local.From, err)
	}
}

func TestReadingInPages(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1000, "1", "2", "3", "4", "5")
	net.sync()
	for _, m := range []*machine{desk, laptop} {
		r, err := m.Read(ctx, "desk", "claude:a", 0, 2)
		if err != nil || len(r.Messages) != 2 || r.Messages[1].Text != "2" || r.Next != r.Messages[1].ID {
			t.Fatalf("%s: first page %+v, %v", m.Self.Name, r, err)
		}
		r, err = m.Read(ctx, "d1", "claude:a", r.Next, 10) // the node by its id
		if err != nil || len(r.Messages) != 3 || r.Messages[0].Text != "3" || r.Next != 0 {
			t.Fatalf("%s: second page %+v, %v", m.Self.Name, r, err)
		}
		if r.Messages[0].Seq != 3 {
			t.Errorf("%s: seq = %d", m.Self.Name, r.Messages[0].Seq)
		}
	}
	if _, err := laptop.Read(ctx, "nobody", "claude:a", 0, 2); err == nil || !strings.Contains(err.Error(), `no node "nobody"`) {
		t.Errorf("unknown node: %v", err)
	}
	if _, err := laptop.Read(ctx, "desk", "claude:zzz", 0, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
}

func TestListFilters(t *testing.T) {
	desk, laptop, net := pair(t)
	put := func(m *machine, key, title, project, path string, updated int64) {
		m.src.put(key, title, updated)
		s := m.src.sessions[key]
		s.Project, s.ProjectPath = project, path
		m.src.sessions[key] = s
	}
	put(desk, "claude:1", "Fix the parser", "parser", "/home/alice/src/parser", 5000)
	put(desk, "codex:2", "100% coverage for a_b", "parser", "/home/alice/src/parser", 4000)
	put(desk, "claude:3", "Holiday plans", "notes", "/home/alice/notes", 3000)
	put(laptop, "claude:4", "Parser benchmarks", "bench", "/home/alice/src/bench", 2000)
	put(laptop, "gemini:5", "axb", "misc", "/home/alice/misc", 1000)
	net.sync()

	for name, tc := range map[string]struct {
		q    Query
		want string
	}{
		"everything":                   {Query{}, "claude:1 codex:2 claude:3 claude:4 gemini:5"},
		"limit":                        {Query{Limit: 2}, "claude:1 codex:2"},
		"node by name":                 {Query{Node: "laptop"}, "claude:4 gemini:5"},
		"node by id":                   {Query{Node: "d1"}, "claude:1 codex:2 claude:3"},
		"agent":                        {Query{Agent: "claude"}, "claude:1 claude:3 claude:4"},
		"project by name":              {Query{Project: "pars"}, "claude:1 codex:2"},
		"project by path":              {Query{Project: "/src/"}, "claude:1 codex:2 claude:4"},
		"since":                        {Query{Since: 3000}, "claude:1 codex:2 claude:3"},
		"text in title":                {Query{Text: []string{"PARSER"}}, "claude:1 codex:2 claude:4"},
		"text in project":              {Query{Text: []string{"notes"}}, "claude:3"},
		"every text must match":        {Query{Text: []string{"parser", "bench"}}, "claude:4"},
		"percent is not a wildcard":    {Query{Text: []string{"100%"}}, "codex:2"},
		"underscore is not a wildcard": {Query{Text: []string{"a_b"}}, "codex:2"},
		"quote is just text":           {Query{Text: []string{"' OR 1=1 --"}}, ""},
		"combined":                     {Query{Node: "desk", Agent: "claude", Since: 4000}, "claude:1"},
	} {
		var got []string
		for _, s := range desk.list(tc.q).Sessions {
			got = append(got, s.Key)
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s: got %v, want %s", name, got, tc.want)
		}
	}
	if _, err := desk.List(ctx, Query{Node: "nobody"}); err == nil {
		t.Error("an unknown node was accepted")
	}
}

// Two nodes with the same name are told apart by id.
func TestRefsNameTheNodeUnambiguously(t *testing.T) {
	net := newNetwork(t)
	here := net.machine("h1", "here")
	a, b := net.machine("aaaa", "box"), net.machine("bbbb", "box")
	odd := net.machine("cccc", "top/left")
	a.src.put("claude:1", "On a", 3)
	b.src.put("claude:1", "On b", 2)
	odd.src.put("claude:1", "On the odd one", 1)
	net.sync()

	l := here.list(Query{})
	want := []string{"aaaa/claude:1=On a", "bbbb/claude:1=On b", "cccc/claude:1=On the odd one"}
	if !slices.Equal(titles(l), want) {
		t.Fatalf("refs = %v, want %v", titles(l), want)
	}
	for _, s := range l.Sessions {
		node, key, _ := strings.Cut(s.Ref, "/")
		r, err := here.Read(ctx, node, key, 0, 1)
		if err != nil || r.Session.Title != s.Title {
			t.Errorf("reading %s: %+v, %v", s.Ref, r.Session, err)
		}
	}
	if _, err := here.Read(ctx, "box", "claude:1", 0, 1); err == nil || !strings.Contains(err.Error(), "several nodes") {
		t.Errorf("ambiguous name: %v", err)
	}
}

// A peer that sends nonsense is stopped; it cannot make this node loop or
// store without end.
func TestFeedThatMisbehavesIsRefused(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1)
	net.sync()
	peer := Peer{ID: "d1", Name: "desk", Online: true, Addr: "d1"}

	for name, answer := range map[string]changesPage{
		"goes backwards": {Epoch: "e", Changes: []Change{{Rev: 5}, {Rev: 5}}},
		"no epoch":       {Changes: []Change{{Rev: 9}}},
		"too many":       {Epoch: "e", Changes: make([]Change, MaxPage+1)},
	} {
		laptop.Fetch = func(_ context.Context, _ Peer, _ string, out any) error {
			raw, _ := json.Marshal(answer)
			return json.Unmarshal(raw, out)
		}
		if err := laptop.Pull(ctx, peer); err == nil {
			t.Errorf("%s: the feed was accepted", name)
		}
	}

	// Always "more", never anything: the pull ends.
	asked := 0
	laptop.Fetch = func(_ context.Context, _ Peer, _ string, out any) error {
		asked++
		raw, _ := json.Marshal(changesPage{Epoch: "e", More: true})
		return json.Unmarshal(raw, out)
	}
	if err := laptop.Pull(ctx, peer); err != nil || asked > 2 {
		t.Errorf("an empty feed that promises more: %d requests, %v", asked, err)
	}

	// A message page that does not go forward.
	laptop.Fetch = net.fetch
	net.sync()
	laptop.Fetch = func(_ context.Context, _ Peer, path string, out any) error {
		raw, _ := json.Marshal(messagesPage{Rev: 1, More: true, Messages: []Message{{ID: 7, Text: "again"}}})
		return json.Unmarshal(raw, out)
	}
	if _, err := laptop.Read(ctx, "desk", "claude:a", 0, 10); err == nil {
		t.Error("a session that repeats itself was accepted")
	}
	var left int
	if err := laptop.Store.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d messages of the refused copy were kept (%v)", left, err)
	}
}

func TestOverlongFieldsAreCut(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", strings.Repeat("长", maxField), 1)
	net.sync()
	for _, m := range []*machine{desk, laptop} {
		if got := m.list(Query{}).Sessions[0].Title; len(got) > maxField || !strings.HasPrefix(got, "长长") {
			t.Errorf("%s keeps a title of %d bytes", m.Self.Name, len(got))
		}
	}
}

func TestStoreIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1, "text")
	net.sync()
	if _, _, err := laptop.readAll("desk", "claude:a", 10); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(laptop.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %o", e.Name(), perm)
		}
	}
}

// A machine without an index of its own still sees the others.
func TestMachineWithoutAnIndex(t *testing.T) {
	desk, laptop, net := pair(t)
	laptop.Source = nil
	desk.src.put("claude:a", "A", 1, "text")
	net.sync()
	if got := titles(laptop.list(Query{})); !slices.Equal(got, []string{"desk/claude:a=A"}) {
		t.Errorf("laptop lists %v", got)
	}
	if texts, _, err := laptop.readAll("desk", "claude:a", 10); err != nil || len(texts) != 1 {
		t.Errorf("laptop reads %v, %v", texts, err)
	}
	if got := desk.list(Query{}); len(got.Sessions) != 1 || got.Nodes[1].Sessions != 0 {
		t.Errorf("desk sees %+v", got)
	}
}

// The index is refreshed before it is read, but not in every round.
func TestRefreshRunsEveryFewRounds(t *testing.T) {
	desk, _, net := pair(t)
	runs := 0
	desk.Refresh = func(context.Context) (string, error) {
		runs++
		desk.src.put(fmt.Sprintf("claude:%d", runs), "Found by the refresh", int64(runs))
		return "Refreshed.", nil
	}
	desk.SyncLocal(ctx)
	if runs != 1 || len(desk.list(Query{}).Sessions) != 1 {
		t.Fatalf("first round: %d refreshes", runs)
	}
	net.now = net.now.Add(Interval)
	desk.SyncLocal(ctx)
	if runs != 1 {
		t.Errorf("refreshed again after %v", Interval)
	}
	net.now = net.now.Add(refreshEvery)
	desk.SyncLocal(ctx)
	if runs != 2 || len(desk.list(Query{}).Sessions) != 2 {
		t.Errorf("after %v: %d refreshes", refreshEvery, runs)
	}

	// A refresh that fails does not stop the index from being read.
	desk.Refresh = func(context.Context) (string, error) { return "", errors.New("wake-cli: exit status 2") }
	desk.src.put("claude:x", "Still seen", 99)
	net.now = net.now.Add(refreshEvery)
	desk.SyncLocal(ctx)
	if len(desk.list(Query{}).Sessions) != 3 {
		t.Error("the index was not read after a failed refresh")
	}
}

// messageRequests counts the requests for messages in what was asked of peers.
func (n *network) messageRequests() int {
	count := 0
	for _, path := range n.requests {
		if strings.HasPrefix(path, "/v1/sessions/messages") {
			count++
		}
	}
	return count
}

func (m *machine) cachedRows() int {
	m.t.Helper()
	var rows int
	if err := m.Store.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&rows); err != nil {
		m.t.Fatal(err)
	}
	return rows
}

// Two readers open the same session of another node at the same moment.
// The second waits for the first one's copy instead of making its own, so
// a failure of one can never take the other's copy with it.
func TestConcurrentReadersShareOneCopy(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1000, "one", "two", "three")
	net.sync()

	firstAsked, letFirst := make(chan struct{}), make(chan struct{})
	firstDone := make(chan struct{})
	var secondAsked atomic.Bool
	asked := 0
	net.before = func(path string) {
		if !strings.HasPrefix(path, "/v1/sessions/messages") {
			return
		}
		if asked++; asked == 1 {
			close(firstAsked)
			<-letFirst
			return
		}
		// Only a second copy under way would ask again. Let it fail, after
		// the first copy is in use: that is when it used to do damage.
		secondAsked.Store(true)
		<-firstDone
		desk.online = false
	}

	type result struct {
		texts []string
		err   error
	}
	read := func(out chan<- result) {
		texts, _, err := laptop.readAll("desk", "claude:a", 10)
		out <- result{texts, err}
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go read(first)
	<-firstAsked
	go read(second)
	// Wait until the second reader is where it will stay until the first is
	// done: queued behind it, or, without the queue, asking desk itself.
	for !secondAsked.Load() {
		laptop.mu.Lock()
		queued := 0
		for _, l := range laptop.copying {
			queued = l.waiting
		}
		laptop.mu.Unlock()
		if queued == 2 {
			break
		}
		runtime.Gosched()
	}
	close(letFirst)
	if r := <-first; r.err != nil || len(r.texts) != 3 {
		t.Fatalf("first reader: %v, %v", r.texts, r.err)
	}
	close(firstDone)
	if r := <-second; r.err != nil || len(r.texts) != 3 {
		t.Errorf("second reader: %v, %v", r.texts, r.err)
	}

	net.before, desk.online = nil, true
	if secondAsked.Load() {
		t.Error("the second reader made a copy of its own")
	}
	if rows := laptop.cachedRows(); rows != 3 {
		t.Errorf("the copy in use has %d messages, want 3", rows)
	}
	if texts, r, err := laptop.readAll("desk", "claude:a", 10); err != nil || len(texts) != 3 || r.From != "cache" {
		t.Errorf("reading afterwards: %v from %q, %v", texts, r.From, err)
	}
	if len(laptop.copying) != 0 {
		t.Errorf("%d copy locks were left behind", len(laptop.copying))
	}
}

// longSession gives desk a session that takes several answers to copy.
func longSession(desk *machine, key string) (texts []string) {
	for i := range 3 * PageBudget / MaxText {
		texts = append(texts, fmt.Sprintf("%d:%s", i, strings.Repeat("x", MaxText-8)))
	}
	texts = append(texts, "end")
	desk.src.put(key, "Long", 1000, texts...)
	return texts
}

// While a session is being copied, its node starts a new feed. Revisions of
// the old feed mean nothing in the new one, so the half-made copy is thrown
// away, not kept under a revision that would make it look current.
func TestCopyIsDroppedWhenTheFeedStartsOver(t *testing.T) {
	desk, laptop, net := pair(t)
	texts := longSession(desk, "claude:a")
	for i := range 40 { // the old feed gets far ahead of where the new one will start
		desk.src.put("claude:other", fmt.Sprintf("Edit %d", i), int64(i))
		desk.SyncLocal(ctx)
	}
	net.sync()

	asked := 0
	net.before = func(path string) {
		if !strings.HasPrefix(path, "/v1/sessions/messages") {
			return
		}
		if asked++; asked == 2 { // between the first and the second answer
			desk.Store.Close()
			if err := os.Remove(filepath.Join(desk.dir, "sessions.db")); err != nil {
				t.Fatal(err)
			}
			desk.open("d1", "desk")
			desk.SyncLocal(ctx)
			if err := laptop.Pull(ctx, Peer{ID: "d1", Name: "desk", Online: true, Addr: "d1"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, _, err := laptop.readAll("desk", "claude:a", 10)
	if !errors.Is(err, ErrFeedChanged) || !strings.Contains(err.Error(), "desk restarted its session feed") || Code(err) != CodeFeedRestarted {
		t.Fatalf("reading across the restart: err = %v, want ErrFeedChanged", err)
	}
	if rows := laptop.cachedRows(); rows != 0 {
		t.Errorf("%d messages of the abandoned copy were kept", rows)
	}

	net.before = nil
	first, err := laptop.Read(ctx, "desk", "claude:a", 0, 10)
	if err != nil || first.From != "peer" {
		t.Fatalf("reading after the restart: from %q, %v; want a new copy", first.From, err)
	}
	got, r, err := laptop.readAll("desk", "claude:a", 10)
	if err != nil || !slices.Equal(got, texts) || r.Stale || r.Incomplete {
		t.Errorf("reading after the restart: %d of %d messages, stale %v, incomplete %v, %v",
			len(got), len(texts), r.Stale, r.Incomplete, err)
	}
}

// The session is deleted on its machine while it is being copied here.
func TestCopyOfAVanishedSessionIsNotKept(t *testing.T) {
	desk, laptop, net := pair(t)
	longSession(desk, "claude:a")
	net.sync()

	asked := 0
	net.before = func(path string) {
		if strings.HasPrefix(path, "/v1/sessions/messages") {
			if asked++; asked == 3 {
				desk.src.remove("claude:a")
				desk.SyncLocal(ctx)
				if err := laptop.Pull(ctx, Peer{ID: "d1", Name: "desk", Online: true, Addr: "d1"}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if _, _, err := laptop.readAll("desk", "claude:a", 10); err == nil {
		t.Fatal("a session that was deleted meanwhile was read")
	}
	net.before = nil
	if rows := laptop.cachedRows(); rows != 0 {
		t.Errorf("%d messages of the deleted session were kept", rows)
	}
	var copies int
	if err := laptop.Store.db.QueryRow(`SELECT COUNT(*) FROM content`).Scan(&copies); err != nil || copies != 0 {
		t.Errorf("%d copies are on record (%v)", copies, err)
	}
}

// limits sets the bounds of a copy for one test.
func limits(t *testing.T, bytes, pages int) {
	oldBytes, oldPages := maxCopyBytes, maxCopyPages
	maxCopyBytes, maxCopyPages = bytes, pages
	t.Cleanup(func() { maxCopyBytes, maxCopyPages = oldBytes, oldPages })
}

// A copy stops at its size limit, and the limit counts whole messages, not
// just their text. A copy that was cut there is used as it is while the
// session stays the same.
func TestCopyStopsAtTheSizeLimit(t *testing.T) {
	desk, laptop, net := pair(t)
	empty := make([]string, 100) // messages without text still take room
	desk.src.put("claude:a", "A", 1000, empty...)
	net.sync()
	limits(t, 2000, maxCopyPages)

	got, r, err := laptop.readAll("desk", "claude:a", MaxPage)
	if err != nil || !r.Incomplete || r.From != "peer" {
		t.Fatalf("reading: %d messages, incomplete %v, from %q, %v", len(got), r.Incomplete, r.From, err)
	}
	if len(got) == 0 || len(got) >= 100 {
		t.Fatalf("the copy holds %d of 100 messages, want it cut at the limit", len(got))
	}
	stored := 0
	for _, m := range desk.src.messages["claude:a"][:len(got)] {
		stored += MessageSize(m)
	}
	if stored > maxCopyBytes {
		t.Errorf("the copy holds %d bytes, over the limit of %d", stored, maxCopyBytes)
	}

	// Reading again does not fetch again.
	net.requests = nil
	again, r, err := laptop.readAll("desk", "claude:a", MaxPage)
	if err != nil || len(again) != len(got) || !r.Incomplete || r.From != "cache" || net.messageRequests() != 0 {
		t.Errorf("second reading: %d messages, incomplete %v, from %q, %d requests, %v",
			len(again), r.Incomplete, r.From, net.messageRequests(), err)
	}
	// It does once the session has changed.
	desk.src.add("claude:a", "more")
	net.sync()
	net.requests = nil
	if _, r, _ = laptop.readAll("desk", "claude:a", MaxPage); r.From != "peer" || net.messageRequests() == 0 {
		t.Errorf("reading after a change: from %q, %d requests", r.From, net.messageRequests())
	}
}

// A copy also stops after a number of answers, however little is in them.
func TestCopyStopsAfterSoManyAnswers(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1000, "x")
	net.sync()
	limits(t, maxCopyBytes, 5)

	epoch, _ := desk.Store.Epoch()
	served := 0
	laptop.Fetch = func(_ context.Context, _ Peer, path string, out any) error {
		// A session that hands out one small message per answer, 50 times.
		served++
		raw, _ := json.Marshal(messagesPage{Epoch: epoch, Rev: 1, More: served < 50,
			Messages: []Message{{ID: int64(served), Seq: int64(served)}}})
		return json.Unmarshal(raw, out)
	}
	got, r, err := laptop.readAll("desk", "claude:a", MaxPage)
	if err != nil || served != 5 || len(got) != 5 || !r.Incomplete {
		t.Errorf("%d answers were fetched, %d messages kept, incomplete %v, %v; want it to stop at 5",
			served, len(got), r.Incomplete, err)
	}
}

// A page is cut while it is read, not afterwards: the index is not asked
// for hundreds of messages to send five.
func TestPageOfALongSessionReadsOnlyWhatItSends(t *testing.T) {
	desk, laptop, net := pair(t)
	texts := make([]string, 300)
	for i := range texts {
		texts[i] = strings.Repeat("y", MaxText/2)
	}
	desk.src.put("claude:a", "A", 1000, texts...)
	net.sync()

	desk.src.read = 0
	pg, err := desk.LocalMessages(ctx, "claude:a", 0, MaxPage)
	if err != nil || !pg.More || len(pg.Messages) == 0 || len(pg.Messages) > 2*PageBudget/(MaxText/2) {
		t.Fatalf("page: %d messages, more %v, %v", len(pg.Messages), pg.More, err)
	}
	if desk.src.read != len(pg.Messages) {
		t.Errorf("the index handed out %d messages for a page of %d", desk.src.read, len(pg.Messages))
	}
	if got, _, err := laptop.readAll("desk", "claude:a", MaxPage); err != nil || len(got) != len(texts) {
		t.Errorf("read %d of %d messages, %v", len(got), len(texts), err)
	}
}

// Whatever the index cannot do right now, the feeds of the peers are read.
func TestPeersAreReadWhenTheRefreshFails(t *testing.T) {
	desk, laptop, net := pair(t)
	laptop.src.put("claude:c", "On the laptop", 1)
	laptop.SyncLocal(ctx)
	desk.src.put("claude:a", "Indexed earlier", 2)
	desk.Refresh = func(context.Context) (string, error) {
		return "", errors.New("wake-cli refresh did not finish within 2m0s and was stopped")
	}
	desk.Round(ctx)
	want := []string{"desk/claude:a=Indexed earlier", "laptop/claude:c=On the laptop"}
	if got := titles(desk.list(Query{})); !slices.Equal(got, want) {
		t.Errorf("after a refresh that failed, desk lists %v", got)
	}
	_ = net
}

// A peer starts speaking another protocol, as after an update to the next
// major version. Its sessions are no longer read, but what is already here
// stays, marked, and what was read before can still be read.
func TestPeerThatMovedToAnotherProtocol(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:read", "Was read", 1000, "one", "two")
	desk.src.put("claude:unread", "Never read", 2000, "three")
	net.sync()
	if _, _, err := laptop.readAll("desk", "claude:read", 10); err != nil {
		t.Fatal(err)
	}

	desk.protocol = 2 // as laptop's hello now learns
	desk.src.put("claude:new", "After the update", 3000)
	net.requests = nil
	laptop.Round(ctx)
	if len(net.requests) != 0 {
		t.Errorf("laptop asked a node of another protocol: %v", net.requests)
	}

	l := laptop.list(Query{})
	if len(l.Sessions) != 2 || !l.Sessions[0].Incompatible || !l.Sessions[0].Online {
		t.Fatalf("laptop lists %+v, want the two sessions it had, marked", l.Sessions)
	}
	if n := l.Nodes[1]; !n.Incompatible || n.Protocol != 2 || !n.Online || n.Sessions != 2 || n.SyncedAt == nil {
		t.Errorf("desk in the listing = %+v", n)
	}
	if n := l.Nodes[0]; n.Incompatible || n.Protocol != 1 {
		t.Errorf("laptop itself in the listing = %+v", n)
	}

	texts, r, err := laptop.readAll("desk", "claude:read", 10)
	if err != nil || len(texts) != 2 || r.From != "cache" || len(net.requests) != 0 {
		t.Errorf("reading the copy: %v from %q, requests %v, %v", texts, r.From, net.requests, err)
	}
	_, _, err = laptop.readAll("desk", "claude:unread", 10)
	var notCached *NotCachedError
	if !errors.As(err, &notCached) || !strings.Contains(err.Error(), "desk speaks protocol 2 while this node speaks 1") || Code(err) != CodeIncompatible {
		t.Errorf("reading what was never read: %v", err)
	}
}

// A listing is continued with the "next" of the page before. Walking it to
// the end shows every session once, in order, whatever the page size, also
// where many sessions share an update time.
func TestListingContinuesWithoutGapsOrRepeats(t *testing.T) {
	desk, laptop, net := pair(t)
	const total = 257
	for i := range total {
		// Update times repeat, so the node and the key have to break ties.
		desk.src.put(fmt.Sprintf("claude:%04d", i), fmt.Sprintf("Session %d", i), int64(1000+i/10))
		if i%3 == 0 {
			laptop.src.put(fmt.Sprintf("codex:%04d", i), fmt.Sprintf("Laptop %d", i), int64(1000+i/10))
		}
	}
	net.sync()
	all := total + (total+2)/3

	for _, size := range []int{1, 7, 50, MaxPage} {
		seen := make(map[string]bool)
		var order []Entry
		q := Query{Limit: size}
		for pages := 0; ; pages++ {
			if pages > all {
				t.Fatalf("page size %d: the listing does not end", size)
			}
			l := laptop.list(q)
			if len(l.Sessions) > size {
				t.Fatalf("page size %d: a page of %d", size, len(l.Sessions))
			}
			for _, s := range l.Sessions {
				if seen[s.Ref] {
					t.Fatalf("page size %d: %s is listed twice", size, s.Ref)
				}
				seen[s.Ref] = true
				order = append(order, s)
			}
			if l.Next == "" {
				break
			}
			if len(l.Sessions) != size {
				t.Fatalf("page size %d: a page of %d that says there is more", size, len(l.Sessions))
			}
			q.After = l.Next
		}
		if len(seen) != all {
			t.Errorf("page size %d: %d sessions listed, want %d", size, len(seen), all)
		}
		for i := 1; i < len(order); i++ {
			if order[i].UpdatedAt.After(order[i-1].UpdatedAt) {
				t.Fatalf("page size %d: not newest first at %d", size, i)
			}
		}
	}

	// A filter applies to every page of its listing.
	seen := 0
	q := Query{Node: "laptop", Text: []string{"Laptop"}, Limit: 20}
	for {
		l := laptop.list(q)
		for _, s := range l.Sessions {
			if s.Node != "laptop" {
				t.Fatalf("the filter was lost on a later page: %s", s.Ref)
			}
			seen++
		}
		if q.After = l.Next; q.After == "" {
			break
		}
	}
	if seen != (total+2)/3 {
		t.Errorf("filtered listing: %d sessions, want %d", seen, (total+2)/3)
	}

	if _, err := laptop.List(ctx, Query{After: "not-a-position"}); err == nil {
		t.Error("a made-up position was accepted")
	}
	if l := laptop.list(Query{Limit: MaxPage, Since: 99999}); l.Next != "" || len(l.Sessions) != 0 {
		t.Errorf("an empty listing says there is more: %+v", l)
	}
}

// The listing says, for this machine, why its sessions are not shared.
func TestListingSaysWhyThisMachineSharesNothing(t *testing.T) {
	desk, laptop, net := pair(t)
	desk.src.put("claude:a", "A", 1)
	net.sync()
	note := func(m *machine) string { return m.list(Query{}).Nodes[0].Note }
	if note(desk) != "" || note(laptop) != "" {
		t.Fatalf("notes while both share: %q, %q", note(desk), note(laptop))
	}

	desk.src.err = fmt.Errorf("%w: no Wake index at /home/alice/.local/share/wake/wake.db", ErrNoIndex)
	net.sync()
	if got := note(desk); !strings.Contains(got, "no Wake index at") {
		t.Errorf("note = %q", got)
	}
	if got := laptop.list(Query{}).Nodes[1].Note; got != "" {
		t.Errorf("laptop is told about desk's index: %q", got)
	}
	desk.src.err = nil
	net.sync()
	if got := note(desk); got != "" {
		t.Errorf("note after the index came back = %q", got)
	}

	laptop.Source = nil
	if got := note(laptop); got != ErrNoIndex.Error() {
		t.Errorf("note of a machine without an index = %q", got)
	}
}

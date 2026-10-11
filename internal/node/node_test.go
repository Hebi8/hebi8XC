package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/control"
	"github.com/Hebi8/hebi8XC/internal/discovery"
	"github.com/Hebi8/hebi8XC/internal/identity"
	"github.com/Hebi8/hebi8XC/internal/link"
	"github.com/Hebi8/hebi8XC/internal/peers"
	"github.com/Hebi8/hebi8XC/internal/roster"
)

// clock is the only source of time in these tests: nothing sleeps, rounds
// are run by hand and time moves when a test says so.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testNode is a node on loopback with its own data directory. static is
// its list of manually entered addresses and may change between rounds.
type testNode struct {
	*Node
	t      *testing.T
	dir    string
	clock  *clock
	addr   string
	id     *identity.Identity
	roster *roster.File
	static []string
	extra  []discovery.Source
}

func startNode(t *testing.T, c *clock, extra ...discovery.Source) *testNode {
	t.Helper()
	tn := &testNode{t: t, dir: t.TempDir(), clock: c, extra: extra}
	tn.boot("127.0.0.1:0")
	return tn
}

// boot starts the node from whatever its data directory holds.
func (tn *testNode) boot(listen string) {
	t := tn.t
	t.Helper()
	var err error
	if tn.id, err = identity.Load(tn.dir); err != nil {
		t.Fatal(err)
	}
	if tn.roster, err = roster.OpenFile(filepath.Join(tn.dir, RosterFile)); err != nil {
		t.Fatal(err)
	}
	table, err := peers.Load(filepath.Join(tn.dir, PeersFile), tn.clock.now, OfflineAfter)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := control.Listen(filepath.Join(tn.dir, ControlSocket))
	if err != nil {
		t.Fatal(err)
	}
	tn.addr = ln.Addr().String()
	tn.Node, err = New(Config{
		Identity: tn.id,
		User:     "me",
		Roster:   tn.roster,
		Table:    table,
		Sources: append([]discovery.Source{
			discovery.Func(discovery.Remembered, table.Addrs),
			discovery.Func(discovery.Static, func() []string { return tn.static }),
		}, tn.extra...),
		Listener: ln,
		Control:  ctl,
		Now:      tn.clock.now,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	tn.Start()
	t.Cleanup(tn.Close)
}

func (tn *testNode) restart() {
	tn.Close()
	tn.boot(tn.addr)
}

func (tn *testNode) trust(other *testNode) {
	tn.t.Helper()
	if _, err := tn.roster.Add(other.id.Public(), other.id.Name); err != nil {
		tn.t.Fatal(err)
	}
}

func (tn *testNode) round() { tn.Round(context.Background()) }

// peers asks the node the way the CLI does, through the control socket.
func (tn *testNode) peers() []peers.Peer {
	tn.t.Helper()
	var list []peers.Peer
	err := control.Dial(filepath.Join(tn.dir, ControlSocket)).Get(context.Background(), "/v1/peers", &list)
	if err != nil {
		tn.t.Fatal(err)
	}
	return list
}

func (tn *testNode) peer(other *testNode) peers.Peer {
	tn.t.Helper()
	list := tn.peers()
	if len(list) != 1 || list[0].ID != other.id.ID {
		tn.t.Fatalf("peers = %+v, want exactly node %s", list, other.id.ID)
	}
	return list[0]
}

func pair(t *testing.T) (a, b *testNode, c *clock) {
	t.Helper()
	c = newClock()
	a, b = startNode(t, c), startNode(t, c)
	a.trust(b)
	b.trust(a)
	return a, b, c
}

// The end-to-end case: two nodes with separate data directories and ports,
// pointed at each other by hand, end up in each other's node list.
func TestTwoNodesFindEachOther(t *testing.T) {
	a, b, _ := pair(t)
	a.static = []string{b.addr}
	b.static = []string{a.addr}
	a.round()
	b.round()

	for _, tc := range []struct{ self, other *testNode }{{a, b}, {b, a}} {
		p := tc.self.peer(tc.other)
		if !p.Online || p.Name != tc.other.id.Name {
			t.Errorf("peer = %+v, want online and named %s", p, tc.other.id.Name)
		}
		if len(p.Addrs) != 1 || p.Addrs[0].Addr != tc.other.addr {
			t.Errorf("addrs = %+v, want %s", p.Addrs, tc.other.addr)
		}
		if !slices.Contains(p.Sources, discovery.Static) {
			t.Errorf("sources = %v, want static", p.Sources)
		}
	}

	var st Status
	if err := control.Dial(filepath.Join(a.dir, ControlSocket)).Get(context.Background(), "/v1/status", &st); err != nil {
		t.Fatal(err)
	}
	if st.ID != a.id.ID || st.Listen != a.addr || st.NetworkTag != discovery.NetworkTag("me") {
		t.Errorf("status = %+v", st)
	}
}

// Only a knows where b is. b learns about a from a's hello and finds its
// address by calling back.
func TestOneAddressIsEnoughForBothSides(t *testing.T) {
	a, b, _ := pair(t)
	a.static = []string{b.addr}
	a.round()

	p := b.peer(a)
	if !p.Online || len(p.Addrs) != 0 || !slices.Equal(p.Sources, []string{discovery.Inbound}) {
		t.Fatalf("after a's hello, b has %+v; want a online via inbound with no address yet", p)
	}

	b.round()
	p = b.peer(a)
	if len(p.Addrs) != 1 || p.Addrs[0].Addr != a.addr {
		t.Errorf("b did not learn a's address by calling back: %+v", p.Addrs)
	}
}

func TestStrangersDoNotBecomePeers(t *testing.T) {
	c := newClock()
	a, b := startNode(t, c), startNode(t, c)
	a.static = []string{b.addr}
	b.static = []string{a.addr}
	a.round()
	b.round()
	if len(a.peers()) != 0 || len(b.peers()) != 0 {
		t.Errorf("nodes without each other's key became peers: %+v / %+v", a.peers(), b.peers())
	}
}

// a trusts b but b does not trust a: neither side lists the other, until b
// adds a's key. No restart is needed; the next round reads the roster again.
func TestOneWayTrustIsNotEnough(t *testing.T) {
	c := newClock()
	a, b := startNode(t, c), startNode(t, c)
	a.trust(b)
	a.static = []string{b.addr}
	b.static = []string{a.addr}
	a.round()
	b.round()
	if len(a.peers()) != 0 || len(b.peers()) != 0 {
		t.Fatalf("one-way trust produced peers: %+v / %+v", a.peers(), b.peers())
	}

	b.trust(a)
	a.round()
	b.round()
	if !a.peer(b).Online || !b.peer(a).Online {
		t.Error("nodes did not connect after trust became mutual")
	}
}

func TestPeerGoesOfflineAndRecovers(t *testing.T) {
	a, b, c := pair(t)
	a.static = []string{b.addr}
	a.round()
	seen := c.now()

	b.Close()
	c.advance(OfflineAfter)
	a.round()
	if !a.peer(b).Online {
		t.Fatal("peer went offline before OfflineAfter had passed")
	}
	c.advance(time.Second)
	a.round()
	p := a.peer(b)
	if p.Online || !p.LastSeen.Equal(seen) || len(p.Addrs) != 1 {
		t.Fatalf("want b offline but kept, last seen %v: %+v", seen, p)
	}

	c.advance(time.Hour)
	b.boot(b.addr)
	a.round()
	p = a.peer(b)
	if !p.Online || !p.LastSeen.Equal(c.now()) {
		t.Errorf("b did not come back: %+v", p)
	}
}

// After a restart the node list is still there, and the remembered address
// alone is enough to reconnect.
func TestRestartReconnectsFromRememberedAddress(t *testing.T) {
	a, b, _ := pair(t)
	a.static = []string{b.addr}
	a.round()
	idBefore := a.id.ID

	a.static = nil
	a.restart()
	if a.id.ID != idBefore {
		t.Fatal("node id changed across restart")
	}
	p := a.peer(b)
	if p.Online {
		t.Fatal("peer is online right after restart, before any contact")
	}

	a.round()
	p = a.peer(b)
	if !p.Online || !slices.Equal(p.Addrs[0].Sources, []string{discovery.Static}) {
		t.Errorf("want b online again with its address still attributed to static: %+v", p)
	}
}

func TestNodeIgnoresItself(t *testing.T) {
	a := startNode(t, newClock())
	if _, err := a.roster.Add(a.id.Public(), "self"); err != nil {
		t.Fatal(err)
	}
	a.static = []string{a.addr}
	a.round()
	if list := a.peers(); len(list) != 0 {
		t.Errorf("node listed itself: %+v", list)
	}
}

// stuckSource holds a round inside source collection until released.
type stuckSource struct {
	entered chan struct{}
	release chan struct{}
}

func (s *stuckSource) Name() string { return "stuck" }

func (s *stuckSource) Candidates(context.Context) ([]discovery.Candidate, error) {
	s.entered <- struct{}{}
	<-s.release
	return nil, nil
}

// h8c trust remove must cut a key off by the time it returns, even while
// the node is in the middle of a round that will not finish soon.
func TestRemovedKeyIsRefusedBeforeTheRoundEnds(t *testing.T) {
	c := newClock()
	stuck := &stuckSource{entered: make(chan struct{}), release: make(chan struct{})}
	a, b := startNode(t, c, stuck), startNode(t, c)
	a.trust(b)
	b.trust(a)
	ctx := context.Background()
	if _, _, err := b.hello(ctx, a.addr); err != nil {
		t.Fatalf("hello while trusted: %v", err)
	}

	roundDone := make(chan struct{})
	go func() {
		a.round()
		close(roundDone)
	}()
	<-stuck.entered // a has refreshed its roster and now waits on the source

	// What "h8c trust remove" does: change the file through its own handle,
	// then tell the running node.
	cli, err := roster.OpenFile(filepath.Join(a.dir, RosterFile))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := cli.Remove(b.id.Name); err != nil || n != 1 {
		t.Fatalf("Remove = %d, %v", n, err)
	}
	if err := control.Dial(filepath.Join(a.dir, ControlSocket)).Post(ctx, "/v1/refresh"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := b.hello(ctx, a.addr); !errors.Is(err, link.ErrRejected) {
		t.Errorf("hello after removal: err = %v, want ErrRejected", err)
	}

	close(stuck.release)
	<-roundDone
}

// deadEnd is an address that accepts connections and hangs up, like a
// machine that runs something else on the port. It counts the attempts.
type deadEnd struct {
	ln    net.Listener
	count atomic.Int32
}

func newDeadEnd(t *testing.T) *deadEnd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &deadEnd{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Count before hanging up: the prober only sees the failure
			// after the close, so its round ends after the count is in.
			d.count.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return d
}

func TestSilentAddressIsProbedLessAndLess(t *testing.T) {
	c := newClock()
	a := startNode(t, c)
	dead := newDeadEnd(t)
	a.static = []string{dead.ln.Addr().String()}

	// One round every Interval, as Run does. An address that never answers
	// is probed after 0, 20, 60, 140, 300 seconds and then every 5 minutes.
	var probedAt []time.Duration
	start := c.now()
	for c.now().Sub(start) <= 15*time.Minute {
		before := dead.count.Load()
		a.round()
		if dead.count.Load() != before {
			probedAt = append(probedAt, c.now().Sub(start))
		}
		c.advance(Interval)
	}
	want := []time.Duration{0, 20 * time.Second, 60 * time.Second, 140 * time.Second,
		300 * time.Second, 600 * time.Second, 900 * time.Second}
	if !slices.Equal(probedAt, want) {
		t.Errorf("probed at %v, want %v", probedAt, want)
	}
}

// Once a backed-off address answers, it is back on the normal beat.
func TestAnsweringAddressReturnsToTheNormalPace(t *testing.T) {
	a, b, c := pair(t)
	a.static = []string{b.addr}
	a.round()

	b.Close()
	for range 6 { // fail at 10s and 30s; the next probe is due at 70s
		c.advance(Interval)
		a.round()
	}
	if a.peer(b).Online {
		t.Fatal("b should be offline by now")
	}

	b.boot(b.addr)
	a.round() // 60s: still backing off
	if a.peer(b).Online {
		t.Fatal("a probed b before the backoff was over")
	}
	c.advance(Interval)
	a.round() // 70s: due, and b answers
	if !a.peer(b).Online {
		t.Fatal("b did not come back at the first probe that was due")
	}
	for range 3 {
		c.advance(Interval)
		a.round()
		if p := a.peer(b); !p.LastSeen.Equal(c.now()) {
			t.Fatalf("after recovery b was not probed in the round at %v (last seen %v)", c.now(), p.LastSeen)
		}
	}
}

// A peer that comes back and calls us is probed at once, whatever backoff
// its address had built up.
func TestHelloFromAPeerEndsItsBackoff(t *testing.T) {
	a, b, c := pair(t)
	a.static = []string{b.addr}
	a.round()

	b.Close()
	for range 6 {
		c.advance(Interval)
		a.round()
	}
	b.boot(b.addr)
	b.static = []string{a.addr}
	b.round() // b says hello; a learns b is up

	a.round() // 60s: would still be backing off
	if p := a.peer(b); !p.Online || len(p.Addrs) != 1 || !p.Addrs[0].LastOK.Equal(c.now()) {
		t.Errorf("a did not probe b right after b's hello: %+v", p)
	}
}

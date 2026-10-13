// Package node runs one node: it serves the peer link and the local control
// socket, and in every discovery round it probes the candidates its sources
// suggest and keeps the peer table current.
package node

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Hebi8/hebi8XC/internal/buildinfo"
	"github.com/Hebi8/hebi8XC/internal/discovery"
	"github.com/Hebi8/hebi8XC/internal/identity"
	"github.com/Hebi8/hebi8XC/internal/link"
	"github.com/Hebi8/hebi8XC/internal/peers"
	"github.com/Hebi8/hebi8XC/internal/roster"
	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

// Protocol is the major version of what nodes say to each other: the hello,
// the session feed and the reading of messages. Two builds with the same
// Protocol work together, whatever their age. Within a version, fields are
// only ever added, and a node does without a field its peer does not send.
// Anything else is a new version. See "Protocol compatibility" in
// docs/design.md.
const Protocol = 1

const (
	DefaultPort = 4892

	// Interval is the time between discovery rounds. Every round is also a
	// heartbeat to the peers that are already known.
	Interval = 10 * time.Second
	// OfflineAfter is how long a peer may stay silent before it is offline.
	OfflineAfter = 30 * time.Second

	probeTimeout = 4 * time.Second
	// maxProbes bounds the connections one round has open at a time.
	maxProbes = 16
	// maxBackoff is the longest gap between two probes of an address that
	// does not answer. The gap doubles from Interval with every failure.
	maxBackoff = 5 * time.Minute

	// firstRemoteWait is how long the first round waits for the first fetch
	// of a remote roster that has nothing cached.
	firstRemoteWait = 10 * time.Second

	// RemoteInterval is the time between refreshes of the remote roster.
	// Reading it costs two requests out of the 60 per hour that GitHub
	// allows an IP address without a token, and all nodes behind one
	// router share that allowance.
	RemoteInterval = 15 * time.Minute

	// Files in the data directory, next to the identity.
	RosterFile       = "trusted_keys"
	AccountFile      = "account.json"
	GitHubRosterFile = "github_keys.json"
	PeersFile        = "peers.json"
	SessionsFile     = "sessions.db"
	ControlSocket    = "control.sock"
)

// ErrNoRemote is the answer to a remote roster refresh on a node that has
// no remote roster.
var ErrNoRemote = errors.New("this node is not logged in to GitHub")

// Info is what two nodes tell each other in POST /v1/hello. Port is where
// the sender listens, so the receiver can call back. Protocol and Version
// are missing in the hello of a build from before they were added; such a
// build speaks protocol 1.
type Info struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Protocol int    `json:"protocol,omitempty"`
	Version  string `json:"version,omitempty"`
}

// Status is the answer of GET /v1/status on the control socket.
type Status struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Key        string `json:"key"`
	User       string `json:"user"`
	NetworkTag string `json:"network_tag"`
	Listen     string `json:"listen"`
	Protocol   int    `json:"protocol"`
	Version    string `json:"version"`
}

type Config struct {
	Identity *identity.Identity
	User     string
	// Local is re-read every round; it is a file on this machine. Remote
	// is fetched over the network every RemoteInterval and may be nil.
	// A key in either one is trusted.
	Local   roster.Roster
	Remote  roster.Roster
	Table   *peers.Table
	Sources []discovery.Source
	// Sessions is the session module. The node gives it the peers and the
	// link, serves its routes and runs its rounds. Nil leaves it out.
	Sessions *sessions.Service
	Listener net.Listener // peer link, plain TCP; the node adds TLS
	Control  net.Listener // control socket
	Now      func() time.Time
	Log      *slog.Logger
}

type Node struct {
	cfg     Config
	log     *slog.Logger
	client  *link.Client
	peerSrv *http.Server
	ctlSrv  *http.Server
	kick    chan struct{}
	after   func(time.Duration) <-chan time.Time // time.After, except in tests

	sessionsKick chan struct{}   // a peer can be read: read its feed soon
	readable     map[string]bool // peers that were online with an address after the last round

	mu      sync.Mutex
	inbound map[string]string // node id -> address to call back
	retries map[string]retry  // addresses that did not answer
	foreign map[string]int    // peers that speak another protocol, and which

	// reported remembers the last trust problem logged per address, so a
	// problem is logged once, not every round. Only Round touches it.
	reported map[string]string
}

// retry is the backoff state of an address that does not answer.
type retry struct {
	failures int
	next     time.Time // do not probe before this
}

func New(cfg Config) (*Node, error) {
	n := &Node{
		cfg:   cfg,
		log:   cfg.Log,
		kick:  make(chan struct{}, 1),
		after: time.After,

		sessionsKick: make(chan struct{}, 1),
		readable:     make(map[string]bool),
		inbound:      make(map[string]string),
		retries:      make(map[string]retry),
		foreign:      make(map[string]int),
		reported:     make(map[string]string),
	}
	trusted := cfg.Local
	if cfg.Remote != nil {
		trusted = roster.Union(cfg.Local, cfg.Remote)
	}
	auth := &link.Auth{Key: cfg.Identity.Key, Roster: trusted, User: cfg.User}
	var err error
	if n.client, err = link.NewClient(auth); err != nil {
		return nil, err
	}

	// Services offered to other nodes are routes on this mux.
	peerMux := http.NewServeMux()
	peerMux.HandleFunc("POST /v1/hello", n.handleHello)
	if n.peerSrv, err = link.NewServer(auth, peerMux); err != nil {
		return nil, err
	}

	ctlMux := http.NewServeMux()
	ctlMux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(n.status())
	})
	ctlMux.HandleFunc("GET /v1/peers", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(cfg.Table.List())
	})
	// The roster is reloaded before the answer is sent, so a key removed by
	// the caller is refused from this point on. The round that follows only
	// brings the peer table up to date.
	ctlMux.HandleFunc("POST /v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		if err := cfg.Local.Refresh(r.Context(), cfg.User); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		n.roundNow()
	})
	// The same for the remote roster: the answer is sent when the fetch is
	// over, and says so if it failed.
	ctlMux.HandleFunc("POST /v1/roster/refresh", func(w http.ResponseWriter, r *http.Request) {
		if err := n.RefreshRemote(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		n.roundNow()
	})
	if svc := cfg.Sessions; svc != nil {
		svc.Protocol = Protocol
		svc.Peers, svc.Fetch = n.sessionPeers, n.fetchFromPeer
		svc.PeerRoutes(peerMux)
		svc.ControlRoutes(ctlMux)
	}
	n.ctlSrv = &http.Server{Handler: ctlMux}
	return n, nil
}

// Sessions returns the session module, nil if the node has none.
func (n *Node) Sessions() *sessions.Service { return n.cfg.Sessions }

// sessionPeers lists the peers for the session module, each with the
// address it answered on last.
func (n *Node) sessionPeers() []sessions.Peer {
	list := n.cfg.Table.List()
	out := make([]sessions.Peer, len(list))
	for i, p := range list {
		out[i] = sessions.Peer{ID: p.ID, Name: p.Name, Online: p.Online, Protocol: p.Speaks(), Version: p.Version}
		var last time.Time
		for _, a := range p.Addrs {
			if out[i].Addr == "" || a.LastOK.After(last) {
				out[i].Addr, last = a.Addr, a.LastOK
			}
		}
	}
	return out
}

// sessionFetchTimeout bounds one request of the session module to a peer.
const sessionFetchTimeout = 30 * time.Second

// fetchFromPeer gets a page from a peer over the link. An address can pass
// from one node to another; the answer only counts if it comes from the
// key the peer is known by.
func (n *Node) fetchFromPeer(ctx context.Context, p sessions.Peer, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, sessionFetchTimeout)
	defer cancel()
	key, err := n.client.Get(ctx, p.Addr, path, out, sessions.MaxReply)
	if err != nil {
		return err
	}
	for _, known := range n.cfg.Table.List() {
		if known.ID == p.ID && known.Key == sshkey.Marshal(key) {
			return nil
		}
	}
	return fmt.Errorf("%s is now another node than %s", p.Addr, p.Name)
}

// noteReadable starts a round of the session module when a peer has become
// readable: online, at an address this node has reached. A peer that called
// first is online before that; its address is only known a round later.
func (n *Node) noteReadable() {
	if n.cfg.Sessions == nil {
		return
	}
	now := make(map[string]bool)
	fresh := false
	for _, p := range n.sessionPeers() {
		if p.Online && p.Addr != "" {
			now[p.ID] = true
			fresh = fresh || !n.readable[p.ID]
		}
	}
	n.readable = now
	if fresh {
		select {
		case n.sessionsKick <- struct{}{}:
		default:
		}
	}
}

// sessionsLoop runs the rounds of the session module: at start, every
// sessions.Interval, and as soon as a peer becomes readable.
func (n *Node) sessionsLoop(ctx context.Context) {
	ticker := time.NewTicker(sessions.Interval)
	defer ticker.Stop()
	for {
		n.cfg.Sessions.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.sessionsKick:
		}
	}
}

// Start begins serving both listeners in the background.
func (n *Node) Start() {
	go func() {
		if err := n.peerSrv.ServeTLS(n.cfg.Listener, "", ""); !errors.Is(err, http.ErrServerClosed) {
			n.log.Error("peer link stopped", "err", err)
		}
	}()
	go func() {
		if err := n.ctlSrv.Serve(n.cfg.Control); !errors.Is(err, http.ErrServerClosed) {
			n.log.Error("control socket stopped", "err", err)
		}
	}()
}

// Close stops both servers and saves the peer table.
func (n *Node) Close() {
	n.peerSrv.Close()
	n.ctlSrv.Close()
	if err := n.cfg.Table.Save(true); err != nil {
		n.log.Error("saving peers", "err", err)
	}
}

// Run serves, keeps the remote roster fresh and runs discovery rounds until
// ctx is done.
func (n *Node) Run(ctx context.Context) {
	n.Start()
	defer n.Close()
	n.run(ctx)
}

func (n *Node) run(ctx context.Context) {
	if n.cfg.Remote != nil {
		attempted := make(chan struct{})
		go n.keepRemoteFresh(ctx, attempted)
		n.awaitFirstRoster(ctx, attempted)
	}
	if n.cfg.Sessions != nil {
		go n.sessionsLoop(ctx)
	}
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		n.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.kick:
		}
	}
}

// awaitFirstRoster holds the first round back while the remote roster has
// no keys cached and its first fetch is still under way. On a node's very
// first start every other node of the account would otherwise fail the
// trust check and be reported as a stranger, moments before the fetch
// shows that it is not. A node with cached keys does not wait, and nobody
// waits longer than firstRemoteWait.
func (n *Node) awaitFirstRoster(ctx context.Context, attempted <-chan struct{}) {
	if keys, _ := n.cfg.Remote.Keys(n.cfg.User); len(keys) > 0 {
		return
	}
	select {
	case <-attempted:
	case <-n.after(firstRemoteWait):
	case <-ctx.Done():
	}
}

// keepRemoteFresh refreshes the remote roster at start and then every
// RemoteInterval, and closes attempted when the first try is over, however
// it went. It runs beside the rounds, so a slow or unreachable GitHub never
// delays a heartbeat. Whenever a refresh fails, the roster's cache is what
// counts.
func (n *Node) keepRemoteFresh(ctx context.Context, attempted chan<- struct{}) {
	ticker := time.NewTicker(RemoteInterval)
	defer ticker.Stop()
	for first := true; ; first = false {
		err := n.RefreshRemote(ctx)
		if first {
			close(attempted)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.log.Warn(err.Error())
		} else {
			n.roundNow()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RefreshRemote fetches the remote roster now and returns when that is
// done. Keys it no longer lists are refused from then on.
func (n *Node) RefreshRemote(ctx context.Context) error {
	if n.cfg.Remote == nil {
		return ErrNoRemote
	}
	return n.cfg.Remote.Refresh(ctx, n.cfg.User)
}

// roundNow asks Run for a round without waiting for the next tick.
func (n *Node) roundNow() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// Round re-reads the local roster, probes the candidate addresses that are due,
// records the nodes that answered and marks the silent ones offline. It must
// not be called concurrently with itself.
func (n *Node) Round(ctx context.Context) {
	if err := n.cfg.Local.Refresh(ctx, n.cfg.User); err != nil {
		n.log.Warn("local roster not re-read, using the keys already loaded", "err", err)
	}

	targets := n.targets(ctx)
	now := n.cfg.Now()
	type result struct {
		peer Info
		key  ed25519.PublicKey
		err  error
	}
	results := make(map[string]*result, len(targets))
	n.mu.Lock()
	for addr := range n.retries {
		if _, ok := targets[addr]; !ok {
			delete(n.retries, addr)
		}
	}
	for addr := range targets {
		if !now.Before(n.retries[addr].next) {
			results[addr] = &result{}
		}
	}
	n.mu.Unlock()
	for addr := range n.reported {
		if _, ok := targets[addr]; !ok {
			delete(n.reported, addr)
		}
	}

	var wg sync.WaitGroup
	slots := make(chan struct{}, maxProbes)
	for addr, res := range results {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			res.peer, res.key, res.err = n.hello(ctx, addr)
		})
	}
	wg.Wait()

	for addr, res := range results {
		n.settle(addr, res.err, now)
		if res.err != nil {
			continue
		}
		if res.peer.ID != n.cfg.Identity.ID {
			n.observe(res.peer, res.key, addr, targets[addr], addr)
		}
	}

	for _, p := range n.cfg.Table.Sweep() {
		n.log.Info("peer offline", "name", p.Name, "id", p.ID)
	}
	n.noteReadable()
	if err := n.cfg.Table.Save(false); err != nil {
		n.log.Error("saving peers", "err", err)
	}
}

// settle updates what is remembered about addr after a probe. A node that
// answered, even to refuse us, is probed again next round. An address that
// did not answer is left alone for a while, longer after every failure.
// The two trust failures are the ones the user can fix, so they are logged,
// once per address.
func (n *Node) settle(addr string, err error, now time.Time) {
	var problem string
	var untrusted *link.UntrustedError
	switch {
	case err == nil:
	case errors.As(err, &untrusted):
		// A candidate that turned out to be this node is not a problem.
		if !untrusted.Key.Equal(n.cfg.Identity.Public()) {
			problem = "found a node whose key is not in the roster; if h8c id on that machine shows this key, run: h8c trust add " +
				sshkey.Marshal(untrusted.Key)
		}
	case errors.Is(err, link.ErrRejected):
		problem = "a node in the roster refused this node; add this node's key (h8c id) to its roster"
	default:
		n.log.Debug("no answer", "addr", addr, "err", err)
		n.mu.Lock()
		r := n.retries[addr]
		if Interval<<r.failures < maxBackoff {
			r.failures++
		}
		// Half an interval of slack, so the round that is due is not
		// skipped because it started a moment early.
		r.next = now.Add(min(Interval<<r.failures, maxBackoff) - Interval/2)
		n.retries[addr] = r
		n.mu.Unlock()
		return
	}

	n.mu.Lock()
	delete(n.retries, addr)
	n.mu.Unlock()
	if problem == "" {
		delete(n.reported, addr)
	} else if n.reported[addr] != problem {
		n.reported[addr] = problem
		n.log.Warn(problem, "addr", addr)
	}
}

// targets collects the candidates of all sources as address -> source names.
// A remembered address keeps the sources it was first found through, so
// "remembered" itself is not recorded.
func (n *Node) targets(ctx context.Context) map[string][]string {
	targets := make(map[string][]string)
	add := func(c discovery.Candidate) {
		sources := targets[c.Addr]
		if c.Source != discovery.Remembered && !slices.Contains(sources, c.Source) {
			sources = append(sources, c.Source)
		}
		targets[c.Addr] = sources
	}
	for _, src := range n.cfg.Sources {
		cands, err := src.Candidates(ctx)
		if err != nil {
			n.log.Debug("discovery source failed", "source", src.Name(), "err", err)
			continue
		}
		for _, c := range cands {
			add(c)
		}
	}
	n.mu.Lock()
	for _, addr := range n.inbound {
		add(discovery.Candidate{Addr: addr, Source: discovery.Inbound})
	}
	n.mu.Unlock()
	return targets
}

func (n *Node) hello(ctx context.Context, addr string) (Info, ed25519.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var peer Info
	key, err := n.client.Call(ctx, addr, http.MethodPost, "/v1/hello", n.info(), &peer)
	if err == nil && peer.ID == "" {
		err = errors.New("hello answer has no node id")
	}
	return peer, key, err
}

// handleHello answers a peer that reached us. The peer is authenticated, so
// it counts as seen right away; the address it can be called back on is
// probed in the next round like any other candidate.
func (n *Node) handleHello(w http.ResponseWriter, r *http.Request) {
	var peer Info
	if err := json.NewDecoder(r.Body).Decode(&peer); err != nil || peer.ID == "" {
		http.Error(w, "bad hello", http.StatusBadRequest)
		return
	}
	if peer.ID != n.cfg.Identity.ID {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		callback := net.JoinHostPort(host, strconv.Itoa(peer.Port))
		n.mu.Lock()
		n.inbound[peer.ID] = callback
		delete(n.retries, callback) // it is up: try it in the next round
		n.mu.Unlock()
		n.observe(peer, link.PeerKey(r), "", []string{discovery.Inbound}, host)
	}
	json.NewEncoder(w).Encode(n.info())
}

// observe records an authenticated contact. addr is the address we reached
// the peer on, empty if the peer reached us; remote is only for the log.
func (n *Node) observe(peer Info, key ed25519.PublicKey, addr string, sources []string, remote string) {
	version := peer.Version
	if len(version) > 64 {
		version = version[:64]
	}
	cameOnline := n.cfg.Table.Observe(peers.Contact{
		ID:       peer.ID,
		Name:     peer.Name,
		Key:      sshkey.Marshal(key),
		Addr:     addr,
		Sources:  sources,
		Protocol: peer.Protocol,
		Version:  version,
	})
	n.noteProtocol(peer)
	if cameOnline {
		n.log.Info("peer online", "name", peer.Name, "id", peer.ID, "remote", remote, "via", sources)
	}
}

// noteProtocol says, once per peer and protocol, that a peer speaks another
// protocol than this node. Such a peer is still found and still shown as
// online; what the two cannot do is use each other's services.
func (n *Node) noteProtocol(peer Info) {
	speaks := peers.Peer{Protocol: peer.Protocol}.Speaks()
	n.mu.Lock()
	defer n.mu.Unlock()
	if speaks == Protocol {
		delete(n.foreign, peer.ID)
		return
	}
	if n.foreign[peer.ID] == speaks {
		return
	}
	n.foreign[peer.ID] = speaks
	n.log.Warn(fmt.Sprintf("%s speaks protocol %d and this node speaks %d: sessions are not exchanged with it until both run builds of the same protocol",
		peer.Name, speaks, Protocol), "id", peer.ID, "version", peer.Version)
}

func (n *Node) port() int {
	return n.cfg.Listener.Addr().(*net.TCPAddr).Port
}

func (n *Node) info() Info {
	return Info{ID: n.cfg.Identity.ID, Name: n.cfg.Identity.Name, Port: n.port(),
		Protocol: Protocol, Version: buildinfo.String()}
}

func (n *Node) status() Status {
	return Status{
		ID:         n.cfg.Identity.ID,
		Name:       n.cfg.Identity.Name,
		Key:        sshkey.Marshal(n.cfg.Identity.Public()),
		User:       n.cfg.User,
		NetworkTag: discovery.NetworkTag(n.cfg.User),
		Listen:     n.cfg.Listener.Addr().String(),
		Protocol:   Protocol,
		Version:    buildinfo.String(),
	}
}

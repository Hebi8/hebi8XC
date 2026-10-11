// Package node runs one node: it serves the peer link and the local control
// socket, and in every discovery round it probes the candidates its sources
// suggest and keeps the peer table current.
package node

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"hebi8xc/internal/discovery"
	"hebi8xc/internal/identity"
	"hebi8xc/internal/link"
	"hebi8xc/internal/peers"
	"hebi8xc/internal/roster"
	"hebi8xc/internal/sshkey"
)

const (
	DefaultPort = 4892

	// Interval is the time between discovery rounds. Every round is also a
	// heartbeat to the peers that are already known.
	Interval = 10 * time.Second
	// OfflineAfter is how long a peer may stay silent before it is offline.
	OfflineAfter = 30 * time.Second

	probeTimeout = 4 * time.Second

	// Files in the data directory, next to the identity.
	RosterFile    = "trusted_keys"
	PeersFile     = "peers.json"
	ControlSocket = "control.sock"
)

// Info is what two nodes tell each other in POST /v1/hello. Port is where
// the sender listens, so the receiver can call back.
type Info struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"`
}

// Status is the answer of GET /v1/status on the control socket.
type Status struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Key        string `json:"key"`
	User       string `json:"user"`
	NetworkTag string `json:"network_tag"`
	Listen     string `json:"listen"`
}

type Config struct {
	Identity *identity.Identity
	User     string
	Roster   roster.Roster
	Table    *peers.Table
	Sources  []discovery.Source
	Listener net.Listener // peer link, plain TCP; the node adds TLS
	Control  net.Listener // control socket
	Log      *slog.Logger
}

type Node struct {
	cfg     Config
	log     *slog.Logger
	client  *link.Client
	peerSrv *http.Server
	ctlSrv  *http.Server
	kick    chan struct{}

	mu      sync.Mutex
	inbound map[string]string // node id -> address to call back

	// reported remembers the last trust problem logged per address, so a
	// problem is logged once, not every round. Only Round touches it.
	reported map[string]string
}

func New(cfg Config) (*Node, error) {
	n := &Node{
		cfg:      cfg,
		log:      cfg.Log,
		kick:     make(chan struct{}, 1),
		inbound:  make(map[string]string),
		reported: make(map[string]string),
	}
	auth := &link.Auth{Key: cfg.Identity.Key, Roster: cfg.Roster, User: cfg.User}
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
	ctlMux.HandleFunc("POST /v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		select {
		case n.kick <- struct{}{}:
		default:
		}
	})
	n.ctlSrv = &http.Server{Handler: ctlMux}
	return n, nil
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

// Run serves and runs discovery rounds until ctx is done.
func (n *Node) Run(ctx context.Context) {
	n.Start()
	defer n.Close()
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

// Round refreshes the roster, probes every candidate address once, records
// the nodes that answered and marks the silent ones offline. It must not be
// called concurrently with itself.
func (n *Node) Round(ctx context.Context) {
	if err := n.cfg.Roster.Refresh(ctx, n.cfg.User); err != nil {
		n.log.Warn("roster refresh failed, using the cached keys", "err", err)
	}

	targets := n.targets(ctx)
	type result struct {
		peer Info
		key  ed25519.PublicKey
		err  error
	}
	results := make(map[string]*result, len(targets))
	var wg sync.WaitGroup
	for addr := range targets {
		res := &result{}
		results[addr] = res
		wg.Go(func() {
			res.peer, res.key, res.err = n.hello(ctx, addr)
		})
	}
	wg.Wait()

	for addr, res := range results {
		if res.err != nil {
			n.reportFailure(addr, res.err)
			continue
		}
		delete(n.reported, addr)
		if res.peer.ID != n.cfg.Identity.ID {
			n.observe(res.peer, res.key, addr, targets[addr], addr)
		}
	}

	for _, p := range n.cfg.Table.Sweep() {
		n.log.Info("peer offline", "name", p.Name, "id", p.ID)
	}
	if err := n.cfg.Table.Save(false); err != nil {
		n.log.Error("saving peers", "err", err)
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
		n.mu.Lock()
		n.inbound[peer.ID] = net.JoinHostPort(host, strconv.Itoa(peer.Port))
		n.mu.Unlock()
		n.observe(peer, link.PeerKey(r), "", []string{discovery.Inbound}, host)
	}
	json.NewEncoder(w).Encode(n.info())
}

// observe records an authenticated contact. addr is the address we reached
// the peer on, empty if the peer reached us; remote is only for the log.
func (n *Node) observe(peer Info, key ed25519.PublicKey, addr string, sources []string, remote string) {
	cameOnline := n.cfg.Table.Observe(peers.Contact{
		ID:      peer.ID,
		Name:    peer.Name,
		Key:     sshkey.Marshal(key),
		Addr:    addr,
		Sources: sources,
	})
	if cameOnline {
		n.log.Info("peer online", "name", peer.Name, "id", peer.ID, "remote", remote, "via", sources)
	}
}

// reportFailure logs the two failures the user can fix. An address that
// simply does not answer is normal for most candidates and stays quiet.
func (n *Node) reportFailure(addr string, err error) {
	var problem string
	var untrusted *link.UntrustedError
	switch {
	case errors.As(err, &untrusted):
		if untrusted.Key.Equal(n.cfg.Identity.Public()) {
			return // a candidate that turned out to be this node
		}
		problem = "found a node whose key is not in the roster; if h8c id on that machine shows this key, run: h8c trust add " +
			sshkey.Marshal(untrusted.Key)
	case errors.Is(err, link.ErrRejected):
		problem = "a node in the roster refused this node; add this node's key (h8c id) to its roster"
	default:
		n.log.Debug("no answer", "addr", addr, "err", err)
		return
	}
	if n.reported[addr] != problem {
		n.reported[addr] = problem
		n.log.Warn(problem, "addr", addr)
	}
}

func (n *Node) port() int {
	return n.cfg.Listener.Addr().(*net.TCPAddr).Port
}

func (n *Node) info() Info {
	return Info{ID: n.cfg.Identity.ID, Name: n.cfg.Identity.Name, Port: n.port()}
}

func (n *Node) status() Status {
	return Status{
		ID:         n.cfg.Identity.ID,
		Name:       n.cfg.Identity.Name,
		Key:        sshkey.Marshal(n.cfg.Identity.Public()),
		User:       n.cfg.User,
		NetworkTag: discovery.NetworkTag(n.cfg.User),
		Listen:     n.cfg.Listener.Addr().String(),
	}
}

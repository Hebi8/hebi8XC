package discovery

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

const (
	mdnsService = "_hebi8xc._tcp"
	mdnsDomain  = "local."

	// The browser restarts every browseWindow so that every node on the link
	// answers again; a node that stayed silent for mdnsExpiry is forgotten.
	browseWindow = 30 * time.Second
	mdnsExpiry   = 75 * time.Second

	// maxMDNSEntries bounds how many announced nodes are remembered.
	// Announcements are not authenticated, so anyone on the link can send
	// any number of them.
	maxMDNSEntries = 64
)

// MDNSSource announces this node on the local link and collects the other
// nodes announcing the same network tag. An announcement carries the node id
// (as the instance name), the port and the tag, nothing else.
type MDNSSource struct {
	id  string
	tag string
	now func() time.Time

	mu   sync.Mutex
	seen map[string]mdnsEntry // by node id
}

type mdnsEntry struct {
	addr string
	at   time.Time
}

// StartMDNS announces and browses until ctx is done. It uses the one
// interface that carries the default route, over IPv4.
func StartMDNS(ctx context.Context, id string, port int, tag string) (*MDNSSource, error) {
	iface, ip, err := primaryInterface()
	if err != nil {
		return nil, err
	}
	ifaces := []net.Interface{iface}
	server, err := zeroconf.RegisterProxy(id, mdnsService, mdnsDomain, port,
		id, []string{ip.String()}, []string{"net=" + tag}, ifaces, zeroconf.TTL(120))
	if err != nil {
		return nil, err
	}

	s := &MDNSSource{id: id, tag: tag, now: time.Now, seen: make(map[string]mdnsEntry)}
	go func() {
		defer server.Shutdown()
		for ctx.Err() == nil {
			s.browse(ctx, ifaces)
		}
	}()
	return s, nil
}

func (s *MDNSSource) browse(ctx context.Context, ifaces []net.Interface) {
	ctx, cancel := context.WithTimeout(ctx, browseWindow)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry, 16)
	failed := make(chan struct{})
	go func() {
		// Browse closes entries when ctx ends, unless it fails to start.
		err := zeroconf.Browse(ctx, mdnsService, mdnsDomain, entries,
			zeroconf.SelectIfaces(ifaces), zeroconf.SelectIPTraffic(zeroconf.IPv4))
		if err != nil {
			close(failed)
		}
	}()
	for {
		select {
		case e, ok := <-entries:
			if !ok {
				return
			}
			if addr, ok := s.accept(e.Instance, e.Text, e.Port, e.AddrIPv4); ok {
				s.remember(e.Instance, addr)
			}
		case <-failed:
			<-ctx.Done()
			return
		}
	}
}

// accept decides whether an announcement is another node of our network and
// returns the address to probe.
func (s *MDNSSource) accept(instance string, text []string, port int, ips []net.IP) (string, bool) {
	if instance == s.id || len(ips) == 0 {
		return "", false
	}
	for _, kv := range text {
		if tag, ok := strings.CutPrefix(kv, "net="); ok && tag == s.tag {
			return net.JoinHostPort(ips[0].String(), strconv.Itoa(port)), true
		}
	}
	return "", false
}

// remember records an accepted announcement. When the cache is full, a node
// that is not in it yet has to wait until an entry expires.
func (s *MDNSSource) remember(id, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if _, known := s.seen[id]; known || len(s.seen) < maxMDNSEntries {
		s.seen[id] = mdnsEntry{addr: addr, at: s.now()}
	}
}

// expire forgets the nodes that have stopped announcing. Callers hold mu.
func (s *MDNSSource) expire() {
	now := s.now()
	for id, e := range s.seen {
		if now.Sub(e.at) > mdnsExpiry {
			delete(s.seen, id)
		}
	}
}

func (s *MDNSSource) Name() string { return MDNS }

func (s *MDNSSource) Candidates(context.Context) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	var out []Candidate
	for _, e := range s.seen {
		out = append(out, Candidate{Addr: e.addr, Source: MDNS})
	}
	return out, nil
}

// primaryInterface returns the interface the OS routes multicast through and
// its IPv4 address. Connecting a UDP socket sends nothing; it only asks the
// routing table which local address would be used.
func primaryInterface() (net.Interface, net.IP, error) {
	conn, err := net.Dial("udp4", "224.0.0.251:5353")
	if err != nil {
		return net.Interface{}, nil, err
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP

	ifaces, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return iface, ip, nil
			}
		}
	}
	return net.Interface{}, nil, errors.New("no multicast interface for " + ip.String())
}

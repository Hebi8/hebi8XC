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

// StartMDNS announces and browses until ctx is done. It uses one LAN
// interface, over IPv4.
func StartMDNS(ctx context.Context, id string, port int, tag string) (*MDNSSource, error) {
	iface, ip, err := mdnsInterface()
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

// lanInterface is a network interface and the IP addresses on it.
type lanInterface struct {
	net.Interface
	IPs []net.IP
}

// lanAddr returns the address mDNS can use on the interface: a private
// IPv4 address (10/8, 172.16/12, 192.168/16) on an interface that is up,
// does multicast and is neither loopback nor point-to-point. That leaves
// out VPN interfaces such as Tailscale's, whose addresses are in
// 100.64.0.0/10. prefer is returned if it is one of the usable addresses.
func (i lanInterface) lanAddr(prefer net.IP) (net.IP, bool) {
	const need, never = net.FlagUp | net.FlagMulticast, net.FlagLoopback | net.FlagPointToPoint
	if i.Flags&need != need || i.Flags&never != 0 {
		return nil, false
	}
	var first net.IP
	for _, ip := range i.IPs {
		if ip.To4() == nil || !ip.IsPrivate() {
			continue
		}
		if ip.Equal(prefer) {
			return ip, true
		}
		if first == nil {
			first = ip
		}
	}
	return first, first != nil
}

// chooseMDNSInterface picks the one interface mDNS runs on, and the address
// to announce. routed is the local address the OS would send multicast
// from; the interface that has it is the choice whenever it is a LAN
// interface, which is the usual case. Where the OS prefers something else,
// a VPN for example, the choice is the LAN interface with the lowest index.
func chooseMDNSInterface(ifaces []lanInterface, routed net.IP) (net.Interface, net.IP, bool) {
	var best lanInterface
	var bestIP net.IP
	for _, i := range ifaces {
		ip, ok := i.lanAddr(routed)
		if !ok {
			continue
		}
		if ip.Equal(routed) {
			return i.Interface, ip, true
		}
		if bestIP == nil || i.Index < best.Index {
			best, bestIP = i, ip
		}
	}
	return best.Interface, bestIP, bestIP != nil
}

// mdnsInterface asks the OS for its interfaces and its multicast route and
// chooses among them.
func mdnsInterface() (net.Interface, net.IP, error) {
	all, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, nil, err
	}
	ifaces := make([]lanInterface, len(all))
	for n, iface := range all {
		ifaces[n].Interface = iface
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				ifaces[n].IPs = append(ifaces[n].IPs, ipnet.IP)
			}
		}
	}

	// Connecting a UDP socket sends nothing; it only asks the routing table
	// which local address would be used.
	var routed net.IP
	if conn, err := net.Dial("udp4", "224.0.0.251:5353"); err == nil {
		routed = conn.LocalAddr().(*net.UDPAddr).IP
		conn.Close()
	}

	iface, ip, ok := chooseMDNSInterface(ifaces, routed)
	if !ok {
		return net.Interface{}, nil, errors.New("no LAN interface that can do multicast")
	}
	return iface, ip, nil
}

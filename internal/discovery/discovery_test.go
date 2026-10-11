package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestNetworkTag(t *testing.T) {
	tag := NetworkTag("alice")
	if tag != NetworkTag("alice") {
		t.Error("tag is not stable")
	}
	if tag == NetworkTag("someone-else") {
		t.Error("different users share a tag")
	}
	if len(tag) != 16 {
		t.Errorf("tag = %q, want 16 hex characters", tag)
	}
}

func TestStaticSource(t *testing.T) {
	got, err := StaticSource([]string{"n100:4892"}).Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Candidate{{Addr: "n100:4892", Source: Static}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

const tailscaleStatusJSON = `{
  "BackendState": "Running",
  "Self": {"TailscaleIPs": ["100.64.0.1", "fd7a:115c:a1e0::1"], "Online": true},
  "Peer": {
    "nodekey:aa": {"HostName": "n100", "TailscaleIPs": ["100.64.0.2", "fd7a:115c:a1e0::2"], "Online": true},
    "nodekey:bb": {"HostName": "phone", "TailscaleIPs": ["100.64.0.9"], "Online": false},
    "nodekey:cc": {"HostName": "v6only", "TailscaleIPs": ["fd7a:115c:a1e0::3"], "Online": true}
  }
}`

func tailscaleWith(out string, err error) *TailscaleSource {
	return &TailscaleSource{Port: 4892, timeout: time.Minute, status: func(context.Context) ([]byte, error) {
		return []byte(out), err
	}}
}

func TestTailscaleListsOnlinePeersOnTheFixedPort(t *testing.T) {
	got, err := tailscaleWith(tailscaleStatusJSON, nil).Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Candidate{{Addr: "100.64.0.2:4892", Source: Tailscale}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTailscaleIsQuietWhenAbsentOrStopped(t *testing.T) {
	for name, src := range map[string]*TailscaleSource{
		"not installed": tailscaleWith("", exec.ErrNotFound),
		"stopped":       tailscaleWith(`{"BackendState": "Stopped", "Peer": {"k": {"TailscaleIPs": ["100.64.0.1"], "Online": true}}}`, nil),
	} {
		got, err := src.Candidates(context.Background())
		if err != nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v; want nothing", name, got, err)
		}
	}
}

func TestMDNSAcceptsOnlyOtherNodesOfTheSameNetwork(t *testing.T) {
	s := &MDNSSource{id: "self", tag: "aaaa"}
	ips := []net.IP{net.IPv4(192, 168, 1, 20)}
	for _, tc := range []struct {
		name     string
		instance string
		text     []string
		ips      []net.IP
		want     string
	}{
		{"same network", "other", []string{"net=aaaa"}, ips, "192.168.1.20:4892"},
		{"own announcement", "self", []string{"net=aaaa"}, ips, ""},
		{"another user's network", "other", []string{"net=bbbb"}, ips, ""},
		{"no tag", "other", nil, ips, ""},
		{"no address", "other", []string{"net=aaaa"}, nil, ""},
	} {
		got, ok := s.accept(tc.instance, tc.text, 4892, tc.ips)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: got %q, %v", tc.name, got, ok)
		}
	}
}

// A tailscale CLI that hangs costs the round its timeout, not the round.
func TestTailscaleGivesUpOnAHangingCLI(t *testing.T) {
	src := &TailscaleSource{Port: 4892, timeout: time.Millisecond, status: func(ctx context.Context) ([]byte, error) {
		<-ctx.Done() // returns only because the source's own deadline fires
		return nil, ctx.Err()
	}}
	got, err := src.Candidates(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
		t.Errorf("got %v, %v; want a deadline error and no candidates", got, err)
	}
}

func TestMDNSCacheIsBoundedAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := &MDNSSource{id: "self", tag: "aaaa", now: func() time.Time { return now }, seen: make(map[string]mdnsEntry)}
	count := func() int {
		cands, err := s.Candidates(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(cands)
	}

	for i := range maxMDNSEntries + 10 {
		s.remember(fmt.Sprintf("node-%d", i), fmt.Sprintf("10.0.0.%d:4892", i))
	}
	if got := count(); got != maxMDNSEntries {
		t.Fatalf("cache holds %d entries, want the limit %d", got, maxMDNSEntries)
	}

	// A node already in the cache keeps its place and may change address.
	now = now.Add(mdnsExpiry)
	s.remember("node-0", "10.0.9.9:4892")
	now = now.Add(time.Second)
	cands, _ := s.Candidates(context.Background())
	want := []Candidate{{Addr: "10.0.9.9:4892", Source: MDNS}}
	if !slices.Equal(cands, want) {
		t.Fatalf("after the others expired: %v, want %v", cands, want)
	}

	// With room again, a new node gets in.
	s.remember("late", "10.0.0.200:4892")
	if got := count(); got != 2 {
		t.Errorf("cache holds %d entries, want 2", got)
	}
}

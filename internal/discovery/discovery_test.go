package discovery

import (
	"context"
	"net"
	"os/exec"
	"slices"
	"testing"
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
	return &TailscaleSource{Port: 4892, status: func(context.Context) ([]byte, error) {
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
	ips := []net.IP{net.IPv4(192, 168, 1, 13)}
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

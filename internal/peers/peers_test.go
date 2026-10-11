package peers

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

const offlineAfter = 30 * time.Second

func newTable(t *testing.T) (*Table, *clock, string) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "peers.json")
	table, err := Load(path, c.now, offlineAfter)
	if err != nil {
		t.Fatal(err)
	}
	return table, c, path
}

func n100(addr string, sources ...string) Contact {
	return Contact{ID: "n100-id", Name: "n100", Key: "ssh-ed25519 AAAA", Addr: addr, Sources: sources}
}

func only(t *testing.T, table *Table) Peer {
	t.Helper()
	list := table.List()
	if len(list) != 1 {
		t.Fatalf("table has %d peers, want 1: %+v", len(list), list)
	}
	return list[0]
}

func TestSameNodeFromSeveralSourcesIsOnePeer(t *testing.T) {
	table, _, _ := newTable(t)
	if !table.Observe(n100("192.168.1.20:4892", "mdns")) {
		t.Error("first contact did not bring the peer online")
	}
	if table.Observe(n100("100.64.0.2:4892", "tailscale")) {
		t.Error("second contact reported the peer coming online again")
	}
	table.Observe(n100("192.168.1.20:4892", "static"))
	table.Observe(n100("", "inbound"))

	p := only(t, table)
	if want := []string{"inbound", "mdns", "static", "tailscale"}; !slices.Equal(p.Sources, want) {
		t.Errorf("sources = %v, want %v", p.Sources, want)
	}
	if len(p.Addrs) != 2 {
		t.Fatalf("addrs = %+v, want 2", p.Addrs)
	}
	if want := []string{"mdns", "static"}; !slices.Equal(p.Addrs[0].Sources, want) {
		t.Errorf("sources of %s = %v, want %v", p.Addrs[0].Addr, p.Addrs[0].Sources, want)
	}
}

func TestSilentPeerGoesOfflineAndComesBack(t *testing.T) {
	table, c, _ := newTable(t)
	table.Observe(n100("192.168.1.20:4892", "mdns"))
	seen := c.now()

	c.advance(offlineAfter)
	if gone := table.Sweep(); len(gone) != 0 {
		t.Fatalf("peer went offline at exactly the limit: %+v", gone)
	}
	c.advance(time.Second)
	gone := table.Sweep()
	if len(gone) != 1 || gone[0].ID != "n100-id" {
		t.Fatalf("Sweep = %+v, want n100", gone)
	}
	if again := table.Sweep(); len(again) != 0 {
		t.Error("an offline peer was reported offline twice")
	}

	p := only(t, table)
	if p.Online || !p.LastSeen.Equal(seen) || len(p.Addrs) != 1 {
		t.Errorf("offline peer should keep its record: %+v", p)
	}

	c.advance(time.Hour)
	if !table.Observe(n100("192.168.1.20:4892", "mdns")) {
		t.Error("returning peer did not come online")
	}
	if p := only(t, table); !p.Online || !p.LastSeen.Equal(c.now()) {
		t.Errorf("peer did not recover: %+v", p)
	}
}

func TestTableSurvivesRestart(t *testing.T) {
	table, c, path := newTable(t)
	table.Observe(n100("192.168.1.20:4892", "mdns"))
	if err := table.Save(false); err != nil {
		t.Fatal(err)
	}
	before := only(t, table)

	reloaded, err := Load(path, c.now, offlineAfter)
	if err != nil {
		t.Fatal(err)
	}
	after := only(t, reloaded)
	if after.Online {
		t.Error("a reloaded peer is online before any contact")
	}
	if after.ID != before.ID || after.Name != before.Name || after.Key != before.Key ||
		!after.LastSeen.Equal(before.LastSeen) || !slices.Equal(after.Sources, before.Sources) {
		t.Errorf("reloaded %+v, saved %+v", after, before)
	}
	if got := reloaded.Addrs(); !slices.Equal(got, []string{"192.168.1.20:4892"}) {
		t.Errorf("remembered addresses = %v", got)
	}
}

func TestSaveThrottlesLastSeenOnlyUpdates(t *testing.T) {
	table, c, path := newTable(t)
	table.Observe(n100("192.168.1.20:4892", "mdns"))
	if err := table.Save(false); err != nil {
		t.Fatal(err)
	}
	onDisk := func() time.Time {
		reloaded, err := Load(path, c.now, offlineAfter)
		if err != nil {
			t.Fatal(err)
		}
		return only(t, reloaded).LastSeen
	}
	first := onDisk()

	c.advance(10 * time.Second)
	table.Observe(n100("192.168.1.20:4892", "mdns"))
	if err := table.Save(false); err != nil {
		t.Fatal(err)
	}
	if !onDisk().Equal(first) {
		t.Error("a heartbeat alone rewrote the file")
	}

	c.advance(saveEvery)
	table.Observe(n100("192.168.1.20:4892", "mdns"))
	if err := table.Save(false); err != nil {
		t.Fatal(err)
	}
	if !onDisk().Equal(c.now()) {
		t.Error("last-seen time on disk is older than saveEvery")
	}
}

func TestOldestAddressesAreDropped(t *testing.T) {
	table, c, _ := newTable(t)
	for i := range maxAddrs + 2 {
		table.Observe(n100(fmt.Sprintf("10.0.0.%d:4892", i), "static"))
		c.advance(time.Second)
	}
	p := only(t, table)
	if len(p.Addrs) != maxAddrs {
		t.Fatalf("kept %d addresses, want %d", len(p.Addrs), maxAddrs)
	}
	for _, a := range p.Addrs {
		if a.Addr == "10.0.0.0:4892" || a.Addr == "10.0.0.1:4892" {
			t.Errorf("kept the old address %s", a.Addr)
		}
	}
}

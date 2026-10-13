// Package demo runs a small network of h8c nodes on this machine, for
// looking at the web interface and for the browser check in
// scripts/websmoke: the real binary, temporary data directories, indexes
// built with waketest, loopback addresses and no discovery. Nothing of it
// touches a node that is installed on the machine.
//
// Three machines are played. "desk" is the one whose page is opened,
// "laptop" is online, and "attic" was online once and is not any more: one
// of its sessions was read while it was, so a copy of that one is kept.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

// DefaultPort is the first of the four ports the demo uses: desk listens
// there and has its page on the next one, as an installed node has them on
// 4892 and 4893, which the demo stays far from.
const DefaultPort = 24892

// Options says how to start the demo.
type Options struct {
	// Port is the first of four ports in a row; 0 picks free ones.
	Port int
	// Names of the three machines; an empty one gets its usual name.
	Names [3]string
	// Seed can add sessions of its own, before the nodes start.
	Seed func(desk, laptop, attic *waketest.DB)
}

// Net is a running demo.
type Net struct {
	URL  string // opens the web interface of desk; it carries the token
	Dir  string // everything the demo wrote; Close removes it
	Data Data

	h8c    string
	nodes  []*proc
	failed chan error
}

// proc is a node: h8c up in the foreground of the demo.
type proc struct {
	cmd      *exec.Cmd
	stopping atomic.Bool
	gone     chan struct{} // closed when the process has ended
}

// builder is the demo's side of waketest.TB.
type builder struct{ cleanup []func() }

func (b *builder) Helper()           {}
func (b *builder) Fatal(args ...any) { panic(errors.New(fmt.Sprint(args...))) }
func (b *builder) Cleanup(f func())  { b.cleanup = append(b.cleanup, f) }

// Start builds h8c from this source tree, which the working directory has
// to be in, and brings the demo up. It returns when desk lists the sessions
// of all three machines.
func Start(ctx context.Context, o Options) (n *Net, err error) {
	dir, err := os.MkdirTemp("", "h8c-demo-")
	if err != nil {
		return nil, err
	}
	n = &Net{Dir: dir, failed: make(chan error, 8)}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("building the indexes: %v", r)
		}
		if err != nil {
			n.Close()
			n = nil
		}
	}()

	ports, err := choosePorts(o.Port)
	if err != nil {
		return n, err
	}
	n.h8c = filepath.Join(dir, "h8c")
	if runtime.GOOS == "windows" {
		n.h8c += ".exe"
	}
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", n.h8c, "github.com/Hebi8/hebi8XC/cmd/h8c").CombinedOutput(); err != nil {
		return n, fmt.Errorf("go build (run this from the source tree): %v\n%s", err, out)
	}

	names := [3]string{"desk", "laptop", "attic"}
	var dbs [3]*waketest.DB
	b := &builder{}
	for i := range names {
		if o.Names[i] != "" {
			names[i] = o.Names[i]
		}
		index := filepath.Join(dir, "wake-"+strconv.Itoa(i))
		if err := os.Mkdir(index, 0o700); err != nil {
			return n, err
		}
		dbs[i] = waketest.New(b, index)
	}
	n.Data = fill(dbs[0], dbs[1], dbs[2], time.Now())
	if o.Seed != nil {
		o.Seed(dbs[0], dbs[1], dbs[2])
	}
	for _, f := range b.cleanup {
		f() // the nodes read the indexes; nothing writes them from here on
	}

	// The machines trust each other's keys by hand: there is no login here.
	var keys, ids [3]string
	for i, name := range names {
		data := filepath.Join(dir, "node-"+strconv.Itoa(i))
		if _, err := n.run(ctx, data, "name", name); err != nil {
			return n, err
		}
		out, err := n.run(ctx, data, "id")
		if err != nil {
			return n, err
		}
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 3 && f[0] == "key" {
				keys[i] = f[1] + " " + f[2]
			} else if len(f) == 2 && f[0] == "id" {
				ids[i] = f[1]
			}
		}
		if keys[i] == "" || ids[i] == "" {
			return n, fmt.Errorf("h8c id printed no key or no id:\n%s", out)
		}
	}
	for i := range names {
		for j := range names {
			if i == j {
				continue
			}
			if _, err := n.run(ctx, n.data(i), "trust", "add", keys[j]); err != nil {
				return n, err
			}
		}
	}

	const desk, laptop, attic = 0, 1, 2
	listen := [3]int{ports[0], ports[2], ports[3]}
	addr := func(i int) string { return "127.0.0.1:" + strconv.Itoa(listen[i]) }
	web := "127.0.0.1:" + strconv.Itoa(ports[1])
	up := func(i int, more ...string) (*proc, error) {
		args := append([]string{"--listen", addr(i), "--discover", "none", "--user", "alice",
			"--wake-db", dbs[i].Path, "--wake-cli", filepath.Join(dir, "no-wake-cli")}, more...)
		return n.up(ctx, names[i], n.data(i), args...)
	}

	// First attic is online: desk reads its list and one of its sessions.
	// Desk is the one that dials, and starts second: a node reads the
	// sessions of a peer as soon as it has reached it itself.
	a, err := up(attic, "--web", "none")
	if err != nil {
		return n, err
	}
	d, err := up(desk, "--web", web, "--peer", addr(attic))
	if err != nil {
		return n, err
	}
	if err := n.await(ctx, ids[attic], n.Data.Sessions[attic]); err != nil {
		return n, err
	}
	if _, err := n.run(ctx, n.data(desk), "show", ids[attic]+"/"+n.Data.Kept); err != nil {
		return n, err
	}
	a.stop()
	d.stop()
	n.nodes = nil

	// Then it is gone. Desk starts again, as a machine does that was off:
	// it remembers attic and what it read of it, and does not reach it.
	if _, err := up(laptop, "--web", "none"); err != nil {
		return n, err
	}
	if _, err := up(desk, "--web", web, "--peer", addr(laptop)); err != nil {
		return n, err
	}
	if err := n.await(ctx, ids[laptop], n.Data.Sessions[laptop]); err != nil {
		return n, err
	}
	link, err := n.run(ctx, n.data(desk), "web")
	if err != nil {
		return n, err
	}
	n.URL = strings.TrimSpace(link)
	n.Data.Names, n.Data.IDs = names, ids
	return n, nil
}

func (n *Net) data(i int) string { return filepath.Join(n.Dir, "node-"+strconv.Itoa(i)) }

// Failed gets a value when a node stops that the demo did not stop.
func (n *Net) Failed() <-chan error { return n.failed }

// Close stops the nodes and removes what the demo wrote.
func (n *Net) Close() {
	for _, p := range n.nodes {
		p.stop()
	}
	n.nodes = nil
	os.RemoveAll(n.Dir)
}

// choosePorts returns four ports that are free now: the four from first
// on, or with 0 any four.
func choosePorts(first int) (ports [4]int, err error) {
	for i := range ports {
		at := "127.0.0.1:0"
		if first != 0 {
			at = "127.0.0.1:" + strconv.Itoa(first+i)
		}
		l, err := net.Listen("tcp", at)
		if err != nil {
			return ports, fmt.Errorf("the demo needs four free ports from %d on; choose others with -port: %v", first, err)
		}
		defer l.Close()
		ports[i] = l.Addr().(*net.TCPAddr).Port
	}
	return ports, nil
}

// run runs an h8c command on a data directory and returns what it printed.
func (n *Net) run(ctx context.Context, data string, args ...string) (string, error) {
	var out, errs bytes.Buffer
	cmd := exec.CommandContext(ctx, n.h8c, append([]string{"--data-dir", data}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("h8c %s: %v: %s", args[0], err, strings.TrimSpace(errs.String()))
	}
	return out.String(), nil
}

// up starts a node and waits until it answers on its control socket, so
// that it listens by the time the next one dials it. Its log goes to a
// file next to its data.
func (n *Net) up(ctx context.Context, name, data string, args ...string) (*proc, error) {
	log, err := os.OpenFile(data+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	p := &proc{gone: make(chan struct{})}
	p.cmd = exec.Command(n.h8c, append([]string{"--data-dir", data, "up"}, args...)...)
	p.cmd.Stdout, p.cmd.Stderr = log, log
	err = p.cmd.Start()
	log.Close()
	if err != nil {
		return nil, err
	}
	n.nodes = append(n.nodes, p)
	go func() {
		err := p.cmd.Wait()
		if !p.stopping.Load() {
			n.failed <- fmt.Errorf("the node %s stopped: %v\n%s", name, err, tail(data+".log"))
		}
		close(p.gone)
	}()
	for deadline := time.Now().Add(20 * time.Second); ; {
		_, err := n.run(ctx, data, "peers")
		if err == nil {
			return p, nil
		}
		select {
		case <-p.gone:
			return nil, fmt.Errorf("the node %s did not start:\n%s", name, tail(data+".log"))
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the node %s does not answer: %v\n%s", name, err, tail(data+".log"))
		}
	}
}

// stop ends a node the way Ctrl-C does, and waits until it is gone.
func (p *proc) stop() {
	p.stopping.Store(true)
	if p.cmd.Process.Signal(os.Interrupt) != nil {
		p.cmd.Process.Kill() // where there is no such signal
	}
	select {
	case <-p.gone:
	case <-time.After(10 * time.Second):
		p.cmd.Process.Kill()
		<-p.gone
	}
}

// await waits until desk has read the list of the node id and it has this
// many sessions.
func (n *Net) await(ctx context.Context, id string, sessions int) error {
	var last string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-n.failed:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		out, err := n.run(ctx, n.data(0), "sessions", "--json", "--limit", "1")
		if err != nil {
			last = err.Error()
			continue
		}
		var listing struct {
			Nodes []struct {
				ID       string `json:"id"`
				Sessions int    `json:"sessions"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(out), &listing); err != nil {
			return fmt.Errorf("h8c sessions --json: %v", err)
		}
		last = fmt.Sprintf("desk knows %d nodes", len(listing.Nodes))
		for _, node := range listing.Nodes {
			if node.ID == id {
				last = fmt.Sprintf("desk has %d of its %d sessions", node.Sessions, sessions)
				if node.Sessions == sessions {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("desk did not get the sessions of %s in a minute: %s\n%s", id, last, tail(n.data(0)+".log"))
}

// tail returns the end of a log, for an error.
func tail(path string) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}

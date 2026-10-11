// Command h8c runs and inspects a hebi8XC node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"hebi8xc/internal/control"
	"hebi8xc/internal/discovery"
	"hebi8xc/internal/identity"
	"hebi8xc/internal/node"
	"hebi8xc/internal/peers"
	"hebi8xc/internal/roster"
	"hebi8xc/internal/sshkey"
)

const usage = `usage: h8c [--data-dir DIR] <command>

  up [--listen ADDR] [--user ID] [--peer HOST[:PORT]]... [--discover LIST]
                         run the node in the foreground
  peers                  list the nodes the running node knows
  id                     show this node's id, name and public key
  trust add <key>        trust a node's public key (the "key" line of h8c id)
  trust list             list the trusted keys
  trust remove <key|comment|fingerprint>
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "h8c:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	global := flag.NewFlagSet("h8c", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(global.Output(), usage) }
	dataDir := global.String("data-dir", "", "")
	if err := global.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		*dataDir = filepath.Join(base, "hebi8xc")
	}

	args = global.Args()
	if len(args) == 0 {
		return errors.New("missing command\n" + usage)
	}
	switch args[0] {
	case "up":
		return up(*dataDir, args[1:])
	case "peers":
		return listPeers(*dataDir, out)
	case "id":
		return showID(*dataDir, out)
	case "trust":
		return trust(*dataDir, args[1:], out)
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}

func up(dir string, args []string) error {
	fs := flag.NewFlagSet("h8c up", flag.ContinueOnError)
	listen := fs.String("listen", ":"+strconv.Itoa(node.DefaultPort), "address the peer link listens on")
	userID := fs.String("user", "", "user id shared by all of your nodes (default: OS user name)")
	discover := fs.String("discover", "mdns,tailscale", "automatic discovery sources: mdns, tailscale, none")
	var static []string
	fs.Func("peer", "address of another node, HOST[:PORT]; may be repeated", func(v string) error {
		if _, _, err := net.SplitHostPort(v); err != nil {
			v = net.JoinHostPort(v, strconv.Itoa(node.DefaultPort))
		}
		static = append(static, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *userID == "" {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("cannot determine the OS user, pass --user: %w", err)
		}
		// Windows reports DOMAIN\name.
		*userID = u.Username[strings.LastIndex(u.Username, `\`)+1:]
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ident, err := identity.Load(dir)
	if err != nil {
		return err
	}
	trusted, err := roster.OpenFile(filepath.Join(dir, node.RosterFile))
	if err != nil {
		return err
	}
	table, err := peers.Load(filepath.Join(dir, node.PeersFile), time.Now, node.OfflineAfter)
	if err != nil {
		return err
	}
	ctl, err := control.Listen(filepath.Join(dir, node.ControlSocket))
	if err != nil {
		return err
	}
	defer os.Remove(filepath.Join(dir, node.ControlSocket))
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	tag := discovery.NetworkTag(*userID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sources := []discovery.Source{
		discovery.Func(discovery.Remembered, table.Addrs),
		discovery.StaticSource(static),
	}
	for _, name := range strings.Split(*discover, ",") {
		switch name {
		case discovery.MDNS:
			mdns, err := discovery.StartMDNS(ctx, ident.ID, port, tag)
			if err != nil {
				log.Warn("mDNS discovery is off", "err", err)
				continue
			}
			sources = append(sources, mdns)
		case discovery.Tailscale:
			sources = append(sources, discovery.NewTailscaleSource(node.DefaultPort))
		case "none", "":
		default:
			return fmt.Errorf("unknown discovery source %q", name)
		}
	}

	n, err := node.New(node.Config{
		Identity: ident,
		User:     *userID,
		Roster:   trusted,
		Table:    table,
		Sources:  sources,
		Listener: ln,
		Control:  ctl,
		Log:      log,
	})
	if err != nil {
		return err
	}
	log.Info("node up", "name", ident.Name, "id", ident.ID, "listen", ln.Addr().String(),
		"user", *userID, "network_tag", tag, "discover", *discover, "trusted_keys", len(trusted.Entries()))
	n.Run(ctx)
	log.Info("node down")
	return nil
}

func listPeers(dir string, out io.Writer) error {
	var list []peers.Peer
	err := control.Dial(filepath.Join(dir, node.ControlSocket)).Get(context.Background(), "/v1/peers", &list)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tSTATE\tLAST SEEN\tVIA\tADDRESSES")
	for _, p := range list {
		state := "offline"
		if p.Online {
			state = "online"
		}
		addrs := make([]string, len(p.Addrs))
		for i, a := range p.Addrs {
			addrs[i] = a.Addr
		}
		slices.Sort(addrs)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.ID, state,
			p.LastSeen.Local().Format(time.DateTime), strings.Join(p.Sources, ","), strings.Join(addrs, " "))
	}
	return tw.Flush()
}

func showID(dir string, out io.Writer) error {
	ident, err := identity.Load(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "id    %s\nname  %s\nkey   %s %s\n",
		ident.ID, ident.Name, sshkey.Marshal(ident.Public()), ident.Name)
	return nil
}

func trust(dir string, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("want: h8c trust add|list|remove")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	trusted, err := roster.OpenFile(filepath.Join(dir, node.RosterFile))
	if err != nil {
		return err
	}
	// A key line contains spaces; accept it quoted or as separate arguments.
	arg := strings.Join(args[1:], " ")
	switch args[0] {
	case "list":
		for _, e := range trusted.Entries() {
			fmt.Fprintln(out, strings.TrimSpace(sshkey.Marshal(e.Key)+" "+e.Comment))
		}
		return nil
	case "add":
		key, comment, err := sshkey.Parse(arg)
		if err != nil {
			return err
		}
		added, err := trusted.Add(key, comment)
		if err != nil {
			return err
		}
		if !added {
			fmt.Fprintln(out, "already trusted")
			return nil
		}
		fmt.Fprintln(out, "trusted", sshkey.Fingerprint(key), comment)
	case "remove":
		n, err := trusted.Remove(arg)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no trusted key matches %q", arg)
		}
		fmt.Fprintln(out, "removed", n)
	default:
		return fmt.Errorf("unknown trust command %q", args[0])
	}

	// Let a running node act on the change now instead of in its next round.
	err = control.Dial(filepath.Join(dir, node.ControlSocket)).Post(context.Background(), "/v1/refresh")
	if err != nil && !errors.Is(err, control.ErrNotRunning) {
		return err
	}
	return nil
}

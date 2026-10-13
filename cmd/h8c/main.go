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

	"github.com/Hebi8/hebi8XC/internal/control"
	"github.com/Hebi8/hebi8XC/internal/datadir"
	"github.com/Hebi8/hebi8XC/internal/discovery"
	"github.com/Hebi8/hebi8XC/internal/github"
	"github.com/Hebi8/hebi8XC/internal/identity"
	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/peers"
	"github.com/Hebi8/hebi8XC/internal/roster"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

const usage = `usage: h8c [--data-dir DIR] <command>

  name [NEW NAME]        show or set this node's name
  login                  log in to GitHub and register this node's key there
  up [--listen ADDR] [--peer HOST[:PORT]]... [--discover LIST] [--user ID]
                         run the node in the foreground
  peers                  list the nodes the running node knows
  id                     show this node's id, name, public key and account
  roster                 list the trusted keys and where they come from
  roster refresh         fetch the GitHub roster now
  trust add <key>        trust a node's public key by hand (the "key" line of h8c id)
  trust list             list the keys trusted by hand
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
	case "peers":
		return listPeers(*dataDir, out)
	case "up", "id", "name", "trust", "login", "roster":
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}

	// The remaining commands read or write files in the data directory.
	if err := datadir.Ensure(*dataDir); err != nil {
		return err
	}
	switch args[0] {
	case "up":
		return up(*dataDir, args[1:])
	case "id":
		return showID(*dataDir, out)
	case "name":
		return nodeName(*dataDir, args[1:], out)
	case "login":
		return login(*dataDir, out)
	case "roster":
		return showRoster(*dataDir, args[1:], out)
	default:
		return trust(*dataDir, args[1:], out)
	}
}

// upOptions are the flags of h8c up.
type upOptions struct {
	listen   string
	user     string
	static   []string
	discover string
}

func up(dir string, args []string) error {
	var o upOptions
	fs := flag.NewFlagSet("h8c up", flag.ContinueOnError)
	fs.StringVar(&o.listen, "listen", ":"+strconv.Itoa(node.DefaultPort), "address the peer link listens on")
	fs.StringVar(&o.user, "user", "", "without a GitHub login: user id shared by all of your nodes (default: OS user name)")
	fs.StringVar(&o.discover, "discover", "mdns,tailscale", "automatic discovery sources: mdns, tailscale, none")
	fs.Func("peer", "address of another node, HOST[:PORT]; may be repeated", func(v string) error {
		if _, _, err := net.SplitHostPort(v); err != nil {
			v = net.JoinHostPort(v, strconv.Itoa(node.DefaultPort))
		}
		o.static = append(o.static, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	n, closeNode, err := openNode(ctx, dir, o, log)
	if err != nil {
		return err
	}
	defer closeNode()
	n.Run(ctx)
	log.Info("node down")
	return nil
}

// openNode builds the node of a data directory: identity, rosters, peer
// table, listeners and discovery sources. The returned function releases
// what the node holds in the directory; call it after the node has stopped.
func openNode(ctx context.Context, dir string, o upOptions, log *slog.Logger) (*node.Node, func(), error) {
	account, loggedIn, err := github.LoadAccount(filepath.Join(dir, node.AccountFile))
	if err != nil {
		return nil, nil, err
	}
	userID := o.user
	switch {
	case loggedIn && userID != "":
		return nil, nil, fmt.Errorf("--user does not apply: this node is logged in to GitHub as %s", account.Login)
	case loggedIn:
		userID = account.User()
	case userID == "":
		u, err := user.Current()
		if err != nil {
			return nil, nil, fmt.Errorf("cannot determine the OS user, pass --user: %w", err)
		}
		// Windows reports DOMAIN\name.
		userID = u.Username[strings.LastIndex(u.Username, `\`)+1:]
	}

	// One node per data directory. Holding the lock is also what allows
	// control.Listen to clear a socket left behind by a crash.
	unlock, err := datadir.Lock(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", dir, err)
	}
	socket := filepath.Join(dir, node.ControlSocket)
	var ctl, ln net.Listener
	release := func() {
		if ln != nil {
			ln.Close()
		}
		if ctl != nil {
			ctl.Close()
			os.Remove(socket)
		}
		unlock()
	}
	fail := func(err error) (*node.Node, func(), error) {
		release()
		return nil, nil, err
	}

	ident, err := identity.Load(dir)
	if err != nil {
		return fail(err)
	}
	trusted, err := roster.OpenFile(filepath.Join(dir, node.RosterFile))
	if err != nil {
		return fail(err)
	}
	var remote roster.Roster
	if loggedIn {
		remote, err = openGitHubRoster(dir, account, func(warning error) { log.Warn(warning.Error()) })
		if err != nil {
			return fail(err)
		}
	}
	table, err := peers.Load(filepath.Join(dir, node.PeersFile), time.Now, node.OfflineAfter)
	if err != nil {
		return fail(err)
	}
	if ctl, err = control.Listen(socket); err != nil {
		return fail(err)
	}
	if ln, err = net.Listen("tcp", o.listen); err != nil {
		return fail(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	tag := discovery.NetworkTag(userID)

	sources := []discovery.Source{
		discovery.Func(discovery.Remembered, table.Addrs),
		discovery.StaticSource(o.static),
	}
	for _, name := range strings.Split(o.discover, ",") {
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
			return fail(fmt.Errorf("unknown discovery source %q", name))
		}
	}

	n, err := node.New(node.Config{
		Identity: ident,
		User:     userID,
		Local:    trusted,
		Remote:   remote,
		Table:    table,
		Sources:  sources,
		Listener: ln,
		Control:  ctl,
		Now:      time.Now,
		Log:      log,
	})
	if err != nil {
		return fail(err)
	}
	attrs := []any{"name", ident.Name, "id", ident.ID, "listen", ln.Addr().String(),
		"user", userID, "network_tag", tag, "discover", o.discover, "keys_added_by_hand", len(trusted.Entries())}
	if loggedIn {
		attrs = append(attrs, "github", account.Login)
	}
	log.Info("node up", attrs...)
	return n, release, nil
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
	account, err := accountLine(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "id      %s\nname    %s\nkey     %s %s\ngithub  %s\n",
		ident.ID, ident.Name, sshkey.Marshal(ident.Public()), ident.Name, account)
	return nil
}

// nodeName shows the node's name or, given one, changes it. The name is
// what other nodes list this one under and part of the title of its key on
// GitHub.
func nodeName(dir string, args []string, out io.Writer) error {
	ident, err := identity.Load(dir)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(out, ident.Name)
		return nil
	}
	name := strings.TrimSpace(strings.Join(args, " "))
	if name == "" {
		return errors.New("the name cannot be empty")
	}
	if name == ident.Name {
		fmt.Fprintf(out, "This node is already called %q.\n", name)
		return nil
	}
	if err := identity.SetName(dir, name); err != nil {
		return err
	}
	fmt.Fprintf(out, "This node is now called %q.\n", name)

	var running node.Status
	if control.Dial(filepath.Join(dir, node.ControlSocket)).Get(context.Background(), "/v1/status", &running) == nil {
		fmt.Fprintln(out, "A node is running here under the old name; restart h8c up to use the new one.")
	}
	if _, loggedIn, err := github.LoadAccount(filepath.Join(dir, node.AccountFile)); err != nil {
		return err
	} else if loggedIn {
		fmt.Fprintf(out, "On GitHub this node's key keeps the title it was registered under, old name included, and that title is public. To change it, delete the key at %s and run h8c login again.\n", keySettingsURL)
	}
	return nil
}

func trust(dir string, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("want: h8c trust add|list|remove")
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

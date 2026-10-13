package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Hebi8/hebi8XC/internal/buildinfo"
	"github.com/Hebi8/hebi8XC/internal/control"
	"github.com/Hebi8/hebi8XC/internal/identity"
	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
)

// findWakeCLI locates wake-cli. Tests replace it so that none of them runs
// a wake-cli that happens to be installed.
var findWakeCLI = wake.FindCLI

// openSessions builds the session module of a node. Sessions of other
// nodes are always listed and cached. Those of this machine are shared when
// there is a Wake index to read them from, or wake-cli to build one.
func openSessions(dir string, o upOptions, ident *identity.Identity, log *slog.Logger) (*sessions.Service, func(), error) {
	store, err := sessions.OpenStore(filepath.Join(dir, node.SessionsFile), time.Now)
	if err != nil {
		return nil, nil, err
	}
	svc := &sessions.Service{
		Self:  sessions.Peer{ID: ident.ID, Name: ident.Name, Version: buildinfo.String()},
		Store: store,
		Now:   time.Now,
		Log:   log,
	}
	closeAll := func() { store.Close() }

	db := o.wakeDB
	if db == "" {
		if db, err = wake.DefaultPath(); err != nil {
			log.Warn("sessions of this machine are not shared: cannot tell where Wake keeps its index", "err", err)
			return svc, closeAll, nil
		}
	}
	if cli, ok := findWakeCLI(o.wakeCLI); ok {
		svc.Refresh = wake.CLI{Bin: cli, DB: db}.Refresh
	}
	// The index is opened when it is read, every round anew while that
	// fails. One that is built later, by Wake or by hand, is picked up
	// without a restart; until then the module says that there is none.
	index := wake.Open(db)
	svc.Source = index
	return svc, func() { index.Close(); store.Close() }, nil
}

// parseAnywhere parses flags that may stand before, between or after the
// other arguments, and returns those.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return rest, nil
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// since turns "36h", "7d", "2026-10-01" or an RFC 3339 time into the moment
// it means.
func since(text string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(text, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(text); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, text, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want a duration such as 36h or 7d, or a date such as 2026-10-01", text)
}

func listSessions(dir string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("h8c sessions", flag.ContinueOnError)
	nodeName := fs.String("node", "", "only sessions of this node, by name or id")
	agent := fs.String("agent", "", "only sessions of this agent")
	project := fs.String("project", "", "only sessions whose project name or path contains this")
	sinceText := fs.String("since", "", "only sessions updated since: 36h, 7d or 2026-10-01")
	limit := fs.Int("limit", 30, "how many sessions to list, at most "+strconv.Itoa(sessions.MaxPage))
	asJSON := fs.Bool("json", false, "print JSON")
	text, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}, "text": text}
	for name, value := range map[string]string{"node": *nodeName, "agent": *agent, "project": *project} {
		if value != "" {
			q.Set(name, value)
		}
	}
	if *sinceText != "" {
		from, err := since(*sinceText, time.Now())
		if err != nil {
			return err
		}
		q.Set("since", strconv.FormatInt(from.UnixMilli(), 10))
	}

	var list sessions.Listing
	err = control.Dial(filepath.Join(dir, node.ControlSocket)).Get(context.Background(), "/v1/sessions?"+q.Encode(), &list)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, list)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tUPDATED\tNODE\tAGENT\tPROJECT\tMSGS\tTITLE")
	for _, s := range list.Sessions {
		where := s.Node
		switch {
		case !s.Online:
			where += " (offline)"
		case s.Incompatible:
			where += " (incompatible)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", s.Ref, s.UpdatedAt.Local().Format("2006-01-02 15:04"),
			where, s.Agent, oneLine(s.Project, 24), s.Messages, oneLine(s.Title, 60))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	ours := 0
	for _, n := range list.Nodes {
		if n.Self {
			ours = n.Protocol
		}
	}
	for _, n := range list.Nodes {
		noun := "sessions"
		if n.Sessions == 1 {
			noun = "session"
		}
		fmt.Fprintf(out, "%s: %d %s, %s\n", n.Name, n.Sessions, noun, nodeState(n, ours))
	}
	return nil
}

// nodeState says in words where a node's sessions come from right now.
func nodeState(n sessions.Node, ours int) string {
	switch {
	case n.Self:
		return "this node"
	case n.Online && n.Incompatible:
		state := fmt.Sprintf("protocol incompatible (it speaks %d, this node %d)", n.Protocol, ours)
		if n.SyncedAt == nil {
			return state + ", never read"
		}
		return state + ", as of " + n.SyncedAt.Local().Format(time.DateTime)
	case n.SyncedAt == nil && n.Online:
		return "online, not read yet"
	case n.SyncedAt == nil:
		return "offline, never read"
	case n.Online:
		return "online, read " + n.SyncedAt.Local().Format(time.DateTime)
	default:
		return "offline, as of " + n.SyncedAt.Local().Format(time.DateTime)
	}
}

// oneLine fits text into a table cell: one line, at most width characters.
func oneLine(text string, width int) string {
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) > width {
		return string(r[:width-1]) + "…"
	}
	return string(r)
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func showSession(dir string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("h8c show", flag.ContinueOnError)
	after := fs.Int64("after", 0, "continue after the message with this id")
	limit := fs.Int("limit", 50, "how many messages to show, at most "+strconv.Itoa(sessions.MaxPage))
	asJSON := fs.Bool("json", false, "print JSON")
	rest, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("want: h8c show NODE/KEY, as in the first column of h8c sessions")
	}
	nodeName, key, ok := strings.Cut(rest[0], "/")
	if !ok || nodeName == "" || key == "" {
		return fmt.Errorf("%q is not NODE/KEY", rest[0])
	}
	q := url.Values{"node": {nodeName}, "key": {key},
		"after": {strconv.FormatInt(*after, 10)}, "limit": {strconv.Itoa(*limit)}}

	var r sessions.Reading
	err = control.Dial(filepath.Join(dir, node.ControlSocket)).Get(context.Background(), "/v1/sessions/messages?"+q.Encode(), &r)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, r)
	}

	s := r.Session
	fmt.Fprintf(out, "%s\n%s on %s, %s, %d messages, updated %s\n", oneLine(s.Title, 100),
		s.Agent, s.Node, s.ProjectPath, s.Messages, s.UpdatedAt.Local().Format(time.DateTime))
	switch {
	case r.Note != "":
		fmt.Fprintln(out, "note:", r.Note)
	case r.From == "cache":
		fmt.Fprintln(out, "from the copy made", r.CachedAt.Local().Format(time.DateTime))
	}
	if r.Stale {
		fmt.Fprintln(out, "note: the session has changed since this copy was made")
	}
	if r.Incomplete {
		fmt.Fprintln(out, "note: the copy does not reach the end of the session")
	}
	for _, m := range r.Messages {
		head := fmt.Sprintf("#%d %s", m.Seq, m.Role)
		if m.Sidechain != "" {
			head += " (side thread " + m.Sidechain + ")"
		}
		if m.Time > 0 {
			head += "  " + time.UnixMilli(m.Time).Local().Format(time.DateTime)
		}
		fmt.Fprintf(out, "\n--- %s\n%s\n", head, m.Text)
		if m.Truncated {
			fmt.Fprintln(out, "[cut: the message is longer]")
		}
	}
	if r.Next > 0 {
		fmt.Fprintf(out, "\nmore: h8c show %s --after %d\n", quoteArg(s.Ref), r.Next)
	}
	return nil
}

// quoteArg quotes a command line argument for a POSIX shell: in single
// quotes nothing is special, and a single quote itself is written '\”.
// Always, because the argument is a session name that came from a file on
// some machine and ends up in a command the user is invited to paste.
func quoteArg(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

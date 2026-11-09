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
	"github.com/Hebi8/hebi8XC/internal/sessions/antigravity"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake"
)

// findWakeCLI locates wake-cli. Tests replace it so that none of them runs
// a wake-cli that happens to be installed.
var findWakeCLI = wake.FindCLI

// Replaced by tests so they never read real ACP conversations.
var defaultACPPath = antigravity.DefaultPath

// openSessions builds the session module of a node. Sessions of other
// nodes are always listed and cached. Those of this machine are shared when
// there is a Wake index or an Antigravity ACP conversations directory.
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
	combined := &sessions.Combined{}
	svc.Source = combined
	acpDir := o.acpDir
	if acpDir == "" {
		acpDir, err = defaultACPPath()
		if err != nil {
			log.Warn("cannot locate Antigravity ACP conversations", "err", err)
		}
	}
	if acpDir != "" {
		combined.ACP = antigravity.Open(acpDir)
	}

	db := o.wakeDB
	if db == "" {
		if db, err = wake.DefaultPath(); err != nil {
			log.Warn("cannot tell where Wake keeps its index", "err", err)
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
	index.Log = log
	combined.Wake = index
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
	after := fs.String("after", "", "continue a listing: the value the page before ended with")
	asJSON := fs.Bool("json", false, "print JSON")
	text, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}, "text": text}
	for name, value := range map[string]string{"node": *nodeName, "agent": *agent, "project": *project, "after": *after} {
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
	if list.Next != "" {
		fmt.Fprintf(out, "\nmore: add --after %s\n", list.Next)
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
	after := fs.Int64("after", 0, "cursor, or number of messages after --around")
	around := fs.Int64("around", 0, "anchor message id")
	before := fs.Int("before", 8, "messages before anchor")
	rev := fs.Int64("rev", 0, "revision of the search anchor")
	all := fs.Bool("all", false, "include meta messages")
	limit := fs.Int("limit", 50, "how many messages to show, at most "+strconv.Itoa(sessions.MaxPage))
	asJSON := fs.Bool("json", false, "print JSON")
	rest, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("want: h8c show [NODE/]KEY; omit NODE only when the key is unique")
	}
	nodeName, key, ok := strings.Cut(rest[0], "/")
	if !ok {
		nodeName, key = "", rest[0]
	}
	if key == "" {
		return fmt.Errorf("%q is not NODE/KEY", rest[0])
	}
	q := url.Values{"node": {nodeName}, "key": {key},
		"after": {strconv.FormatInt(*after, 10)}, "limit": {strconv.Itoa(*limit)}}

	hasAround, hasRev := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "around" {
			hasAround = true
		}
		if f.Name == "rev" {
			hasRev = true
		}
	})
	if hasAround && *around <= 0 {
		return errors.New("--around requires a positive message id")
	}
	if hasRev && !hasAround {
		return errors.New("--rev requires --around")
	}
	if hasAround {
		q.Set("around", strconv.FormatInt(*around, 10))
		q.Set("before", strconv.Itoa(*before))
		q.Set("after", "2")
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "after" {
				q.Set("after", strconv.FormatInt(*after, 10))
			}
			if f.Name == "rev" {
				q.Set("rev", strconv.FormatInt(*rev, 10))
			}
		})
	}
	var r sessions.Reading
	err = control.Dial(filepath.Join(dir, node.ControlSocket)).Get(context.Background(), "/v1/sessions/messages?"+q.Encode(), &r)
	if err != nil {
		return err
	}
	if !*all {
		filtered := r.Messages[:0]
		for _, m := range r.Messages {
			if m.Kind != "meta" {
				filtered = append(filtered, m)
			}
		}
		r.Messages = filtered
	}
	if *asJSON {
		return printJSON(out, r)
	}

	fmt.Fprintf(out, "rev: %d\n", r.Rev)
	if r.Moved {
		fmt.Fprintln(out, "note: the anchor revision has moved")
	}
	s := r.Session
	fmt.Fprintf(out, "%s\n%s on %s, %s, %d messages, updated %s\n", oneLine(s.Title, 100),
		s.Agent, s.Node, s.ProjectPath, s.Messages, s.UpdatedAt.Local().Format(time.DateTime))
	if s.Tokens != nil {
		raw, _ := json.Marshal(s.Tokens)
		fmt.Fprintf(out, "tokens: %s\n", raw)
	}
	for _, path := range s.Files {
		fmt.Fprintf(out, "file: %s\n", path)
	}
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
		if m.Kind != "" {
			head += " [" + m.Kind + "]"
		}
		fmt.Fprintf(out, "\n--- %s\n%s\n", head, m.Text)
		for _, call := range m.Calls {
			status := ""
			if call.Error {
				status = " [error]"
			}
			fmt.Fprintf(out, "  call %s: %s%s\n", call.Call, call.Tool, status)
			if call.Agent != "" {
				fmt.Fprintf(out, "    agent: %s\n", call.Agent)
			}
			if call.Input != "" {
				fmt.Fprintf(out, "    input: %s\n", call.Input)
			}
			if call.Output != "" {
				fmt.Fprintf(out, "    output: %s\n", call.Output)
			}
			for _, file := range call.Files {
				fmt.Fprintf(out, "    file: %s %s\n", file.Op, file.Path)
			}
			if call.Truncated {
				fmt.Fprintln(out, "    (call truncated)")
			}
		}
		if m.CallsDropped > 0 {
			fmt.Fprintf(out, "  (%d calls omitted)\n", m.CallsDropped)
		}
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

// exportSessions reads the local snapshot log over the control socket.
func exportSessions(dir string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("h8c export", flag.ContinueOnError)
	epoch := fs.String("epoch", "", "epoch of the delivered and acknowledged cursors")
	after := fs.Int64("after", 0, "last delivered export cursor")
	since := fs.Int64("since", 0, "initial watermark in Unix milliseconds")
	ack := fs.Int64("ack", 0, "last durably accepted cursor to prune")
	limit := fs.Int("limit", 100, "maximum items, at most 500")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseAnywhere(fs, args); err != nil {
		return err
	}
	q := url.Values{"after": {strconv.FormatInt(*after, 10)}, "limit": {strconv.Itoa(*limit)}, "epoch": {*epoch}}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "since" {
			q.Set("since", strconv.FormatInt(*since, 10))
		}
		if f.Name == "ack" {
			q.Set("ack", strconv.FormatInt(*ack, 10))
		}
	})
	var page sessions.ExportPage
	if err := control.Dial(filepath.Join(dir, node.ControlSocket)).Get(context.Background(), "/v1/sessions/export?"+q.Encode(), &page); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, page)
	}
	fmt.Fprintf(out, "epoch: %s\n", page.Epoch)
	for _, item := range page.Items {
		fmt.Fprintf(out, "%d %s %s g=%d seq=%d rev=%d\n", item.Cursor, item.Type, item.Key, item.G, item.Seq, item.Rev)
	}
	fmt.Fprintf(out, "next: %d, more: %t\n", page.Next, page.More)
	return nil
}

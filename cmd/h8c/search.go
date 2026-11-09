package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/Hebi8/hebi8XC/internal/control"
	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/sessions"
)

func searchSessions(dir string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("h8c search", flag.ContinueOnError)
	q := sessions.SearchQuery{}
	fs.StringVar(&q.File, "file", "", "recorded exact path or relative path suffix")
	fs.StringVar(&q.Op, "op", "", "file operation: change, delete, read or any")
	fs.BoolVar(&q.Failed, "failed", false, "include failed file attempts")
	fs.StringVar(&q.Tool, "tool", "", "exact tool name")
	fs.StringVar(&q.Kind, "kind", "", "message kind")
	fs.BoolVar(&q.Errors, "errors", false, "only failed calls")
	fs.StringVar(&q.Agent, "agent", "", "exact agent name")
	fs.StringVar(&q.Project, "project", "", "project name or path contains")
	fs.StringVar(&q.Node, "node", "", "node name or id")
	fs.IntVar(&q.Limit, "limit", 50, "maximum results, at most 500")
	fs.BoolVar(&q.BySession, "by-session", false, "group hits by session")
	from := fs.String("since", "", "message time at or after duration/date")
	until := fs.String("until", "", "message time at or before duration/date")
	asJSON := fs.Bool("json", false, "print JSON")
	var err error
	q.Terms, err = parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	now := time.Now()
	for label, v := range map[string]struct {
		text string
		dst  *int64
	}{"since": {*from, &q.Since}, "until": {*until, &q.Until}} {
		if v.text != "" {
			t, e := since(v.text, now)
			if e != nil {
				return fmt.Errorf("--%s: invalid date or duration", label)
			}
			if label == "until" {
				if date, e := time.ParseInLocation(time.DateOnly, v.text, time.Local); e == nil {
					t = date.AddDate(0, 0, 1).Add(-time.Millisecond)
				}
			}
			*v.dst = t.UnixMilli()
		}
	}
	if err := q.Validate(); err != nil {
		return err
	}
	v := q.Values()
	if q.Node != "" {
		v.Set("node", q.Node)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var r sessions.SearchResult
	if err := control.Dial(filepath.Join(dir, node.ControlSocket)).Get(ctx, "/v1/sessions/search?"+v.Encode(), &r); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out, r)
	}
	for _, h := range r.Hits {
		call := ""
		if h.Call != nil {
			call = fmt.Sprintf(" call=%d tool=%s", *h.Call, h.Tool)
		}
		fmt.Fprintf(out, "%s id=%d rev=%d%s\n  %s\n", h.Ref, h.ID, h.Rev, call, oneLine(h.Snippet, 300))
	}
	for _, g := range r.Sessions {
		fmt.Fprintf(out, "%s rev=%d hits=%d first=%d last=%d %s\n", g.Ref, g.Rev, g.Hits, g.First, g.Last, oneLine(g.Title, 80))
	}
	for _, n := range r.Nodes {
		fmt.Fprintf(out, "%s: %s", n.Name, n.Searched)
		if n.Partial {
			fmt.Fprint(out, " (partial)")
		}
		if n.Note != "" {
			fmt.Fprint(out, ": ", n.Note)
		}
		fmt.Fprintln(out)
	}
	if r.More {
		fmt.Fprintln(out, "more matches; narrow the query or raise --limit")
	}
	return nil
}

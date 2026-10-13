// Command websmoke opens the web interface in headless Chrome and checks
// what Go tests cannot see: what the page's script does. Run it from the
// source tree whenever internal/web/static changes:
//
//	go run ./scripts/websmoke
//
// It starts the nodes of scripts/demo on free ports, and stops them. It
// checks that the "load more" buttons are gone at the end of a list, that
// nothing is shown twice, that an answer which comes late does not bring
// back the place in a list that is no longer asked for, and that text
// which tries to be markup stays text.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
	"github.com/Hebi8/hebi8XC/scripts/internal/demo"
)

//go:embed probe.js
var probe string

// The text that tries to run. Each piece would leave its own mark in the
// title of the page.
const (
	hostileKey     = "claude:hostile"
	hostileTitle   = `<img src=x onerror="document.title='PWNED-TITLE'">Refactor the importer`
	hostileProject = `<b onmouseover="document.title='PWNED-PROJECT'">importer</b>`
	hostileName    = "<svg onload=\"document.title=`PWNED-NODE`\">laptop"
	hostileScript  = `<script>document.title='PWNED-SCRIPT'</script>`
	hostileImage   = `<img src=x onerror="document.title='PWNED-MSG'">`
	hostileLink    = `javascript:document.title='PWNED-LINK'`
	hostileCode    = `<script>document.title='PWNED-CODE'</script>`
	honestLink     = "https://example.com/docs/importer?x=1"
)

func hostile(desk, _, _ *waketest.DB) {
	now := time.Now().UnixMilli()
	desk.Put(waketest.Session{Key: hostileKey, Agent: "claude", Title: hostileTitle, Project: hostileProject,
		Path: "/home/alice/src/importer", UpdatedAt: now - 30_000},
		waketest.Message{Role: "user", Time: now - 90_000,
			Text: "Please look at the importer.\n" + hostileScript + "\n" + hostileImage + "\nDocs: " + honestLink + ", and not a link: " + hostileLink},
		waketest.Message{Role: "assistant", Time: now - 80_000,
			Text: "The loop skips the last row:\n\n```go\nfor i := 0; i < len(rows)-1; i++ {\n\timport(rows[i]) // " + hostileCode + "\n}\n```\n\nChanging the bound fixes it."})
}

type checker struct {
	c      *chrome
	failed int
}

// check prints one line for one thing that has to hold.
func (k *checker) check(name string, ok bool, detail ...any) {
	if ok {
		fmt.Println("ok    " + name)
		return
	}
	k.failed++
	fmt.Println("FAIL  " + name + ": " + strings.TrimSpace(fmt.Sprintln(detail...)))
}

// js returns the value of an expression in the page.
func js[T any](k *checker, expr string) T {
	var v T
	if err := k.c.eval(expr, &v); err != nil {
		k.failed++
		fmt.Println("FAIL  " + err.Error())
	}
	return v
}

func (k *checker) await(cond string) {
	if err := k.c.await(cond); err != nil {
		k.failed++
		fmt.Println("FAIL  " + err.Error())
	}
}

func q(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type listing struct {
	IDs   []string `json:"ids"`
	Pages int      `json:"pages"`
	First int      `json:"first"`
}

func main() {
	os.Exit(run())
}

func run() int {
	bin := flag.String("chrome", "", "the Chrome to run (default: the one on PATH)")
	flag.Parse()
	path, err := findChrome(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "websmoke:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := demo.Start(ctx, demo.Options{Names: [3]string{"", hostileName, ""}, Seed: hostile})
	if err != nil {
		fmt.Fprintln(os.Stderr, "websmoke:", err)
		return 2
	}
	defer n.Close()
	c, err := startChrome(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "websmoke:", err)
		return 2
	}
	defer c.close()
	if err := c.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": probe}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "websmoke:", err)
		return 2
	}
	k := &checker{c: c}
	desk, laptop := n.Data.IDs[0], n.Data.IDs[1]
	home := n.URL[:strings.Index(n.URL, "?")]
	open := func(url string) {
		if err := c.call("Page.navigate", map[string]any{"url": url}, nil); err != nil {
			k.check("open the page", false, err)
		}
		k.await(`window.__t && __t.rows().length > 0 && !document.getElementById('more').disabled`)
	}

	fmt.Println("# the page opens")
	open(n.URL)
	k.check("the link leaves the browser at an address without the token", js[string](k, `location.href`) == home, js[string](k, `location.pathname + location.search.replace(/[0-9a-f]{16,}/, '<token>')`))
	title := js[string](k, `document.title`)

	fmt.Println("# text that tries to be markup stays text")
	k.c.eval(`document.querySelectorAll('#list *, #machines *').forEach(e => e.dispatchEvent(new MouseEvent('mouseover', {bubbles: true})))`, nil)
	k.c.eval(`__t.open(`+q(desk)+`, `+q(hostileKey)+`)`, nil)
	k.await(`__t.seqs().length === 2`)
	text := func(selector string) string {
		return js[string](k, `[...document.querySelectorAll(`+q(selector)+`)].map(e => e.textContent).join('\n')`)
	}
	now := js[string](k, `document.title`)
	stays := func(what, selector, payload, mark string) {
		k.check(what, strings.Contains(text(selector), payload) && !strings.Contains(now, mark),
			"shown as text:", strings.Contains(text(selector), payload), "title of the page:", now)
	}
	stays("a session title", "#list > li > .t", hostileTitle, "PWNED-TITLE")
	stays("a project name", "#list > li > .m", hostileProject, "PWNED-PROJECT")
	stays("a node name", "#machines, #node", hostileName, "PWNED-NODE")
	stays("a script element in a message", "#messages .text", hostileScript, "PWNED-SCRIPT")
	stays("an image with a handler in a message", "#messages .text", hostileImage, "PWNED-MSG")
	stays("a javascript: address in a message", "#messages .text", hostileLink, "PWNED-LINK")
	stays("a script element in a code block", "#messages pre", hostileCode, "PWNED-CODE")
	k.check("the title of the page is what it was", now == title, now)
	k.check("none of it became an element or a handler", js[int](k, `document.querySelectorAll('img, svg, iframe, object, embed, b, script').length`) == 1 &&
		js[int](k, `[...document.querySelectorAll('*')].filter(e => [...e.attributes].some(a => a.name.startsWith('on'))).length`) == 0,
		js[string](k, `[...document.querySelectorAll('img, svg, iframe, object, embed, b, script')].map(e => e.outerHTML.slice(0, 80)).join(' ')`))
	links := js[[]string](k, `[...document.querySelectorAll('#messages a')].map(a => a.protocol + ' ' + a.target + ' ' + a.rel + ' ' + a.textContent)`)
	k.check("the only link is the http one, and it opens apart from the page",
		reflect.DeepEqual(links, []string{"https: _blank noopener noreferrer " + honestLink}), links)

	fmt.Println("# the list, to its end")
	all := js[listing](k, `__t.sessions('')`)
	k.check("the demo fills more than one page", all.Pages > 1 && all.First == 50, all.Pages, "pages")
	k.check("\"load more\" is shown while there is more", js[bool](k, `__t.shown('more')`))
	for i := 1; i < all.Pages+3 && js[bool](k, `__t.shown('more')`); i++ {
		k.check(fmt.Sprintf("a click asks for page %d", i+1), js[int](k, `__t.press('more')`) == 1)
		k.await(`!document.getElementById('more').disabled`)
	}
	rows := js[[]string](k, `__t.rows()`)
	k.check("the list is the node's list, every session once", reflect.DeepEqual(rows, all.IDs), len(rows), "rows on the page,", len(all.IDs), "sessions")
	k.check("at the end \"load more\" is not shown", !js[bool](k, `__t.shown('more')`))
	k.check("at the end a click on it asks for nothing", js[int](k, `__t.press('more')`) == 0)

	fmt.Println("# a session, to its end")
	k.c.eval(`__t.open(`+q(laptop)+`, `+q(n.Data.Long)+`)`, nil)
	k.await(`__t.seqs().length === 50 && !document.getElementById('more-messages').disabled`)
	want := js[[]string](k, `__t.messages(`+q(laptop)+`, `+q(n.Data.Long)+`)`)
	k.check("the long session fills more than one page", len(want) > 50, len(want), "messages")
	k.check("\"load more\" is shown while there is more", js[bool](k, `__t.shown('more-messages')`))
	for i := 1; i < len(want)/50+3 && js[bool](k, `__t.shown('more-messages')`); i++ {
		k.check(fmt.Sprintf("a click asks for page %d", i+1), js[int](k, `__t.press('more-messages')`) == 1)
		k.await(`!document.getElementById('more-messages').disabled`)
	}
	seqs := js[[]string](k, `__t.seqs()`)
	k.check("the messages are the session's, every one once", reflect.DeepEqual(seqs, want), len(seqs), "on the page,", len(want), "in the session")
	k.check("at the end \"load more\" is not shown", !js[bool](k, `__t.shown('more-messages')`))
	k.check("at the end a click on it asks for nothing", js[int](k, `__t.press('more-messages')`) == 0)

	fmt.Println("# the answer to a refresh arrives before the answer to new filters")
	open(home)
	const agent = "codex"
	filtered := js[listing](k, `__t.sessions('agent=`+agent+`')`)
	k.check("the filter leaves some sessions and not all", filtered.First > 0 && len(filtered.IDs) < len(all.IDs), len(filtered.IDs))
	k.check("there is a place in the list to lose", js[bool](k, `__t.shown('more')`))
	from := js[int](k, `(__t.hold = true, __t.log.length)`)
	k.check("the refresh is asked for", js[int](k, `__t.press('refresh')`) == 1)
	k.c.eval(`(() => { const a = document.getElementById('agent'); a.value = `+q(agent)+`; a.dispatchEvent(new Event('input', {bubbles: true})); })()`, nil)
	k.check("once the filters change \"load more\" is not shown", !js[bool](k, `__t.shown('more')`))
	k.await(`__t.held.length === 2`)
	k.c.eval(`__t.release(0)`, nil)
	k.check("after the late answer \"load more\" is not shown", !js[bool](k, `__t.shown('more')`))
	k.check("after the late answer a click on it asks for nothing", js[int](k, `__t.press('more')`) == 0)
	k.c.eval(`__t.release(1)`, nil)
	rows = js[[]string](k, `__t.rows()`)
	k.check("the list is the first page for the new filters", reflect.DeepEqual(rows, filtered.IDs[:filtered.First]), len(rows), "rows,", filtered.First, "expected")
	k.check("\"load more\" is shown again only if that list goes on", js[bool](k, `__t.shown('more')`) == (filtered.Pages > 1))
	asked := js[[]string](k, `__t.log.slice(`+fmt.Sprint(from)+`).map(url => new URL(url).search)`)
	k.check("two requests were made, and none with the place in the old list",
		len(asked) == 2 && len(js[[]string](k, `__t.withCursor(`+fmt.Sprint(from)+`)`)) == 0, asked)

	fmt.Println("# the answer to \"load more\" arrives after another session was opened")
	k.c.eval(`__t.hold = false; __t.held = []; __t.open(`+q(laptop)+`, `+q(n.Data.Long)+`)`, nil)
	k.await(`__t.seqs().length === 50 && __t.shown('more-messages')`)
	short := js[[]string](k, `__t.messages(`+q(desk)+`, `+q(n.Data.Code)+`)`)
	from = js[int](k, `(__t.hold = true, __t.log.length)`)
	k.check("the next page is asked for", js[int](k, `__t.press('more-messages')`) == 1)
	k.c.eval(`__t.open(`+q(desk)+`, `+q(n.Data.Code)+`)`, nil)
	k.await(`__t.held.length === 2`)
	k.c.eval(`__t.release(0)`, nil)
	k.check("the late page is not added to the other session", js[int](k, `__t.seqs().length`) == 0)
	k.check("after the late answer \"load more\" is not shown", !js[bool](k, `__t.shown('more-messages')`))
	k.check("after the late answer a click on it asks for nothing", js[int](k, `__t.press('more-messages')`) == 0)
	k.c.eval(`__t.release(1)`, nil)
	seqs = js[[]string](k, `__t.seqs()`)
	k.check("the messages are those of the session that was opened", len(short) > 0 && reflect.DeepEqual(seqs, short), len(seqs), "on the page,", len(short), "in the session")
	k.check("two requests were made", js[int](k, `__t.log.length`)-from == 2)

	fmt.Println("# all along")
	k.check("no dialog opened and no exception went uncaught", len(c.troubles()) == 0, c.troubles())

	if k.failed > 0 {
		fmt.Printf("\n%d checks failed\n", k.failed)
		return 1
	}
	fmt.Println("\nall checks passed")
	return 0
}

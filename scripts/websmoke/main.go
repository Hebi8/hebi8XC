// Command websmoke opens the web interface in headless Chrome and checks
// what Go tests cannot see: what the page's script does. Run it from the
// source tree whenever internal/web/static changes:
//
//	go run ./scripts/websmoke
//
// It starts the nodes of scripts/demo on free ports, and stops them. It
// checks that the "load more" buttons are gone at the end of a list, that
// nothing is shown twice, that an answer which comes late does not bring
// back the place in a list that is no longer asked for, that text which
// tries to be markup stays text, calls and thinking included, and that a
// narrow window shows a session that cannot be read, and why, in the
// reader's language. Of the search it checks that the places found are
// the node's answer, that the page says how each machine was searched and
// what is missing, from what the node says of it and not from its note,
// when there is more, when nothing was found, when the node would refuse
// a search, when it refuses one in its own words and when it cannot be
// reached, that a refresh asks again for
// the search on its way, and that a place found opens its session there,
// marked and in view, with the messages before and after it each shown
// once, earlier ones offered as long as the node says there are some.
// Under a message it checks the calls: their tools, failures and files
// shown, input and output folded until a click; and that a record of text
// only says so.
//
// With -shots DIR it also writes pictures of the page there, narrow and
// wide, in English and in Chinese, to look at after a change of the
// layout. They are of the demo; they do not belong in the repository.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Hebi8/hebi8XC/internal/buildinfo"
	"github.com/Hebi8/hebi8XC/internal/sessions"
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
	hostileTool    = `<img src=x onerror="document.title='PWNED-TOOL'">`
	hostileInput   = `<script>document.title='PWNED-INPUT'</script>`
	hostileOutput  = `<img src=x onerror="document.title='PWNED-OUTPUT'">`
	hostilePath    = `/home/alice/notes/<svg onload="document.title='PWNED-PATH'">.md`
	hostileThought = `<img src=x onerror="document.title='PWNED-THINKING'">`
)

func hostile(desk, _, _ *waketest.DB) {
	now := time.Now().UnixMilli()
	desk.Put(waketest.Session{Key: hostileKey, Agent: "claude", Title: hostileTitle, Project: hostileProject,
		Path: "/home/alice/src/importer", UpdatedAt: now - 30_000},
		waketest.Message{Role: "user", Time: now - 90_000,
			Text: "Please look at the importer.\n" + hostileScript + "\n" + hostileImage + "\nDocs: " + honestLink + ", and not a link: " + hostileLink},
		waketest.Message{Role: "assistant", Time: now - 80_000,
			Text: "The loop skips the last row:\n\n```go\nfor i := 0; i < len(rows)-1; i++ {\n\timport(rows[i]) // " + hostileCode + "\n}\n```\n\nChanging the bound fixes it."})
	desk.Structure(hostileKey, 2, sessions.Message{Thinking: hostileThought, Calls: []sessions.Call{
		{Call: "c1", Tool: hostileTool, Input: hostileInput, Output: hostileOutput, Error: true,
			Files: []sessions.File{{Path: hostilePath, Op: "change"}}},
	}})
}

// Sessions whose messages are large as JSON though not in characters: the
// node writes each "<" as six bytes. Around a message of heavyKey it sends
// fewer messages than were asked for, and says with has_before that more
// come before them. Two messages of hugeKey are too large to be sent
// together, and an answer around the second always holds that one.
const (
	heavyKey = "claude:heavy"
	hugeKey  = "claude:huge"
)

func heavy(desk *waketest.DB) {
	now := time.Now().UnixMilli()
	const minute = 60_000
	var parts []waketest.Message
	for i := range 30 {
		parts = append(parts, waketest.Message{Role: "user", Time: now - int64(100-i)*minute,
			Text: fmt.Sprintf("Part %d of the page:\n", i+1) + strings.Repeat("<", 35_000)})
	}
	desk.Put(waketest.Session{Key: heavyKey, Agent: "claude", Title: "A page of markup, pasted in parts",
		Project: "atlas", Path: "/home/alice/src/atlas", UpdatedAt: now - 70*minute}, parts...)
	desk.Put(waketest.Session{Key: hugeKey, Agent: "claude", Title: "Two exports of the page, whole",
		Project: "atlas", Path: "/home/alice/src/atlas", UpdatedAt: now - 66*minute},
		waketest.Message{Role: "user", Time: now - 69*minute, Text: "Here are both exports."},
		waketest.Message{Role: "user", Time: now - 68*minute, Text: "The first:\n" + strings.Repeat("<", 190_000)},
		waketest.Message{Role: "user", Time: now - 67*minute, Text: "The second:\n" + strings.Repeat("<", 190_000)},
		waketest.Message{Role: "assistant", Time: now - 66*minute, Text: "They are the same."})
	// Like the rest of desk, they have their structure, of no calls.
	desk.SealStructure(heavyKey)
	desk.SealStructure(hugeKey)
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

// found is what the node itself answers to a search, with the places named
// as __t.places names those on the page.
type found struct {
	Places []string `json:"places"`
	More   bool     `json:"more"`
	Hits   []struct {
		ID  int64 `json:"id"`
		Rev int64 `json:"rev"`
	} `json:"hits"`
	Groups []struct {
		Hits int `json:"hits"`
	} `json:"groups"`
	Nodes []struct {
		ID   string `json:"id"`
		Note string `json:"note"`
	} `json:"nodes"`
}

// note is what the node said of the machine id in its answer.
func (f found) note(id string) string {
	for _, n := range f.Nodes {
		if n.ID == id {
			return n.Note
		}
	}
	return ""
}

// window is what the node itself answers when asked around a message.
type window struct {
	Seqs      []string `json:"seqs"`
	HasBefore bool     `json:"has_before"`
}

// message is a message of a session: its id, and the revision it was read at.
type message struct {
	ID  int64 `json:"id"`
	Rev int64 `json:"rev"`
}

// anchor is the message the page marks as a place found.
type anchor struct {
	Seq    string `json:"seq"`
	Badge  string `json:"badge"`
	Calls  []int  `json:"calls"`
	Open   bool   `json:"open"`
	InView bool   `json:"inView"`
}

// none is an answer to a search in which attic was not searched at all,
// as the node gives it when attic does not answer and nothing of it was
// read here before: no place on attic, and its note. The demo, where attic
// has a copy, does not come to that.
func none(attic string) string {
	return `__t.edit = (url, v) => url.includes('/sessions/search') ? { ...v,
		hits: v.hits.filter(h => h.node_id !== ` + q(attic) + `),
		sessions: v.sessions && v.sessions.filter(g => g.node_id !== ` + q(attic) + `),
		nodes: v.nodes.map(n => n.id === ` + q(attic) + ` ? { id: n.id, name: n.name, searched: 'none', note: 'peer search unavailable' } : n) } : v`
}

// reversed gives the search a range of dates from today to yesterday, the
// wrong way round, and returns how many requests the page made of it.
const reversed = `(() => {
	const ymd = d => d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
	const now = new Date(), yesterday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
	const before = __t.log.length;
	const s = document.getElementById('s-when');
	s.value = 'range';
	s.dispatchEvent(new Event('change', { bubbles: true }));
	document.getElementById('s-from').value = ymd(now);
	const to = document.getElementById('s-to');
	to.value = ymd(yesterday);
	to.dispatchEvent(new Event('change', { bubbles: true }));
	return __t.log.length - before; })()`

// aDayAgo is a time a day before now, to the minute, as JavaScript in the
// page: dayAgo, a number of milliseconds, and its forms in a node's note,
// utc and iso. shown is how the page writes it.
const aDayAgo = `const dayAgo = Math.floor((Date.now() - 864e5) / 6e4) * 6e4;
	const iso = new Date(dayAgo).toISOString().replace('.000Z', 'Z');
	const utc = iso.replace('T', ' ').replace('Z', ' UTC');
	const shown = (() => {
		const form = { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' };
		if (new Date(dayAgo).getFullYear() !== new Date().getFullYear()) form.year = 'numeric';
		return new Date(dayAgo).toLocaleString(document.documentElement.lang, form);
	})();`

// covered is an answer to a search in which laptop searched its message
// text in full, and its thinking and full tool inputs only back to a day
// ago, as a node says it, in scanned_since and in its note. It returns
// that time as the page writes it.
func covered(laptop string) string {
	return `(() => {
		` + aDayAgo + `
		__t.edit = (url, v) => url.includes('/sessions/search') ? { ...v, nodes: v.nodes.map(n => n.id === ` + q(laptop) + ` ? { ...n, scanned_since: dayAgo,
			note: 'message text searched completely; thinking and full tool inputs searched since ' + utc + '; add --since ' + iso + ' to narrow this scan' } : n) } : v;
		return shown;
	})()`
}

// The notes of desk and laptop in an answer edited by spent: each ran out
// of a budget of its own.
const (
	deskNote   = "messages examined through 2026-11-09T08:00:00Z; search time budget exhausted; results are incomplete; add --project or a narrower --since/--until range"
	laptopNote = "search candidate limit reached; results are incomplete; add --project or a narrower --since/--until range"
	atticNote  = "searched held copies; search time budget exhausted; results are incomplete"
)

// spent is an answer to a search in which desk and laptop each ran out of
// a budget, with a note of its own, and laptop searched its thinking and
// full tool inputs only back to a day ago. Attic's note speaks of
// incomplete results, but attic does not say partial. It returns the time
// of laptop's cutoff as the page writes it.
func spent(desk, laptop, attic string) string {
	return `(() => {
		` + aDayAgo + `
		const edits = {
			` + q(desk) + `: { partial: true, note: ` + q(deskNote) + ` },
			` + q(laptop) + `: { partial: true, scanned_since: dayAgo,
				note: ` + q(laptopNote) + ` + '; thinking and full tool inputs searched since ' + utc + '; add --since ' + iso + ' to narrow this scan' },
			` + q(attic) + `: { note: ` + q(atticNote) + ` },
		};
		__t.edit = (url, v) => url.includes('/sessions/search') ? { ...v, nodes: v.nodes.map(n => ({ ...n, ...edits[n.id] })) } : v;
		return shown;
	})()`
}

func main() {
	os.Exit(run())
}

func run() int {
	bin := flag.String("chrome", "", "the Chrome to run (default: the one on PATH)")
	shots := flag.String("shots", "", "a directory to write pictures of the page to")
	flag.Parse()
	path, err := findChrome(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "websmoke:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := demo.Start(ctx, demo.Options{Names: [3]string{"", hostileName, ""}, Seed: func(desk, laptop, attic *waketest.DB) {
		hostile(desk, laptop, attic)
		heavy(desk)
	}})
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
	desk, laptop, attic := n.Data.IDs[0], n.Data.IDs[1], n.Data.IDs[2]
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

	fmt.Println("# calls and thinking that try to be markup stay text")
	k.check("folded away, they are not in the page yet", js[int](k, `document.querySelectorAll('#messages .calls pre, #messages .thinking .text').length`) == 0)
	k.c.eval(`document.querySelectorAll('#messages details').forEach(d => { d.open = true; })`, nil)
	k.await(`document.querySelectorAll('#messages .calls pre').length === 2 && document.querySelector('#messages .thinking .text')`)
	now = js[string](k, `document.title`)
	stays("a tool name", "#messages .calls .tool", hostileTool, "PWNED-TOOL")
	stays("the input of a call", "#messages .calls pre", hostileInput, "PWNED-INPUT")
	stays("the output of a call", "#messages .calls pre", hostileOutput, "PWNED-OUTPUT")
	stays("the path of a file", "#messages .calls .path", hostilePath, "PWNED-PATH")
	stays("thinking", "#messages .thinking .text", hostileThought, "PWNED-THINKING")
	k.check("the title of the page is still what it was", now == title, now)
	k.check("none of that became an element or a handler either", js[int](k, `document.querySelectorAll('img, svg, iframe, object, embed, b, script').length`) == 1 &&
		js[int](k, `[...document.querySelectorAll('*')].filter(e => [...e.attributes].some(a => a.name.startsWith('on'))).length`) == 0)

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
	k.check("while it is, \"refresh\" cannot be used", js[bool](k, `document.getElementById('refresh').disabled`))
	k.c.eval(`(() => { const a = document.getElementById('agent'); a.value = `+q(agent)+`; a.dispatchEvent(new Event('input', {bubbles: true})); })()`, nil)
	k.check("once the filters change \"load more\" is not shown", !js[bool](k, `__t.shown('more')`))
	k.await(`__t.held.length === 2`)
	k.c.eval(`__t.release(0)`, nil)
	k.check("after the late answer \"load more\" is not shown", !js[bool](k, `__t.shown('more')`))
	k.check("after the late answer a click on it asks for nothing", js[int](k, `__t.press('more')`) == 0)
	k.check("with its answer \"refresh\" can be used again", !js[bool](k, `document.getElementById('refresh').disabled`))
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

	d := n.Data
	searched := func() { k.await(`__t.searched()`) }
	// row is the text of what the page says of one machine in a search.
	row := func(id string) string {
		return js[string](k, `(() => { const e = document.querySelector('#search-nodes [data-node="`+id+`"]'); return e ? e.closest('li').textContent : ''; })()`)
	}

	fmt.Println("# a search over all three machines")
	open(home)
	k.check("the search is a view of its own", js[int](k, `__t.press('mode-search')`) == 0 &&
		js[bool](k, `__t.shown('search') && !__t.shown('list')`))
	intro := js[string](k, `document.getElementById('search-note').textContent`)
	k.check("with nothing to look for nothing is asked", js[int](k, `__t.submit()`) == 0 &&
		js[string](k, `document.getElementById('search-note').textContent`) != intro, intro)
	k.check("typing asks nothing yet", js[int](k, `__t.type('s-q', `+q(d.Term)+`)`) == 0)
	k.check("Enter asks once", js[int](k, `__t.submit()`) == 1)
	searched()
	term := js[found](k, `__t.found('q=`+d.Term+`')`)
	places := js[[]string](k, `__t.places()`)
	k.check("the places found are the node's answer, in its order", len(term.Places) > 0 && reflect.DeepEqual(places, term.Places), places, term.Places)
	on := map[string]bool{}
	for _, p := range places {
		on[p[:strings.Index(p, "/")]] = true
	}
	k.check("they are on all three machines", on[desk] && on[laptop] && on[attic], places)
	states := js[map[string]string](k, `__t.states()`)
	k.check("desk and laptop answered it themselves", states[desk] == "live" && states[laptop] == "live", states)
	// Attic keeps an index of text only. For words that is enough: what is
	// held of it is searched in full.
	k.check("attic was searched in the copies kept of it, in full for words", states[attic] == "copies", states)
	k.check("the page says what was searched of attic, and nothing more", strings.Contains(row(attic), "only the sessions read on this machine before") &&
		!strings.Contains(row(attic), "may be missing"), row(attic))
	k.check("a call found names its tool and its files, from the project", js[bool](k, `__t.found('q=`+d.Term+`').then(f => {
		const calls = f.hits.filter(h => h.call !== undefined);
		return calls.length > 0 && calls.some(h => (h.files || []).length) && calls.every(h => {
			const li = [...document.querySelectorAll('#hits > li')].find(li => li.dataset.key === h.key && li.dataset.at === String(h.id) && li.dataset.call === String(h.call));
			const files = h.files || [];
			const inside = p => h.project_path && p.startsWith(h.project_path + '/') ? p.slice(h.project_path.length + 1) : p;
			return li.querySelector('.call .tool').textContent === h.tool &&
				[...li.querySelectorAll('.call .file')].map(e => e.title).join('\n') === files.map(f => f.path).join('\n') &&
				[...li.querySelectorAll('.call .file .path')].map(e => e.textContent).join('\n') === files.map(f => inside(f.path)).join('\n') &&
				files.every(f => inside(f.path) !== f.path);
		});
	})`))
	k.check("each place names the agent and the project the node gave it", js[bool](k, `__t.found('q=`+d.Term+`').then(f =>
		f.hits.length > 0 && f.hits.every((h, i) => h.agent && document.querySelectorAll('#hits > li')[i].querySelector('.grow').textContent === [h.agent, h.project].filter(Boolean).join(' · ')))`))
	k.check("in every snippet the word found is marked", js[bool](k, `[...document.querySelectorAll('#hits .snip')].every(p =>
		p.querySelectorAll('mark').length > 0 && [...p.querySelectorAll('mark')].every(m => m.textContent.toLowerCase() === `+q(d.Term)+`))`))
	now = js[string](k, `document.title`)
	stays("a session title among the places found", "#hits .t", hostileTitle, "PWNED-TITLE")
	stays("a project name among the places found", "#hits .m", hostileProject, "PWNED-PROJECT")

	fmt.Println("# grouped by session")
	k.check("a choice in the form asks again at once", js[int](k, `__t.press('s-group')`) == 1)
	searched()
	grouped := js[found](k, `__t.found('q=`+d.Term+`&by_session=true')`)
	places = js[[]string](k, `__t.places()`)
	k.check("one row per session, as the node grouped them", len(grouped.Places) > 1 && reflect.DeepEqual(places, grouped.Places), places, grouped.Places)
	var counts []string
	for _, g := range grouped.Groups {
		if g.Hits == 1 {
			counts = append(counts, "1 match")
		} else {
			counts = append(counts, fmt.Sprintf("%d matches", g.Hits))
		}
	}
	shownCounts := js[[]string](k, `[...document.querySelectorAll('#hits > li')].map(li => li.querySelector('.count').textContent)`)
	k.check("each row says how many matches the node counted in it", len(counts) > 1 && reflect.DeepEqual(shownCounts, counts) &&
		strings.Join(counts, "") != strings.Repeat("1 match", len(counts)), shownCounts, counts)
	k.check("above them, how many sessions there are", js[string](k, `document.getElementById('search-note').textContent`) == fmt.Sprintf("%d sessions", len(grouped.Places)))
	k.check("unticked, it asks again for the places one by one", js[int](k, `__t.press('s-group')`) == 1)
	searched()
	k.check("and those are listed again", reflect.DeepEqual(js[[]string](k, `__t.places()`), term.Places))

	fmt.Println("# a range of dates")
	k.check("choosing to give dates asks nothing yet, and shows where to give them", js[int](k, `(() => {
		const before = __t.log.length;
		const s = document.getElementById('s-when');
		s.value = 'range';
		s.dispatchEvent(new Event('change', { bubbles: true }));
		return __t.log.length - before; })()`) == 0 && js[bool](k, `__t.shown('s-from') && __t.shown('s-to')`))
	// From yesterday to today, as days of the calendar here: everything of
	// desk and laptop is in it, and attic's copy, weeks old, is not.
	k.check("the second date asks at once", js[int](k, `(() => {
		const ymd = d => d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
		const now = new Date(), yesterday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
		document.getElementById('s-from').value = ymd(yesterday);
		const before = __t.log.length;
		const to = document.getElementById('s-to');
		to.value = ymd(now);
		to.dispatchEvent(new Event('change', { bubbles: true }));
		return __t.log.length - before; })()`) == 1)
	searched()
	span := js[[]int64](k, `(() => {
		const u = new URL(__t.log[__t.log.length - 1]);
		const now = new Date();
		return [Number(u.searchParams.get('since')), Number(u.searchParams.get('until')),
			new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1).getTime(), new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1).getTime() - 1]; })()`)
	k.check("it asks from the start of the first day to the end of the last", len(span) == 4 && span[0] == span[2] && span[1] == span[3], span)
	dated := js[found](k, fmt.Sprintf(`__t.found('q=%s&since=%d&until=%d')`, d.Term, span[0], span[1]))
	places = js[[]string](k, `__t.places()`)
	k.check("what is found is the node's answer for those days, and attic's old copy is not in it", reflect.DeepEqual(places, dated.Places) &&
		len(places) > 0 && !strings.Contains(strings.Join(places, " "), attic+"/"), places)

	fmt.Println("# a refresh in the search")
	from = js[int](k, `__t.log.length`)
	k.check("\"refresh\" asks for the list and again for the search on screen", js[int](k, `__t.press('refresh')`) == 2 &&
		js[string](k, `new URL(__t.log[__t.log.length - 1]).search`) == js[string](k, `new URL(__t.log[`+fmt.Sprint(from-1)+`]).search`),
		js[[]string](k, `__t.log.slice(`+fmt.Sprint(from-1)+`)`))
	k.await(`!document.getElementById('refresh').disabled`)
	k.check("and lists the same places", reflect.DeepEqual(js[[]string](k, `__t.places()`), dated.Places))
	k.c.eval(`(() => { const s = document.getElementById('s-when'); s.value = ''; s.dispatchEvent(new Event('change', { bubbles: true })); })()`, nil)
	searched()
	k.check("back to any time, the range is put away and attic is found again", !js[bool](k, `__t.shown('s-from')`) &&
		reflect.DeepEqual(js[[]string](k, `__t.places()`), term.Places))

	fmt.Println("# a refresh while a new search is on its way")
	broad := js[found](k, `__t.found('q=`+d.Broad+`')`)
	js[int](k, `__t.type('s-q', `+q(d.Broad)+`)`)
	from = js[int](k, `(__t.hold = true, __t.log.length)`)
	k.check("the new search is asked", js[int](k, `__t.submit()`) == 1)
	k.check("\"refresh\" asks for the list and for the search on its way, not the one on screen", js[int](k, `__t.press('refresh')`) == 2 &&
		reflect.DeepEqual(js[[]string](k, `new URL(__t.log[__t.log.length - 1]).searchParams.getAll('q')`), []string{d.Broad}) &&
		js[string](k, `new URL(__t.log[__t.log.length - 1]).searchParams.get('limit')`) == "50",
		js[[]string](k, `__t.log.slice(`+fmt.Sprint(from)+`).map(url => new URL(url).search)`))
	k.await(`__t.held.length === 3`)
	k.c.eval(`__t.release(0)`, nil)
	k.check("the first answer to it is not shown: the refresh asked after it", len(js[[]string](k, `__t.places()`)) == 0 &&
		js[bool](k, `document.getElementById('s-go').disabled`))
	k.c.eval(`__t.release(1)`, nil)
	k.c.eval(`__t.release(2)`, nil)
	k.await(`!document.getElementById('refresh').disabled`)
	k.check("the places on screen are those of the new search, with its word marked", reflect.DeepEqual(js[[]string](k, `__t.places()`), broad.Places) &&
		js[bool](k, `[...document.querySelectorAll('#hits .snip mark')].every(m => m.textContent.toLowerCase() === `+q(d.Broad)+`)`) &&
		js[string](k, `document.getElementById('s-q').value`) == d.Broad, js[[]string](k, `__t.places()`))
	k.c.eval(`__t.hold = false; __t.held = []`, nil)
	js[int](k, `__t.type('s-q', `+q(d.Term)+`)`)
	js[int](k, `__t.submit()`)
	searched()

	fmt.Println("# more than one answer holds")
	js[int](k, `__t.type('s-q', `+q(d.Broad)+`)`)
	k.check("a new search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.check("50 places are listed, and the page says there are more", len(js[[]string](k, `__t.places()`)) == 50 &&
		js[bool](k, `__t.shown('search-more') && __t.shown('search-wider')`))
	k.check("\"show up to 500\" asks once, for 500", js[int](k, `__t.press('search-wider')`) == 1 &&
		js[string](k, `new URL(__t.log[__t.log.length - 1]).searchParams.get('limit')`) == "500")
	searched()
	wide := js[found](k, `__t.found('q=`+d.Broad+`&limit=500')`)
	places = js[[]string](k, `__t.places()`)
	k.check("then all of them are listed, and nothing is said to be left out", len(places) > 50 && !wide.More &&
		reflect.DeepEqual(places, wide.Places) && !js[bool](k, `__t.shown('search-more')`), len(places), len(wide.Places))

	fmt.Println("# a machine that was not searched")
	k.c.eval(none(attic), nil)
	js[int](k, `__t.type('s-q', `+q(d.Term)+`)`)
	k.check("the search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.c.eval(`__t.edit = null`, nil)
	k.check("the page says it was not searched", js[map[string]string](k, `__t.states()`)[attic] == "none")
	k.check("and why, and what the node said of it", strings.Contains(row(attic), "not searched") &&
		strings.Contains(row(attic), "none of its sessions was read on this machine before") &&
		strings.HasSuffix(row(attic), "It says: peer search unavailable"), row(attic))
	k.check("nothing of it is listed, and the rest is", !strings.Contains(strings.Join(js[[]string](k, `__t.places()`), " "), attic+"/") &&
		len(js[[]string](k, `__t.places()`)) == len(term.Places)-strings.Count(strings.Join(term.Places, " "), attic+"/"), js[[]string](k, `__t.places()`))
	k.check("a search not done everywhere that finds something says what it found", js[string](k, `__t.note()`) == fmt.Sprintf("%d matches", len(js[[]string](k, `__t.places()`))), js[string](k, `__t.note()`))

	fmt.Println("# a machine whose thinking was searched only so far back")
	side := js[string](k, covered(laptop))
	k.check("the search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.c.eval(`__t.edit = null`, nil)
	k.check("it searched live and in full, as it says, and shares the line of the others", js[map[string]string](k, `__t.states()`)[laptop] == "live" &&
		js[int](k, `document.querySelectorAll('#search-nodes li.short [data-node="`+laptop+`"], #search-nodes li.short[data-node="`+laptop+`"]').length`) == 0,
		js[map[string]string](k, `__t.states()`))
	sides := js[[]string](k, `[...document.querySelectorAll('#search-nodes li.live .side')].map(e => e.textContent)`)
	k.check("a quiet line under it says how far back its thinking and full tool inputs were searched, in the reader's time",
		side != "" && reflect.DeepEqual(sides, []string{d.Names[1] + ": Message text searched in full; thinking and full tool inputs only back to " + side + "."}), sides, side)
	k.check("its note, which says the same for the command line, is not shown", !strings.Contains(row(laptop), "--since"), row(laptop))
	// Its message text was searched in full: nothing found there is so.
	js[string](k, covered(laptop))
	js[int](k, `__t.type('s-q', 'zyzzyva')`)
	k.check("a search that finds nothing is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.c.eval(`__t.edit = null`, nil)
	k.check("nothing found there is said plainly, with the line under it", js[string](k, `__t.note()`) == "Nothing found." &&
		js[int](k, `document.querySelectorAll('#search-nodes li.live .side').length`) == 1, js[string](k, `__t.note()`))
	js[int](k, `__t.type('s-q', `+q(d.Term)+`)`)

	fmt.Println("# machines that each ran out of a budget")
	cut := js[string](k, spent(desk, laptop, attic))
	k.check("the search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.c.eval(`__t.edit = null`, nil)
	states = js[map[string]string](k, `__t.states()`)
	k.check("each says it is incomplete", states[desk] == "live+partial" && states[laptop] == "live+partial" &&
		strings.Contains(row(desk), "searched live · incomplete") && strings.Contains(row(laptop), "searched live · incomplete"), states)
	k.check("each with its own note beside it, as it came", strings.HasSuffix(row(desk), "It says: "+deskNote) &&
		strings.Contains(row(laptop), "It says: "+laptopNote+"; thinking and full tool inputs searched since ") &&
		!strings.Contains(row(desk), laptopNote) && !strings.Contains(row(laptop), deskNote), row(desk), row(laptop))
	k.check("laptop's thinking was searched only so far back, and nothing is said of its message text", cut != "" &&
		strings.Contains(row(laptop), "Thinking and full tool inputs only back to "+cut+".") && !strings.Contains(row(laptop), "Message text searched in full"), row(laptop))
	k.check("nothing is read from a note: desk's says when it examined messages up to, and the page gives no time of its own",
		!strings.Contains(row(desk), "only back to") && !strings.Contains(row(desk), "older than"), row(desk))
	k.check("a note alone changes nothing: attic's speaks of incomplete results, and attic is not said to be incomplete", states[attic] == "copies" &&
		js[string](k, `document.querySelector('#search-nodes li[data-node="`+attic+`"] .state').textContent`) == "copies only", states)

	fmt.Println("# a file, of which attic's copies cannot say")
	js[int](k, `__t.type('s-q', '')`)
	js[int](k, `__t.type('s-file', `+q(d.File)+`)`)
	k.check("the search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	byFile := js[found](k, `__t.found('file=`+url.QueryEscape(d.File)+`')`)
	k.check("the places found are the node's answer", len(byFile.Places) > 0 && reflect.DeepEqual(js[[]string](k, `__t.places()`), byFile.Places), js[[]string](k, `__t.places()`), byFile.Places)
	k.check("attic's copies have no calls: that is said to be incomplete, with the node's note beside it as it came",
		js[map[string]string](k, `__t.states()`)[attic] == "copies+partial" && strings.Contains(row(attic), "copies only · incomplete") &&
			byFile.note(attic) != "" && strings.HasSuffix(row(attic), "It says: "+byFile.note(attic)), row(attic))
	// The change to the file is made in the second message of Code: what
	// the node sends around it reaches the first.
	near := -1
	for i, p := range byFile.Places {
		if strings.HasPrefix(p, desk+"/"+d.Code+"#") {
			near = i
		}
	}
	if near < 0 || near >= len(byFile.Hits) {
		k.check("the change is found in Code", false, byFile.Places)
	} else {
		k.c.eval(`__t.choose(`+q(byFile.Places[near])+`)`, nil)
		k.await(`__t.anchor() && !document.getElementById('earlier').disabled`)
		start := js[window](k, fmt.Sprintf(`__t.around(%s, %s, %d, 10, 10)`, q(desk), q(d.Code), byFile.Hits[near].ID))
		k.check("a place near the start: the node says nothing comes before it, and \"earlier messages\" is not shown",
			!start.HasBefore && js[anchor](k, `__t.anchor()`).Seq == "#2" && reflect.DeepEqual(js[[]string](k, `__t.seqs()`), start.Seqs) &&
				len(start.Seqs) == 5 && !js[bool](k, `__t.shown('earlier')`), start, js[[]string](k, `__t.seqs()`))
	}

	fmt.Println("# nothing found")
	js[int](k, `__t.type('s-file', '')`)
	js[int](k, `__t.type('s-q', 'zyzzyva')`)
	k.check("the search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.check("searched in full, the page says nothing was found", len(js[[]string](k, `__t.places()`)) == 0 && js[string](k, `__t.note()`) == "Nothing found." &&
		js[map[string]string](k, `__t.states()`)[attic] == "copies", js[string](k, `__t.note()`))
	js[int](k, `__t.type('s-q', '')`)
	js[int](k, `__t.type('s-file', 'importer/missing.go')`)
	k.check("a file found nowhere is asked for", js[int](k, `__t.submit()`) == 1)
	searched()
	k.check("searched in part, the page says that is no proof there is none", len(js[[]string](k, `__t.places()`)) == 0 &&
		js[string](k, `__t.note()`) == "Nothing found. But the search was not complete, so that does not mean there is nothing." &&
		js[map[string]string](k, `__t.states()`)[attic] == "copies+partial", js[string](k, `__t.note()`), js[map[string]string](k, `__t.states()`))
	js[int](k, `__t.type('s-file', '')`)

	fmt.Println("# a search the node would refuse, and a node that cannot be reached")
	js[int](k, `__t.type('s-q', `+q(d.Term)+`)`)
	// nothing is the page with no place found and no machine listed.
	nothing := func() bool {
		return len(js[[]string](k, `__t.places()`)) == 0 && !js[bool](k, `__t.shown('search-nodes')`) && !js[bool](k, `__t.shown('search-more')`)
	}
	k.check("dates the wrong way round ask nothing, and the page says why", js[int](k, reversed)+js[int](k, `__t.submit()`) == 0 &&
		js[string](k, `__t.note()`) == "The start date cannot be after the end date." && nothing(), js[string](k, `__t.note()`))
	k.c.eval(`(() => { const s = document.getElementById('s-when'); s.value = ''; s.dispatchEvent(new Event('change', { bubbles: true })); })()`, nil)
	searched()
	k.check("33 words ask nothing, and the page says why", js[int](k, `__t.type('s-q', Array.from({ length: 33 }, (_, i) => 'w' + i).join(' '))`)+js[int](k, `__t.submit()`) == 0 &&
		js[string](k, `__t.note()`) == "At most 32 words can be searched for; leave some out." && nothing(), js[string](k, `__t.note()`))
	k.check("words longer than the node takes ask nothing, and the page says why", js[int](k, `__t.type('s-q', 'x'.repeat(4097))`)+js[int](k, `__t.submit()`) == 0 &&
		js[string](k, `__t.note()`) == "The words to find are too long: 4096 bytes at most." && nothing(), js[string](k, `__t.note()`))
	js[int](k, `__t.type('s-q', `+q(d.Term)+`)`)
	k.c.eval(`__t.fail = true`, nil)
	k.check("when the node cannot be reached, the search is tried", js[int](k, `__t.submit()`) == 1)
	searched()
	k.c.eval(`__t.fail = false`, nil)
	k.check("and the page says so in its own words", js[string](k, `__t.note()`) == "Could not search: The h8c node on this machine cannot be reached." && nothing(),
		js[string](k, `__t.note()`))
	// A machine that has left the node's list since the page listed it: a
	// refusal the page does not foresee is said as the node said it.
	k.c.eval(`(() => { const s = document.getElementById('s-node'); s.append(new Option('gone', 'gone')); s.value = 'gone'; })()`, nil)
	k.check("a machine the node no longer knows is asked for", js[int](k, `__t.submit()`) == 1)
	searched()
	k.check("the page says what the node said, as it came", js[string](k, `__t.note()`) == `Could not search: no node "gone"` && nothing(),
		js[string](k, `__t.note()`))
	k.c.eval(`(() => { const s = document.getElementById('s-node'); s.value = ''; s.querySelector('option[value="gone"]').remove(); })()`, nil)
	k.check("it can be asked again at once", !js[bool](k, `document.getElementById('s-go').disabled`))
	k.check("and then it is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.check("and finds what it found before", reflect.DeepEqual(js[[]string](k, `__t.places()`), term.Places))

	fmt.Println("# a place found opens its session there")
	js[int](k, `__t.type('s-q', `+q(`"`+d.Anchor+`"`)+`)`)
	k.check("the search is asked", js[int](k, `__t.submit()`) == 1)
	k.check("words in quotes are one term", js[[]string](k, `new URL(__t.log[__t.log.length - 1]).searchParams.getAll('q')`)[0] == d.Anchor,
		js[[]string](k, `new URL(__t.log[__t.log.length - 1]).searchParams.getAll('q')`))
	searched()
	place := js[found](k, `__t.found('q=`+url.QueryEscape(d.Anchor)+`')`)
	k.check("one place is found: a call, deep in the long session", len(place.Places) == 1 && len(place.Hits) == 1 &&
		strings.HasPrefix(place.Places[0], laptop+"/"+d.Long+"#") && strings.HasSuffix(place.Places[0], ".0"), place.Places)
	if len(place.Places) == 1 && len(place.Hits) == 1 {
		whole := js[[]string](k, `__t.messages(`+q(laptop)+`, `+q(d.Long)+`)`)
		k.c.eval(`__t.choose(`+q(place.Places[0])+`)`, nil)
		k.await(`__t.anchor() && !document.getElementById('earlier').disabled`)
		a := js[anchor](k, `__t.anchor()`)
		k.check("its message is marked, and in view", a.Seq == "#200" && a.InView, fmt.Sprintf("%+v", a))
		k.check("the call found is marked in it, and opened", reflect.DeepEqual(a.Calls, []int{0}) && a.Open, fmt.Sprintf("%+v", a))
		seqs = js[[]string](k, `__t.seqs()`)
		k.check("ten messages before it and ten after it are shown, every one once", len(whole) == 260 && reflect.DeepEqual(seqs, whole[189:210]), seqs)
		k.check("nothing says it may have moved", a.Badge == "match" && !strings.Contains(js[string](k, `document.getElementById('notes').textContent`), "moved"))
		k.check("among the places found it is the one marked", js[int](k, `document.querySelectorAll('#hits > li.selected').length`) == 1)
		k.check("the address names the place", regexp.MustCompile(`&at=\d+&rev=\d+&call=0$`).MatchString(js[string](k, `location.hash`)), js[string](k, `location.hash`))
		k.check("\"earlier messages\" is shown", js[bool](k, `__t.shown('earlier')`))
		k.check("a click on it asks once", js[int](k, `__t.press('earlier')`) == 1)
		k.await(`!document.getElementById('earlier').disabled`)
		seqs = js[[]string](k, `__t.seqs()`)
		k.check("fifty earlier messages come above, every one once", reflect.DeepEqual(seqs, whole[139:210]), seqs)
		for i := 0; i < 10 && js[bool](k, `__t.shown('more-messages')`); i++ {
			k.check("\"load more\" asks once", js[int](k, `__t.press('more-messages')`) == 1)
			k.await(`!document.getElementById('more-messages').disabled`)
		}
		seqs = js[[]string](k, `__t.seqs()`)
		k.check("on to the end, every message once", reflect.DeepEqual(seqs, whole[139:]), len(seqs))
		k.check("the place is still marked", js[anchor](k, `__t.anchor()`).Seq == "#200")
		k.check("chosen again, it is brought back into view without asking", js[int](k, `(() => {
			const before = __t.log.length;
			__t.choose(`+q(place.Places[0])+`);
			return __t.log.length - before; })()`) == 0 && js[anchor](k, `__t.anchor()`).InView)

		fmt.Println("# a place whose session has changed since")
		h := place.Hits[0]
		k.c.eval(fmt.Sprintf(`__t.at(%s, %s, %d, %d, 0)`, q(laptop), q(d.Long), h.ID, h.Rev+1000), nil)
		k.await(`__t.anchor() && __t.anchor().badge !== 'match'`)
		k.check("the page says the place may have moved", strings.Contains(js[string](k, `document.getElementById('notes').textContent`), "the match may have moved") &&
			js[anchor](k, `__t.anchor()`).Badge == "match, may have moved", js[string](k, `document.getElementById('notes').textContent`))
	}

	fmt.Println("# a window the node cut short by its size")
	at := js[message](k, `__t.message(`+q(desk)+`, `+q(heavyKey)+`, 20)`)
	cutShort := js[window](k, fmt.Sprintf(`__t.around(%s, %s, %d, 10, 10)`, q(desk), q(heavyKey), at.ID))
	k.check("the node sends fewer messages than were asked for, from the place on, and says more come before",
		at.ID > 0 && len(cutShort.Seqs) > 1 && len(cutShort.Seqs) < 11 && cutShort.Seqs[0] == "#20" && cutShort.HasBefore, fmt.Sprintf("%+v", cutShort))
	k.c.eval(fmt.Sprintf(`__t.at(%s, %s, %d, %d)`, q(desk), q(heavyKey), at.ID, at.Rev), nil)
	k.await(`__t.anchor() && !document.getElementById('earlier').disabled`)
	k.check("the page shows what the node sent, the place first", reflect.DeepEqual(js[[]string](k, `__t.seqs()`), cutShort.Seqs) &&
		js[anchor](k, `__t.anchor()`).Seq == "#20", js[[]string](k, `__t.seqs()`))
	k.check("and offers \"earlier messages\", as the node says there are some", js[bool](k, `__t.shown('earlier')`))
	heavyAll := js[[]string](k, `__t.messages(`+q(desk)+`, `+q(heavyKey)+`)`)
	from = js[int](k, `__t.log.length`)
	clicks := 0
	for clicks < 30 && js[bool](k, `__t.shown('earlier')`) {
		js[int](k, `__t.press('earlier')`)
		clicks++
		k.await(`!document.getElementById('earlier').disabled`)
	}
	k.check("more than one click is needed, and each asks once", clicks > 1 && js[int](k, `__t.log.length`)-from == clicks, clicks)
	k.check("the earlier messages come above, every one once, to the first", len(heavyAll) == 30 && 19+len(cutShort.Seqs) <= 30 &&
		reflect.DeepEqual(js[[]string](k, `__t.seqs()`), heavyAll[:19+len(cutShort.Seqs)]), js[[]string](k, `__t.seqs()`))
	k.check("at the first message \"earlier messages\" is not shown, and a click on it asks nothing",
		!js[bool](k, `__t.shown('earlier')`) && js[int](k, `__t.press('earlier')`) == 0)

	fmt.Println("# two messages too large to be fetched together")
	second := js[message](k, `__t.message(`+q(desk)+`, `+q(hugeKey)+`, 3)`)
	k.c.eval(fmt.Sprintf(`__t.at(%s, %s, %d, %d)`, q(desk), q(hugeKey), second.ID, second.Rev), nil)
	k.await(`__t.anchor() && !document.getElementById('earlier').disabled`)
	k.check("the node sends the place alone, and says more come before", second.ID > 0 &&
		reflect.DeepEqual(js[[]string](k, `__t.seqs()`), []string{"#3"}) && js[bool](k, `__t.shown('earlier')`), js[[]string](k, `__t.seqs()`))
	k.check("\"earlier messages\" asks once", js[int](k, `__t.press('earlier')`) == 1)
	k.await(`!document.getElementById('earlier').disabled`)
	k.check("nothing can come above: the page says so, and offers to read from the start instead",
		reflect.DeepEqual(js[[]string](k, `__t.seqs()`), []string{"#3"}) && !js[bool](k, `__t.shown('earlier')`) &&
			strings.HasPrefix(js[string](k, `(document.querySelector('#notes .stuck') || {}).textContent || ''`), "The messages before these are too large") &&
			js[bool](k, `Boolean(document.querySelector('#notes .stuck button'))`), js[string](k, `document.getElementById('notes').textContent`))
	k.c.eval(`document.querySelector('#notes .stuck button').click()`, nil)
	k.await(`!__t.anchor() && __t.seqs()[0] === '#1' && !document.getElementById('more-messages').disabled`)
	for i := 0; i < 5 && js[bool](k, `__t.shown('more-messages')`); i++ {
		js[int](k, `__t.press('more-messages')`)
		k.await(`!document.getElementById('more-messages').disabled`)
	}
	k.check("from the start, \"load more\" goes through all of it", !strings.Contains(js[string](k, `location.hash`), "&at=") &&
		reflect.DeepEqual(js[[]string](k, `__t.seqs()`), []string{"#1", "#2", "#3", "#4"}), js[[]string](k, `__t.seqs()`))

	fmt.Println("# calls under a message")
	k.c.eval(`__t.open(`+q(desk)+`, `+q(d.Code)+`)`, nil)
	k.await(`__t.seqs().length === 5`)
	type call struct {
		Tool   string   `json:"tool"`
		Failed bool     `json:"failed"`
		Files  []string `json:"files"`
		Open   bool     `json:"open"`
		Shown  int      `json:"shown"`
	}
	calls := func(i int) string {
		return fmt.Sprintf(`[...document.querySelectorAll('#messages > article')[%d].querySelectorAll('.calls > li')]`, i)
	}
	views := js[[]call](k, calls(1)+`.map(li => ({ tool: li.querySelector('summary .tool').textContent, failed: Boolean(li.querySelector('summary .bad')),
		files: [...li.querySelectorAll('summary .file')].map(f => f.textContent.replace(/\s+/g, ' ')), open: li.querySelector('details').open, shown: li.querySelectorAll('pre').length }))`)
	type given struct {
		Tool   string `json:"tool"`
		Error  bool   `json:"error"`
		Input  string `json:"input"`
		Output string `json:"output"`
	}
	sent := js[[]given](k, `fetch('/api/v1/sessions/messages?node=`+desk+`&key=`+url.QueryEscape(d.Code)+`').then(r => r.json()).then(r => r.messages[1].calls)`)
	var tools, wantTools []string
	var failed, wantFailed []bool
	for _, v := range views {
		tools, failed = append(tools, v.Tool), append(failed, v.Failed)
	}
	for _, s := range sent {
		wantTools, wantFailed = append(wantTools, s.Tool), append(wantFailed, s.Error)
	}
	k.check("the calls of a message are listed under it, each with its tool", len(tools) == 4 && reflect.DeepEqual(tools, wantTools), tools)
	k.check("a failed call says so, and only that one", reflect.DeepEqual(failed, wantFailed) && reflect.DeepEqual(failed, []bool{false, true, false, false}), failed)
	var files [][]string
	for _, v := range views {
		files = append(files, v.Files)
	}
	k.check("each file is named with what was done to it, from the project", reflect.DeepEqual(files, [][]string{
		{"read importer/rows.go"}, {}, {"changed importer/rows.go"}, {"deleted importer/testdata/rows_without_last.csv"}}), files)
	k.check("inputs and outputs are folded away at first", len(views) == 4 && views[1].Shown == 0 && !views[1].Open &&
		js[int](k, `document.querySelectorAll('#messages .calls pre').length`) == 0)
	k.c.eval(calls(1)+`[1].querySelector('summary').click()`, nil)
	k.await(calls(1) + `[1].querySelectorAll('pre').length === 2`)
	io := js[[]string](k, `[...`+calls(1)+`[1].querySelectorAll('pre')].map(p => p.textContent)`)
	k.check("a click on a call shows its input and its output", len(sent) == 4 && reflect.DeepEqual(io, []string{sent[1].Input, sent[1].Output}), io)
	k.c.eval(calls(1)+`[1].querySelector('summary').click()`, nil)
	k.check("a second click folds them away again", !js[bool](k, calls(1)+`[1].querySelector('details').open`))
	k.check("the thinking is folded away at first", js[bool](k, `(() => { const t = document.querySelectorAll('#messages > article')[1].querySelector('.thinking'); return Boolean(t) && !t.open && !t.querySelector('.text'); })()`))
	k.c.eval(`document.querySelectorAll('#messages > article')[1].querySelector('.thinking > summary').click()`, nil)
	k.await(`document.querySelectorAll('#messages > article')[1].querySelector('.thinking .text')`)
	k.check("a click shows it", js[string](k, `document.querySelectorAll('#messages > article')[1].querySelector('.thinking .text').textContent`) ==
		js[string](k, `fetch('/api/v1/sessions/messages?node=`+desk+`&key=`+url.QueryEscape(d.Code)+`').then(r => r.json()).then(r => r.messages[1].thinking)`))
	k.check("a call made later in the session is under its own message", reflect.DeepEqual(
		js[[]string](k, calls(4)+`.map(li => li.querySelector('.tool').textContent)`), []string{"shell"}) &&
		js[int](k, calls(0)+`.length`)+js[int](k, calls(2)+`.length`)+js[int](k, calls(3)+`.length`) == 0)

	fmt.Println("# a record of text only")
	// level is the structure the node says a reading of a session has.
	level := func(node, key string) string {
		return js[string](k, `fetch('/api/v1/sessions/messages?node=`+url.QueryEscape(node)+`&key=`+url.QueryEscape(key)+`').then(r => r.json()).then(r => String(r.structure_level))`)
	}
	k.c.eval(`__t.open(`+q(attic)+`, `+q(d.Kept)+`)`, nil)
	k.await(`__t.seqs().length === 2`)
	k.check("of attic's copy, which has its text only, the page says that it has no calls in it", level(attic, d.Kept) == "0" &&
		strings.Contains(js[string](k, `document.getElementById('notes').textContent`), "This record has the text only, without its tool calls."),
		level(attic, d.Kept), js[string](k, `document.getElementById('notes').textContent`))
	k.c.eval(`__t.open(`+q(desk)+`, `+q(d.Code)+`)`, nil)
	k.await(`__t.seqs().length === 5`)
	k.check("of a session with its calls it says nothing of the kind", level(desk, d.Code) == "2" &&
		!strings.Contains(js[string](k, `document.getElementById('notes').textContent`), "text only"), level(desk, d.Code))

	fmt.Println("# the search form, wide and narrow")
	open(home)
	js[int](k, `__t.press('mode-search')`)
	k.c.eval(`document.getElementById('s-more').open = true`, nil)
	// The name of the operation on one line, its choice as wide as its text,
	// its padding and its arrow, and failed attempts on the line below.
	k.check("the operation on a file is on one line, its choice shows all it says, and failed attempts come below", js[bool](k, `(() => {
		const label = document.getElementById('s-op-label'), s = document.getElementById('s-op'), cs = getComputedStyle(s);
		const text = document.createElement('canvas').getContext('2d');
		text.font = cs.font;
		const need = text.measureText(s.selectedOptions[0].text).width + parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight) + 16;
		return label.getClientRects().length === 1 && label.getBoundingClientRect().height < 1.6 * parseFloat(getComputedStyle(label).fontSize) &&
			s.clientWidth >= need && document.getElementById('s-failed').getBoundingClientRect().top >= s.getBoundingClientRect().bottom; })()`),
		js[string](k, `(() => { const l = document.getElementById('s-op-label').getBoundingClientRect(), s = document.getElementById('s-op'); return l.width + 'x' + l.height + ' select ' + s.clientWidth; })()`))
	if err := c.view(390, "en-US"); err != nil {
		k.check("a narrow window", false, err)
	}
	// A long session below makes the page scroll, and the scroll bar takes
	// its width from the window.
	open(home)
	k.c.eval(`__t.open(`+q(laptop)+`, `+q(d.Long)+`)`, nil)
	k.await(`__t.seqs().length === 50`)
	k.check("the page scrolls", js[bool](k, `document.documentElement.clientWidth < innerWidth`), js[int](k, `document.documentElement.clientWidth`))
	js[int](k, `__t.press('mode-search')`)
	k.c.eval(`(() => { const s = document.getElementById('s-when'); s.value = 'range'; s.dispatchEvent(new Event('change', { bubbles: true })); })()`, nil)
	// What is wider than its line may still be inside the window, in the
	// margin, until a scroll bar takes that: the dates have to fit their
	// line. The window here is what is left of it beside a scroll bar.
	k.check("in a narrow window the dates to give fit their line, and nothing of the form is wider than the window", js[bool](k, `(() => {
		const range = document.getElementById('s-range');
		return __t.shown('s-to') && range.scrollWidth <= range.clientWidth && [...range.children].every(l => l.scrollWidth <= l.clientWidth); })()`) &&
		js[int](k, `[...document.querySelectorAll('#search *')].filter(e => e.getBoundingClientRect().right > document.documentElement.clientWidth + 0.5).length`) == 0 &&
		js[bool](k, `document.documentElement.scrollWidth <= document.documentElement.clientWidth`),
		js[string](k, `(() => { const r = document.getElementById('s-range'); return r.scrollWidth + '/' + r.clientWidth + ' ' + [...r.children].map(l => l.scrollWidth + '/' + l.clientWidth).join(' '); })()`),
		js[[]string](k, `[...document.querySelectorAll('#search *')].filter(e => e.getBoundingClientRect().right > document.documentElement.clientWidth + 0.5).map(e => (e.id || e.tagName) + ' ' + Math.round(e.getBoundingClientRect().right))`),
		js[int](k, `document.documentElement.clientWidth`))

	fmt.Println("# a narrow window, in Chinese")
	// unreadable opens, from the list of attic, the session of it that was
	// never read, with a reader's click on its row.
	unreadable := func() {
		k.c.eval(`__t.hold = false; __t.held = []; __t.filter(`+q(attic)+`)`, nil)
		k.await(`__t.rows().length === ` + fmt.Sprint(n.Data.Sessions[2]) + ` && !document.getElementById('more').disabled`)
		k.c.eval(`__t.click(`+q(attic)+`, `+q(n.Data.Unread)+`)`, nil)
		k.await(`document.querySelector('#notes p')`)
	}
	if err := c.view(390, "zh-CN"); err != nil {
		k.check("a narrow window", false, err)
	}
	open(home)
	k.check("the page speaks Chinese", js[string](k, `document.documentElement.lang`) == "zh-CN")
	k.c.eval(`__t.filter(`+q(attic)+`)`, nil)
	k.await(`__t.rows().length === ` + fmt.Sprint(n.Data.Sessions[2]))
	k.check("in a row the machine and the time are whole", js[int](k, `[...document.querySelectorAll('#list .where, #list .when, #list .count')].filter(__t.clipped).length`) == 0)
	k.check("in a row only a long project name is cut short", reflect.DeepEqual(
		js[[]string](k, `[...document.querySelectorAll('#list > li')].filter(li => __t.clipped(li.querySelector('.grow'))).map(li => li.dataset.key)`), []string{n.Data.Unread}),
		js[[]string](k, `[...document.querySelectorAll('#list .grow')].filter(__t.clipped).map(e => e.textContent)`))
	k.check("the rows are of one height", js[int](k, `new Set([...document.querySelectorAll('#list > li')].map(li => li.offsetHeight)).size`) == 1)
	listed := js[string](k, `[...document.querySelectorAll('#list > li')].find(li => li.dataset.key === `+q(n.Data.Unread)+`).querySelector('.t').textContent`)
	k.check("the reader is out of sight before the click", !js[bool](k, `__t.inView(document.getElementById('title'))`))
	unreadable()
	why := js[string](k, `document.querySelector('#notes p').textContent`)
	k.check("the reason why it cannot be read is in the window", js[bool](k, `__t.inView(document.querySelector('#notes p'))`),
		js[string](k, `JSON.stringify(document.querySelector('#notes p').getBoundingClientRect())`))
	k.check("its title is the one of the list, in the window too", listed != "" && listed != n.Data.Unread &&
		js[string](k, `document.getElementById('title').textContent`) == listed && js[bool](k, `__t.inView(document.getElementById('title'))`),
		js[string](k, `document.getElementById('title').textContent`))
	k.check("what the list said of it is under the title", strings.Contains(js[string](k, `document.getElementById('meta').textContent`), "opencode"))
	k.check("the reason is in Chinese and names the machine", strings.Contains(why, n.Data.Names[2]+" 现在离线") && !strings.Contains(why, "offline"), why)
	k.c.eval(`__t.open(`+q(attic)+`, `+q(n.Data.Kept)+`)`, nil)
	k.await(`__t.seqs().length === 2`)
	k.check("a record of text only is said to be one, in Chinese", strings.Contains(js[string](k, `document.getElementById('notes').textContent`), "这份记录只有正文，没有工具调用信息。"),
		js[string](k, `document.getElementById('notes').textContent`))
	type build struct{ Version, Shown, Whole string }
	builds := js[[]build](k, `__t.sessions('').then(() => fetch('/api/v1/sessions?limit=1')).then(r => r.json()).then(page =>
		page.nodes.map((n, i) => { const s = document.querySelectorAll('#machines li')[i].querySelectorAll('.facts span'); const b = s[s.length - 2];
			return { Version: n.version || '', Shown: b.textContent, Whole: b.title }; }))`)
	for _, b := range builds {
		short := buildinfo.Short(b.Version)
		k.check("a machine's build is named the short way, the whole on hover", len(builds) == 3 && b.Version != "" && b.Shown == short &&
			(b.Whole == b.Version || short == b.Version && b.Whole == ""), fmt.Sprintf("%+v", b))
	}
	k.check("no fact about a machine breaks in the middle", js[int](k,
		`[...document.querySelectorAll('#machines .facts span')].filter(s => new Set([...s.getClientRects()].map(r => Math.round(r.top))).size !== 1).length`) == 0)

	fmt.Println("# a search in a narrow window, in Chinese")
	open(home)
	js[int](k, `__t.press('mode-search')`)
	js[int](k, `__t.type('s-q', `+q(d.Term)+`)`)
	k.check("the search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.check("the page says in Chinese how each machine was searched", strings.Contains(row(desk), "实时搜索") &&
		strings.Contains(row(attic), "只搜了副本") && strings.Contains(row(attic), "以前读过") && !strings.Contains(row(attic), "结果不完整"), row(desk), row(attic))
	k.check("no place found is wider than the window", js[int](k, `[...document.querySelectorAll('#hits > li')].filter(li => li.getBoundingClientRect().right > innerWidth).length`) == 0)
	// Here laptop has the long name that tries to be markup, which is cut
	// short as it should be; the names of the others fit.
	whole := `[...document.querySelectorAll('#hits > li:not([data-node="` + laptop + `"])')].flatMap(li => [...li.querySelectorAll('.where, .when')]).filter(__t.clipped)`
	k.check("in a place found the machine and the time are whole", js[int](k, whole+`.length`) == 0 &&
		js[int](k, `document.querySelectorAll('#hits > li:not([data-node="`+laptop+`"])').length`) > 0,
		js[[]string](k, whole+`.map(e => e.textContent + ' ' + e.scrollWidth + '/' + e.clientWidth)`))
	first := js[string](k, `__t.places().find(p => p.includes('.'))`)
	k.check("the reader is out of sight before the click", !js[bool](k, `__t.inView(document.getElementById('title'))`))
	k.c.eval(`__t.choose(`+q(first)+`)`, nil)
	k.await(`__t.anchor()`)
	a := js[anchor](k, `__t.anchor()`)
	k.check("in a narrow window the place found is brought into view", a.InView && a.Badge == "命中", fmt.Sprintf("%+v", a))
	k.check("the call found in it is opened, its files named in Chinese", js[bool](k, `(() => {
		const li = document.querySelector('#messages .calls > li.anchor');
		return Boolean(li) && li.querySelector('details').open && [...li.querySelectorAll('.file .op')].every(e => ['修改', '删除', '读取'].includes(e.textContent));
	})()`))
	k.check("nothing in the reader is wider than the window", js[int](k, `[...document.querySelectorAll('#messages *')].filter(e => e.getBoundingClientRect().right > innerWidth + 1).length`) == 0)
	js[int](k, `__t.type('s-q', '')`)
	js[int](k, `__t.type('s-file', 'importer/missing.go')`)
	k.check("a file found nowhere is asked for", js[int](k, `__t.submit()`) == 1)
	searched()
	k.check("the page says in Chinese that finding nothing in part is no proof", js[string](k, `__t.note()`) == "没有找到。但这次搜索不完整，不能说明没有。" &&
		strings.Contains(row(attic), "结果不完整"), js[string](k, `__t.note()`), row(attic))
	js[int](k, `__t.type('s-file', '')`)
	js[int](k, `__t.type('s-q', `+q(d.Term)+`)`)
	k.check("dates the wrong way round are refused in Chinese", js[int](k, reversed)+js[int](k, `__t.submit()`) == 0 &&
		js[string](k, `__t.note()`) == "开始日期不能晚于结束日期。", js[string](k, `__t.note()`))
	k.c.eval(`(() => { const s = document.getElementById('s-when'); s.value = ''; s.dispatchEvent(new Event('change', { bubbles: true })); })()`, nil)
	searched()
	side = js[string](k, covered(laptop))
	k.check("a search is asked", js[int](k, `__t.submit()`) == 1)
	searched()
	k.c.eval(`__t.edit = null`, nil)
	sides = js[[]string](k, `[...document.querySelectorAll('#search-nodes li.live .side')].map(e => e.textContent)`)
	k.check("how far back thinking and full tool inputs were searched is said in Chinese", side != "" &&
		reflect.DeepEqual(sides, []string{d.Names[1] + "：正文已全部搜索；思考过程和完整工具输入只搜到 " + side + "。"}), sides)
	k.c.eval(`__t.fail = true`, nil)
	k.check("a search is tried", js[int](k, `__t.submit()`) == 1)
	searched()
	k.c.eval(`__t.fail = false`, nil)
	k.check("a node that cannot be reached is said so in Chinese", js[string](k, `__t.note()`) == "搜索失败：连不上本机的 h8c 节点。", js[string](k, `__t.note()`))

	if *shots != "" {
		fmt.Println("# pictures")
		if err := pictures(ctx, k, *shots); err != nil {
			k.check("the pictures", false, err)
		}
	}

	fmt.Println("# all along")
	k.check("no dialog opened and no exception went uncaught", len(c.troubles()) == 0, c.troubles())

	if k.failed > 0 {
		fmt.Printf("\n%d checks failed\n", k.failed)
		return 1
	}
	fmt.Println("\nall checks passed")
	return 0
}

// pictures writes pictures of the page to dir: of a demo of its own, which
// has nothing in it that tries to be markup, in a narrow and in a wide
// window, in English and in Chinese. After the list and a session that
// cannot be read come the search, grouped and not, a place found opened in
// its session, calls opened under a message, a machine that was not
// searched, the note that more was found than is listed, nothing found in
// full and in part, a node that cannot be reached, machines that each ran
// out of a budget, a place near the start of its session, as found and
// after its session changed, dates the wrong way round, a machine that
// searched its thinking only so far back, and a copy of text only.
func pictures(ctx context.Context, k *checker, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	n, err := demo.Start(ctx, demo.Options{})
	if err != nil {
		return err
	}
	defer n.Close()
	c := k.c
	desk, laptop, attic := n.Data.IDs[0], n.Data.IDs[1], n.Data.IDs[2]
	home := n.URL[:strings.Index(n.URL, "?")]
	shot := func(name string, whole bool) {
		if err := c.shot(filepath.Join(dir, name+".png"), whole); err != nil {
			k.check("a picture", false, err)
		}
	}
	for _, width := range []int{390, 1440} {
		for _, language := range []string{"en-US", "zh-CN"} {
			name := fmt.Sprintf("%d-%s-", width, language[:2])
			if err := c.view(width, language); err != nil {
				return err
			}
			// Under another name of this machine the browser has no cookie
			// for the page: that is the notice.
			c.call("Page.navigate", map[string]any{"url": strings.Replace(home, "127.0.0.1", "localhost", 1)}, nil)
			k.await(`document.body.className === 'notice'`)
			shot(name+"1-notice", false)

			c.call("Page.navigate", map[string]any{"url": n.URL}, nil)
			k.await(`window.__t && __t.rows().length > 0 && !document.getElementById('more').disabled`)
			c.eval(`__t.hold = true; __t.press('refresh')`, nil)
			shot(name+"2-refresh-pending", false)
			c.eval(`document.getElementById('more').scrollIntoView({block: 'center'})`, nil)
			shot(name+"3-more-disabled", false)
			c.eval(`__t.release(0).then(() => { scrollTo(0, 0); document.getElementById('browse').scrollTop = 0; })`, nil)

			c.eval(`__t.hold = false; __t.held = []; __t.filter(`+q(attic)+`)`, nil)
			k.await(`__t.rows().length === ` + fmt.Sprint(n.Data.Sessions[2]))
			shot(name+"4-machines-and-rows", true)
			c.eval(`__t.click(`+q(attic)+`, `+q(n.Data.Unread)+`)`, nil)
			k.await(`document.querySelector('#notes p')`)
			shot(name+"5-cannot-be-read", false)

			// The search: how each machine was searched and the places
			// found, then grouped by session.
			c.call("Page.navigate", map[string]any{"url": home}, nil)
			k.await(`window.__t && __t.rows().length > 0 && !document.getElementById('more').disabled`)
			c.eval(`__t.press('mode-search'); __t.type('s-q', `+q(n.Data.Term)+`); __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().length > 0`)
			c.eval(`document.activeElement.blur(); document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"6-search", false)
			c.eval(`__t.press('s-group')`, nil)
			k.await(`__t.searched() && document.querySelector('#hits .count')`)
			c.eval(`document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"7-grouped", false)
			// A place found in a call, deep in a long session.
			c.eval(`__t.press('s-group'); __t.type('s-q', `+q(`"`+n.Data.Anchor+`"`)+`); __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().length === 1`)
			c.eval(`document.getElementById('s-more').open = true; document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"8-one-place-found", false)
			c.eval(`__t.choose(__t.places()[0])`, nil)
			k.await(`__t.anchor() && !document.getElementById('earlier').disabled`)
			shot(name+"9-anchor", false)
			// Above it, "earlier messages": the node says there are some.
			c.eval(`document.getElementById('reader-body').scrollIntoView()`, nil)
			shot(name+"9-anchor-top", false)
			// Calls under a message: the failed one and a change opened,
			// with the thinking.
			c.eval(`__t.open(`+q(n.Data.IDs[0])+`, `+q(n.Data.Code)+`)`, nil)
			k.await(`__t.seqs().length === 5`)
			c.eval(`(() => {
				const m = document.querySelectorAll('#messages > article')[1];
				m.querySelector('.thinking').open = true;
				const calls = m.querySelectorAll('.calls > li > details');
				calls[1].open = calls[2].open = true;
				m.scrollIntoView();
			})()`, nil)
			k.await(`document.querySelectorAll('#messages .calls pre').length === 4`)
			shot(name+"10-calls", false)
			// A machine that was not searched, and more found than listed.
			c.eval(none(attic)+`; document.getElementById('s-more').open = false; __t.type('s-q', `+q(n.Data.Broad)+`); __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().length === 50`)
			c.eval(`__t.edit = null; document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"11-not-searched", false)
			c.eval(`document.getElementById('search-more').scrollIntoView({block: 'end'})`, nil)
			shot(name+"12-more", false)
			// Nothing found: searched in full, and in part.
			c.eval(`__t.type('s-q', 'zyzzyva'); __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().length === 0`)
			c.eval(`document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"13-nothing-found", false)
			c.eval(`__t.type('s-q', ''); __t.type('s-file', 'importer/missing.go'); __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().length === 0 && document.querySelector('#search-nodes [data-partial]')`)
			c.eval(`document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"14-nothing-found-in-part", false)
			// A search that fails: the node cannot be reached.
			c.eval(`__t.fail = true; __t.type('s-file', ''); __t.type('s-q', `+q(n.Data.Term)+`); __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().length === 0`)
			c.eval(`__t.fail = false; document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"15-failed", false)
			// Desk and laptop each ran out of a budget, each with its note;
			// laptop's thinking was searched only so far back.
			c.eval(spent(desk, laptop, attic)+`; __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().length > 0`)
			c.eval(`__t.edit = null; document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"16-incomplete", false)
			// A place near the start of its session, with nothing before
			// it; then the same place after its session changed.
			code := q(desk + "/" + n.Data.Code + "#")
			c.eval(`__t.type('s-q', ''); __t.type('s-file', `+q(n.Data.File)+`); __t.submit()`, nil)
			k.await(`__t.searched() && __t.places().some(p => p.startsWith(` + code + `))`)
			c.eval(`__t.choose(__t.places().find(p => p.startsWith(`+code+`)))`, nil)
			k.await(`__t.anchor() && !document.getElementById('earlier').disabled`)
			shot(name+"17-nothing-earlier", false)
			c.eval(`(() => { const d = document.querySelector('#hits > li.selected').dataset;
				__t.at(d.node, d.key, d.at, Number(d.rev) + 1000, d.call); })()`, nil)
			k.await(`__t.anchor() && document.querySelector('#notes p') && !document.getElementById('earlier').disabled`)
			c.eval(`document.getElementById('reader-body').scrollIntoView()`, nil)
			shot(name+"18-moved", false)
			// Dates the wrong way round: the page says so and asks nothing.
			c.eval(`__t.type('s-file', ''); __t.type('s-q', `+q(n.Data.Term)+`)`, nil)
			c.eval(reversed, nil)
			k.await(`__t.searched() && __t.places().length === 0`)
			c.eval(`document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"19-dates-reversed", false)
			// Laptop searched its thinking only so far back, all else in full.
			c.eval(`(() => { const s = document.getElementById('s-when'); s.value = ''; s.dispatchEvent(new Event('change', { bubbles: true })); })()`, nil)
			k.await(`__t.searched() && __t.places().length > 0`)
			c.eval(covered(laptop)+`; __t.submit()`, nil)
			k.await(`__t.searched() && document.querySelector('#search-nodes .side')`)
			c.eval(`__t.edit = null; document.getElementById('modes').scrollIntoView()`, nil)
			shot(name+"20-side-coverage", false)
			// Attic's kept copy, which has its text only.
			c.eval(`__t.open(`+q(attic)+`, `+q(n.Data.Kept)+`)`, nil)
			k.await(`__t.seqs().length === 2 && document.querySelector('#notes p')`)
			c.eval(`document.getElementById('reader-body').scrollIntoView()`, nil)
			shot(name+"21-text-only", false)
		}
	}
	fmt.Println("written to " + dir)
	return nil
}

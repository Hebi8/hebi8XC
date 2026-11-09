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
// what is missing, that it says when there is more, and that a place found
// opens its session there, marked and in view, with the messages before
// and after it each shown once. Under a message it checks the calls: their
// tools, failures and files shown, input and output folded until a click.
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
}

// anchor is the message the page marks as a place found.
type anchor struct {
	Seq    string `json:"seq"`
	Badge  string `json:"badge"`
	Calls  []int  `json:"calls"`
	Open   bool   `json:"open"`
	InView bool   `json:"inView"`
}

// none is an answer to a search in which attic was not searched at all and
// says why: the demo, where attic has a copy, does not come to that.
func none(attic string) string {
	return `__t.edit = (url, v) => url.includes('/sessions/search') ? { ...v, nodes: v.nodes.map(n => n.id === ` + q(attic) +
		` ? { id: n.id, name: n.name, searched: 'none', note: 'peer search unavailable; searched held copies' } : n) } : v`
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
	k.check("attic was searched in the copies kept of it, and that is said to be incomplete", states[attic] == "copies+partial", states)
	k.check("the page says what is missing from attic", strings.Contains(row(attic), "only the sessions read on this machine before") &&
		strings.Contains(row(attic), "matches may be missing"), row(attic))
	k.check("a call found names its tool and its files", js[bool](k, `__t.found('q=`+d.Term+`').then(f => {
		const calls = f.hits.filter(h => h.call !== undefined);
		return calls.length > 0 && calls.every(h => {
			const li = [...document.querySelectorAll('#hits > li')].find(li => li.dataset.key === h.key && li.dataset.at === String(h.id) && li.dataset.call === String(h.call));
			return li.querySelector('.call .tool').textContent === h.tool &&
				[...li.querySelectorAll('.call .file')].map(e => e.title).join('\n') === (h.files || []).map(f => f.path).join('\n');
		});
	})`))
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
	k.check("each row says how many matches it holds", js[bool](k, `[...document.querySelectorAll('#hits > li')].every(li => /^\d+ match(es)?$/.test(li.querySelector('.count').textContent))`))
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
		strings.Contains(row(attic), "It says: peer search unavailable; searched held copies"), row(attic))

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
		strings.Contains(row(attic), "只搜了副本") && strings.Contains(row(attic), "结果不完整") && strings.Contains(row(attic), "可能漏掉了结果"), row(desk), row(attic))
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
// searched, and the note that more was found than is listed.
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
	attic := n.Data.IDs[2]
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
		}
	}
	fmt.Println("written to " + dir)
	return nil
}

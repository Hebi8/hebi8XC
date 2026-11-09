// What the check puts into the page before the page's own script runs. It
// stands between the page and fetch: it notes every request the page makes,
// and can hold the answers back to hand them over in a chosen order. The
// requests themselves leave at once and reach the real node. With edit set
// it changes an answer before the page reads it, for a state of a machine
// that the demo does not reach.
(() => {
  const real = window.fetch.bind(window);
  const t = window.__t = { log: [], held: [], hold: false, edit: null };

  window.fetch = (input, init) => {
    const entry = { url: String(input) };
    t.log.push(entry.url);
    let read;
    entry.read = new Promise(done => { read = done; });
    const answer = real(input, init).then(res => {
      const json = res.json.bind(res);
      res.json = () => json().then(v => t.edit ? t.edit(entry.url, v) : v).finally(read);
      return res;
    });
    if (!t.hold) return answer;
    return new Promise(resolve => {
      entry.release = () => resolve(answer);
      t.held.push(entry);
    });
  };

  // release hands the page a held answer, and returns when the page has
  // done with it all it is going to do.
  t.release = async i => {
    t.held[i].release();
    await t.held[i].read;
    await new Promise(done => setTimeout(done));
  };

  const get = path => real(path, { cache: 'no-store' }).then(res => res.json());
  // sessions asks the node itself, not the page, for a whole list.
  t.sessions = async query => {
    const ids = [];
    let pages = 0, first = 0, after = '';
    do {
      const page = await get(`/api/v1/sessions?limit=50&${query}&after=${encodeURIComponent(after)}`);
      ids.push(...page.sessions.map(s => `${s.node_id}/${s.key}`));
      if (!pages++) first = page.sessions.length;
      after = page.next || '';
    } while (after);
    return { ids, pages, first };
  };
  t.messages = async (node, key) => {
    const seqs = [];
    let after = 0;
    do {
      const r = await get(`/api/v1/sessions/messages?limit=50&node=${encodeURIComponent(node)}&key=${encodeURIComponent(key)}&after=${after}`);
      seqs.push(...r.messages.map(m => `#${m.seq}`));
      after = r.next || 0;
    } while (after);
    return seqs;
  };
  // message is a message of a session as the node lists it: its id, and
  // the revision of the session it was read at, which a place found has.
  t.message = async (node, key, seq) => {
    let after = 0;
    do {
      const r = await get(`/api/v1/sessions/messages?limit=50&node=${encodeURIComponent(node)}&key=${encodeURIComponent(key)}&after=${after}`);
      const m = r.messages.find(m => m.seq === seq);
      if (m) return { id: m.id, rev: r.rev };
      after = r.next || 0;
    } while (after);
    return null;
  };
  // around asks the node itself for the messages around one, as the page
  // does for a place found.
  t.around = async (node, key, id, before, after) => {
    const r = await get(`/api/v1/sessions/messages?node=${encodeURIComponent(node)}&key=${encodeURIComponent(key)}&around=${id}&before=${before}&after=${after}`);
    return { seqs: r.messages.map(m => `#${m.seq}`), has_before: r.has_before };
  };

  // What is on the page.
  t.rows = () => [...document.querySelectorAll('#list > li')].map(li => `${li.dataset.node}/${li.dataset.key}`);
  t.seqs = () => [...document.querySelectorAll('#messages > article > .h > span:nth-child(2)')].map(s => s.textContent);
  // shown: the button is there for a reader to see and to click. That is a
  // matter of what is drawn, not of the hidden attribute: a style sheet can
  // show an element that has it.
  t.shown = id => {
    const e = document.getElementById(id);
    return getComputedStyle(e).display !== 'none' && e.getClientRects().length > 0;
  };
  // press clicks a button from script, which reaches a button that is not
  // shown too, and returns how many requests the page made of it.
  t.press = id => {
    const before = t.log.length;
    document.getElementById(id).click();
    return t.log.length - before;
  };
  // click is a reader's click on the row of a session in the list.
  t.click = (node, key) => {
    [...document.querySelectorAll('#list > li')].find(li => li.dataset.node === node && li.dataset.key === key).click();
  };
  // filter chooses a machine in the filters, as a reader does.
  t.filter = node => {
    const select = document.getElementById('node');
    select.value = node;
    select.dispatchEvent(new Event('input', { bubbles: true }));
  };
  // inView: all of the element is in the window, nothing of it scrolled away.
  t.inView = e => {
    const r = e.getBoundingClientRect();
    return r.height > 0 && r.top >= 0 && r.bottom <= innerHeight && r.left >= 0 && r.right <= innerWidth;
  };
  // clipped: the element shows less than its text.
  t.clipped = e => e.scrollWidth > e.clientWidth;
  t.open = (node, key) => { location.hash = `s=${encodeURIComponent(node)}/${encodeURIComponent(key)}`; };
  // at opens a session at a place in it, as an address the page made does.
  t.at = (node, key, id, rev, call) => {
    location.hash = `s=${encodeURIComponent(node)}/${encodeURIComponent(key)}&at=${id}&rev=${rev}` +
      (call === undefined ? '' : `&call=${call}`);
  };
  t.withCursor = from => t.log.slice(from).filter(url => new URL(url).searchParams.get('after') > '0');

  // The search. found asks the node itself for one, and names what it
  // found the way places shows those on the page: node/key#id, and .call
  // for a call.
  const name = (node, key, id, call) => `${node}/${key}#${id}` + (call === undefined ? '' : `.${call}`);
  t.found = async query => {
    const r = await get(`/api/v1/sessions/search?${query}`);
    const places = r.sessions ? r.sessions.map(g => name(g.node_id, g.key, g.first))
      : r.hits.map(h => name(h.node_id, h.key, h.id, h.call));
    return { places, more: r.more, hits: r.hits, groups: r.sessions || [], nodes: r.nodes };
  };
  t.places = () => [...document.querySelectorAll('#hits > li')].map(li =>
    name(li.dataset.node, li.dataset.key, li.dataset.at, li.dataset.call));
  // states is what the page says of each machine: how it was searched, and
  // "+partial" when it says that what came from there is incomplete.
  t.states = () => Object.fromEntries([...document.querySelectorAll('#search-nodes [data-searched]')].map(e =>
    [e.dataset.node, e.dataset.searched + (e.dataset.partial ? '+partial' : '')]));
  // type puts text into a field, as typing does; submit sends the search
  // form, as Enter does. Both return how many requests the page made.
  t.type = (id, text) => {
    const before = t.log.length;
    const e = document.getElementById(id);
    e.value = text;
    e.dispatchEvent(new Event('input', { bubbles: true }));
    return t.log.length - before;
  };
  t.submit = () => {
    const before = t.log.length;
    document.getElementById('search').requestSubmit();
    return t.log.length - before;
  };
  // choose is a reader's click on a place found.
  t.choose = place => [...document.querySelectorAll('#hits > li')].find(li =>
    name(li.dataset.node, li.dataset.key, li.dataset.at, li.dataset.call) === place).click();
  t.searched = () => !document.getElementById('s-go').disabled && document.getElementById('search-note').textContent !== '';
  // note is what the page says above the places found.
  t.note = () => document.getElementById('search-note').textContent;
  // anchor is the message marked as a place found: its number, the calls
  // marked in it and whether they are open.
  t.anchor = () => {
    const a = document.querySelector('#messages > article.anchor');
    if (!a) return null;
    return {
      seq: a.querySelector('.h > span:nth-child(2)').textContent,
      badge: (a.querySelector('.h .match') || {}).textContent || '',
      calls: [...a.querySelectorAll('.calls > li.anchor')].map(li => Number(li.dataset.call)),
      open: [...a.querySelectorAll('.calls > li.anchor > details')].every(d => d.open),
      inView: t.inView(a.querySelector('.h')),
    };
  };
})();

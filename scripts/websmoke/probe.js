// What the check puts into the page before the page's own script runs. It
// stands between the page and fetch: it notes every request the page makes,
// and can hold the answers back to hand them over in a chosen order. The
// requests themselves leave at once and reach the real node.
(() => {
  const real = window.fetch.bind(window);
  const t = window.__t = { log: [], held: [], hold: false };

  window.fetch = (input, init) => {
    const entry = { url: String(input) };
    t.log.push(entry.url);
    let read;
    entry.read = new Promise(done => { read = done; });
    const answer = real(input, init).then(res => {
      const json = res.json.bind(res);
      res.json = () => json().finally(read);
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
  t.open = (node, key) => { location.hash = `s=${encodeURIComponent(node)}/${encodeURIComponent(key)}`; };
  t.withCursor = from => t.log.slice(from).filter(url => new URL(url).searchParams.get('after') > '0');
})();

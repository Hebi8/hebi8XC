'use strict';

// The page of the h8c web interface. Everything it shows, titles, project
// names, node names and message text, comes from session files and from
// other machines. None of it is ever parsed as markup: text goes into the
// document through textContent only. Do not add innerHTML here.

(() => {
  const zh = (navigator.language || '').toLowerCase().startsWith('zh');
  const lang = zh ? 'zh-CN' : 'en';
  const T = zh ? {
    refresh: '刷新',
    machines: n => `机器（${n}）`,
    thisMachine: '本机',
    online: '在线',
    offline: '离线',
    incompatible: (theirs, ours) => `协议不兼容（对方是 ${theirs}，本机是 ${ours}）`,
    sessions: n => `${n} 个会话`,
    synced: t => `同步于 ${t}`,
    neverSynced: '尚未同步',
    protocol: n => `协议 ${n}`,
    unknownBuild: '版本未知',
    notShared: note => `本机的会话没有共享：${note}`,
    noPeers: '还没有别的机器。在另一台机器上运行 h8c 并登录同一个账号后，它会出现在这里。',
    search: '搜索标题和项目名',
    allMachines: '所有机器',
    agent: 'agent',
    project: '项目',
    anyTime: '全部时间',
    last: { '24h': '最近 24 小时', '7d': '最近 7 天', '30d': '最近 30 天' },
    noSessions: '还没有任何会话。',
    noMatch: '没有符合条件的会话。',
    more: '加载更多',
    loading: '加载中…',
    pick: '选择一个会话来阅读。',
    messages: n => `${n} 条消息`,
    count: n => `${n} 条`,
    updated: t => `更新于 ${t}`,
    fromCopy: t => `这是 ${t} 保存在本机的副本。`,
    stale: '这个会话在副本保存之后有更新。',
    incomplete: '副本没有到会话末尾。',
    cut: '（这条消息更长，这里只有开头）',
    sideThread: id => `支线 ${id}`,
    showAll: n => `显示全部（${n} 个字符）`,
    failed: msg => `读取失败：${msg}`,
    cannot: {
      'not-found': () => '这个会话已经不存在了。',
      'offline-not-read': name => `${name} 现在离线，本机也没有读过这个会话，所以这里没有它的副本。`,
      'incompatible-not-read': name => `${name} 的协议和本机不兼容，本机也没有读过这个会话，所以这里没有它的副本。`,
      'feed-restarted': name => `${name} 刚刚重建了它的会话列表，请稍后再试。`,
    },
  } : {
    refresh: 'Refresh',
    machines: n => `Machines (${n})`,
    thisMachine: 'this machine',
    online: 'online',
    offline: 'offline',
    incompatible: (theirs, ours) => `protocol incompatible (it speaks ${theirs}, this machine ${ours})`,
    sessions: n => n === 1 ? '1 session' : `${n} sessions`,
    synced: t => `read ${t}`,
    neverSynced: 'not read yet',
    protocol: n => `protocol ${n}`,
    unknownBuild: 'unknown build',
    notShared: note => `This machine's sessions are not shared: ${note}`,
    noPeers: 'No other machines yet. Run h8c on another machine, logged in to the same account, and it shows up here.',
    search: 'Search titles and project names',
    allMachines: 'All machines',
    agent: 'agent',
    project: 'project',
    anyTime: 'Any time',
    last: { '24h': 'Last 24 hours', '7d': 'Last 7 days', '30d': 'Last 30 days' },
    noSessions: 'No sessions yet.',
    noMatch: 'No sessions match.',
    more: 'Load more',
    loading: 'Loading…',
    pick: 'Select a session to read.',
    messages: n => n === 1 ? '1 message' : `${n} messages`,
    count: n => n === 1 ? '1 msg' : `${n} msgs`,
    updated: t => `updated ${t}`,
    fromCopy: t => `This is the copy kept on this machine, made ${t}.`,
    stale: 'The session has changed since this copy was made.',
    incomplete: 'The copy does not reach the end of the session.',
    cut: '(this message is longer; this is its beginning)',
    sideThread: id => `side thread ${id}`,
    showAll: n => `Show all (${n} characters)`,
    failed: msg => `Could not read: ${msg}`,
    cannot: {
      'not-found': () => 'This session no longer exists.',
      'offline-not-read': name => `${name} is offline and this session has not been read on this machine before, so there is no copy of it here.`,
      'incompatible-not-read': name => `${name} speaks another protocol than this machine and this session has not been read here before, so there is no copy of it here.`,
      'feed-restarted': name => `${name} has just restarted its list of sessions. Try again in a moment.`,
    },
  };

  const $ = id => document.getElementById(id);
  // el makes an element with a class and text. Text is always set as text.
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  // when writes a time the short way: the year only if it is not this one.
  const when = iso => {
    const d = new Date(iso);
    const form = { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' };
    if (d.getFullYear() !== new Date().getFullYear()) form.year = 'numeric';
    return d.toLocaleString(lang, form);
  };
  // short names a build in a few characters, as buildinfo.Short does: a
  // build from a checkout by seven characters of its commit.
  const short = build => {
    const m = /^v\d+\.\d+\.\d+-(?:.*\.)?\d{14}-([0-9a-f]{12})(?:\+.*)?$/.exec(build);
    if (m) return m[1].slice(0, 7);
    return build.startsWith('devel-') && build.length > 13 ? build.slice(6, 13) : build;
  };
  const PAGE = 50;
  const SPAN = { '24h': 864e5, '7d': 7 * 864e5, '30d': 30 * 864e5 };
  const FOLD = 4000; // characters of a message shown before "show all"

  let nodes = [];
  let next = '';       // continues the list
  let pages = 0;       // pages of the list on screen
  let listSeq = 0;     // the newest list request; older answers are dropped
  let open = null;     // {node, key, next} of the session being read
  let readSeq = 0;
  const listed = new Map(); // the sessions of the list on screen, by node and key

  async function api(path, params) {
    const url = new URL('/api/v1/' + path, location.origin);
    for (const [name, value] of Object.entries(params)) {
      for (const v of [].concat(value)) {
        if (v !== '' && v !== undefined && v !== null) url.searchParams.append(name, v);
      }
    }
    const res = await fetch(url, { credentials: 'same-origin', cache: 'no-store' });
    if (res.status === 401) {
      location.reload(); // the cookie is gone; the server says how to get a new one
      throw new Error('unauthorized');
    }
    if (!res.ok) {
      const err = new Error((await res.text()).trim() || res.statusText);
      err.code = res.headers.get('H8c-Error') || ''; // the reason as a word, if it has one
      throw err;
    }
    return res.json();
  }

  function filters() {
    const span = SPAN[$('since').value];
    return {
      node: $('node').value,
      agent: $('agent').value.trim(),
      project: $('project').value.trim(),
      since: span ? Date.now() - span : '',
      text: $('q').value.trim().split(/\s+/).filter(Boolean),
    };
  }
  const filtered = f => Boolean(f.node || f.agent || f.project || f.since || f.text.length);

  function showMachines(list) {
    nodes = list;
    const self = list.find(n => n.self) || {};
    $('self').textContent = self.name || '';
    $('machines-title').textContent = T.machines(list.length);

    const note = $('machines-note');
    const notes = [];
    if (self.note) notes.push(T.notShared(self.note));
    if (list.length < 2) notes.push(T.noPeers);
    note.hidden = notes.length === 0;
    note.textContent = notes.join(' ');

    const rows = list.map(n => {
      const li = el('li');
      const head = el('div');
      head.append(el('span', 'name', n.name || n.id));
      if (n.self) head.append(el('span', 'dim', T.thisMachine));
      else if (!n.online) head.append(el('span', 'off', T.offline));
      else if (n.incompatible) head.append(el('span', 'bad', T.incompatible(n.protocol, self.protocol)));
      else head.append(el('span', '', T.online));
      // Each fact is one piece that does not break in the middle. Of the
      // build there is the short name; the whole one is there on hover.
      const facts = el('div', 'dim facts');
      const fact = (text, whole) => {
        if (!text) return;
        const span = el('span', '', text);
        if (whole && whole !== text) span.title = whole;
        facts.append(span, ' ');
      };
      fact(T.sessions(n.sessions));
      fact(n.self ? '' : n.synced_at ? T.synced(when(n.synced_at)) : T.neverSynced);
      fact(n.version ? short(n.version) : n.protocol ? T.unknownBuild : '', n.version);
      fact(n.protocol ? T.protocol(n.protocol) : '');
      li.append(head, facts);
      return li;
    });
    $('machines').replaceChildren(...rows);

    // The machine filter offers what is known now, and keeps its choice.
    const chosen = $('node').value;
    const options = [new Option(T.allMachines, '')].concat(list.map(n => new Option(n.name || n.id, n.id)));
    $('node').replaceChildren(...options);
    $('node').value = list.some(n => n.id === chosen) ? chosen : '';
  }

  function row(s) {
    listed.set(`${s.node_id}/${s.key}`, s);
    const li = el('li');
    li.dataset.node = s.node_id;
    li.dataset.key = s.key;
    li.tabIndex = 0;
    li.append(el('div', 't', s.title || s.key));
    const m = el('div', 'm');
    let where = s.node || s.node_id;
    if (!s.online) where += ` · ${T.offline}`;
    m.append(el('span', s.online ? 'where' : 'where off', where));
    m.append(el('span', 'grow', [s.agent, s.project].filter(Boolean).join(' · ')));
    m.append(el('span', 'count', T.count(s.messages)));
    m.append(el('span', 'when', when(s.updated_at)));
    li.append(m);
    if (open && open.node === s.node_id && open.key === s.key) li.classList.add('selected');
    return li;
  }

  // forgetList is for when the filters change: the cursor belongs to the
  // list on screen, which is no longer the one asked for. It is not used
  // again, and there is nothing to continue until the new list is there.
  function forgetList() {
    listSeq++; // answers still on their way are for a list nobody waits for
    next = '';
    $('more').hidden = true;
  }

  // loadList shows the first page of the list, or with more the next one.
  async function loadList(more) {
    if (more && !next) return; // the list is at its end
    const after = more ? next : '';
    const seq = ++listSeq;
    const f = filters();
    $('more').disabled = true;
    let page;
    try {
      page = await api('sessions', { ...f, limit: PAGE, after });
    } catch (err) {
      if (seq !== listSeq) return;
      $('more').disabled = false; // only the newest request gives the button back
      $('list-note').hidden = false;
      $('list-note').textContent = T.failed(err.message);
      return;
    }
    if (seq !== listSeq) return; // another list was asked for meanwhile
    $('more').disabled = false;

    showMachines(page.nodes);
    if (!more) listed.clear();
    const rows = page.sessions.map(row);
    if (more) {
      $('list').append(...rows);
      pages++;
    } else {
      $('list').replaceChildren(...rows);
      pages = 1;
    }
    next = page.next || '';
    $('more').hidden = !next;
    $('more').textContent = T.more;
    const empty = $('list').children.length === 0;
    $('list-note').hidden = !empty;
    $('list-note').textContent = empty ? (filtered(f) ? T.noMatch : T.noSessions) : '';

    const seen = new Set([...$('agents').options].map(o => o.value));
    for (const s of page.sessions) {
      if (s.agent && !seen.has(s.agent)) {
        seen.add(s.agent);
        $('agents').append(new Option(s.agent, s.agent));
      }
    }
  }

  // Message text is plain text. Two things are made of it: fenced code
  // blocks are set in a monospace block, and http and https addresses become
  // links. Both are built as elements; the text itself stays text.
  function body(text) {
    const box = el('div', 'text');
    const parts = text.split(/^```[^\n]*\n?/m);
    parts.forEach((part, i) => {
      if (i % 2 === 1 && i < parts.length - 1) {
        box.append(el('pre', '', part.replace(/\n$/, '')));
        return;
      }
      // The block has its own margins: drop the line break next to it.
      if (i > 0) part = part.replace(/^\n/, '');
      if (i < parts.length - 1) part = part.replace(/\n$/, '');
      // An odd last part is a block that was opened and never closed.
      let last = 0;
      for (const m of part.matchAll(/https?:\/\/[^\s<>"'`]+/g)) {
        let url = m[0].replace(/[.,;:!?)\]}]+$/, '');
        let target;
        try { target = new URL(url); } catch { continue; }
        if (target.protocol !== 'http:' && target.protocol !== 'https:') continue;
        box.append(part.slice(last, m.index));
        const a = el('a', '', url);
        a.href = target.href;
        a.target = '_blank';
        a.rel = 'noopener noreferrer';
        box.append(a);
        last = m.index + url.length;
      }
      box.append(part.slice(last));
    });
    return box;
  }

  function message(m) {
    const art = el('article', 'msg');
    const h = el('div', 'h');
    h.append(el('span', 'role', m.role || '?'));
    h.append(el('span', '', `#${m.seq}`));
    if (m.sidechain) h.append(el('span', '', T.sideThread(m.sidechain)));
    if (m.time) h.append(el('span', '', when(m.time)));
    art.append(h);

    if (m.text.length > FOLD) {
      const short = body(m.text.slice(0, FOLD));
      const all = el('button', '', T.showAll(m.text.length));
      all.type = 'button';
      all.addEventListener('click', () => {
        short.replaceWith(body(m.text));
        all.remove();
      });
      art.append(short, all);
    } else {
      art.append(body(m.text));
    }
    if (m.truncated) art.append(el('p', 'cut', T.cut));
    return art;
  }

  function mark() {
    for (const li of $('list').children) {
      li.classList.toggle('selected', Boolean(open) && li.dataset.node === open.node && li.dataset.key === open.key);
    }
  }

  // heading puts what a list or a reading says of a session above it.
  function heading(s) {
    $('title').textContent = s.title || s.key;
    $('meta').textContent = [s.agent, s.node || s.node_id, s.project_path || s.project,
      T.messages(s.messages), T.updated(when(s.updated_at))].filter(Boolean).join(' · ');
  }

  // In the narrow layout the reader is below the list, out of sight: a
  // session that is opened has to be brought into view, and so has the
  // reason why it cannot be shown.
  const narrow = matchMedia('(max-width: 59.99rem)');
  function toReader() {
    if (narrow.matches) $('reader').scrollIntoView();
  }

  // read shows a session from its start, or with more its next page.
  async function read(node, key, more) {
    if (more && !(open && open.next)) return; // the session is at its end
    const seq = ++readSeq;
    if (!more) {
      open = { node, key, next: 0, listed: listed.get(`${node}/${key}`) };
      mark();
      $('reader-empty').hidden = true;
      $('reader-body').hidden = false;
      $('title').textContent = T.loading;
      $('meta').textContent = '';
      $('notes').replaceChildren();
      $('messages').replaceChildren();
      $('more-messages').hidden = true;
      toReader();
    }
    $('more-messages').disabled = true;
    let r;
    try {
      r = await api('sessions/messages', { node, key, after: more ? open.next : 0, limit: PAGE });
    } catch (err) {
      if (seq !== readSeq) return;
      $('more-messages').disabled = false;
      if (!more) {
        // The list knew the session by more than its key; so does this.
        if (open.listed) heading(open.listed);
        else $('title').textContent = key;
      }
      const at = nodes.find(n => n.id === node);
      const name = (at && at.name) || (open.listed && open.listed.node) || node;
      const known = Object.hasOwn(T.cannot, err.code) && T.cannot[err.code];
      $('notes').append(el('p', '', known ? known(name) : T.failed(err.message)));
      if (!more) toReader();
      return;
    }
    if (seq !== readSeq) return; // another session was opened meanwhile
    $('more-messages').disabled = false;

    if (!more) {
      heading(r.session);
      const notes = [];
      if (r.note) notes.push(r.note);
      else if (r.from === 'cache' && r.cached_at) notes.push(T.fromCopy(when(r.cached_at)));
      if (r.stale) notes.push(T.stale);
      if (r.incomplete) notes.push(T.incomplete);
      $('notes').replaceChildren(...notes.map(n => el('p', '', n)));
      $('reader').scrollTop = 0;
      toReader();
    }
    $('messages').append(...r.messages.map(message));
    open.next = r.next || 0;
    $('more-messages').hidden = !open.next;
    $('more-messages').textContent = T.more;
  }

  // The address remembers which session is open: "#s=<node id>/<key>".
  function fromHash() {
    const m = /^#s=([^/]+)\/(.+)$/.exec(location.hash);
    if (!m) return;
    const node = decodeURIComponent(m[1]), key = decodeURIComponent(m[2]);
    if (!open || open.node !== node || open.key !== key) read(node, key, false);
  }
  function choose(li) {
    location.hash = `s=${encodeURIComponent(li.dataset.node)}/${encodeURIComponent(li.dataset.key)}`;
  }

  // refresh brings the machines and the list up to date. On its own, every
  // minute, it leaves a list alone that the reader has scrolled or extended,
  // and it never touches the session being read.
  function refresh(asked) {
    const moved = pages > 1 || $('browse').scrollTop > 0 || document.documentElement.scrollTop > 0;
    if (asked || !moved) return loadList(false);
    return api('sessions', { limit: 1 }).then(page => showMachines(page.nodes), () => {});
  }

  $('refresh').textContent = T.refresh;
  $('q').placeholder = T.search;
  $('agent').placeholder = T.agent;
  $('project').placeholder = T.project;
  $('reader-empty').textContent = T.pick;
  $('since').replaceChildren(new Option(T.anyTime, ''), ...Object.keys(SPAN).map(k => new Option(T.last[k], k)));
  document.documentElement.lang = lang;

  let typing;
  $('filters').addEventListener('input', () => {
    // From the first keystroke the list on screen is not the one asked for.
    forgetList();
    clearTimeout(typing);
    typing = setTimeout(() => loadList(false), 250);
  });
  $('filters').addEventListener('submit', e => e.preventDefault());
  $('more').addEventListener('click', () => loadList(true));
  $('more-messages').addEventListener('click', () => open && read(open.node, open.key, true));
  $('refresh').addEventListener('click', async () => {
    $('refresh').disabled = true;
    await refresh(true);
    $('refresh').disabled = false;
  });
  $('list').addEventListener('click', e => {
    const li = e.target.closest('li');
    if (li) choose(li);
  });
  $('list').addEventListener('keydown', e => {
    if (e.key === 'Enter' && e.target.matches('li')) choose(e.target);
  });
  addEventListener('hashchange', fromHash);

  loadList(false).then(fromHash);
  setInterval(() => refresh(false), 60000);
})();

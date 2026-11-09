'use strict';

// The page of the h8c web interface. Everything it shows, titles, project
// names, node names and message text, comes from session files and from
// other machines. None of it is ever parsed as markup: text goes into the
// document through textContent only. Do not add innerHTML here.

(() => {
  const zh = (navigator.language || '').toLowerCase().startsWith('zh');
  const lang = zh ? 'zh-CN' : 'en';
  const T = zh ? {
    sep: '', // between two sentences
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
    modeList: '会话',
    modeSearch: '搜索',
    search: '按标题或项目名筛选',
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
    // The search of messages, thinking and calls.
    terms: '要找的词，引号里的算一个词组',
    file: '文件路径，如 importer/rows.go',
    from: '从',
    to: '到',
    range: '自定义日期…',
    moreConditions: '更多条件',
    op: '文件操作',
    ops: { '': '修改或删除', change: '修改', delete: '删除', read: '读取', any: '任意' },
    withFailed: '包括失败的尝试',
    tool: '工具名，如 shell',
    kinds: { '': '任意类型的消息', text: '正文', meta: '元信息', compact: '压缩' },
    onlyFailed: '只看失败的调用',
    group: '按会话分组',
    go: '搜索',
    intro: '在所有机器的消息、思考过程和调用里找词。',
    needSomething: '输入要找的词，或者选一个文件、agent、项目、机器或时间。',
    searching: '搜索中…',
    found: (n, more, grouped) => (more ? '前 ' : '') + (grouped ? `${n} 个会话` : `${n} 处命中`),
    noHits: '没有找到。',
    noHitsShort: '没有找到。但这次搜索不完整，不能说明没有。',
    searchFailed: msg => `搜索失败：${msg}`,
    hits: n => `${n} 处命中`,
    notAll: '结果没有全部列出。可以用文件、项目或时间范围缩小搜索。',
    wider: '最多显示 500 条',
    live: '实时搜索',
    states: { copies: '只搜了副本', none: '没有搜索' },
    partial: '结果不完整',
    why: {
      copies: '它没有回答，只搜了本机以前读过的那些会话的副本。',
      none: '它没有回答，本机也没有读过它的会话。',
      noneSelf: '本机的会话没能搜索。',
      unfinished: '对它的搜索没有完成，可能漏掉了它那里的结果。',
      partial: '有一部分内容没能完整搜索，可能漏掉了结果。',
    },
    through: t => `早于 ${t} 的消息没有搜索。`,
    note: note => `说明：${note}`,
    // A session read around a place that was found.
    earlier: '更早的消息',
    match: '命中',
    matchMoved: '命中，位置可能变了',
    moved: '这个会话在搜索之后变过，命中的位置可能已经移动。',
    nearest: '原来那条消息已经不在了，标出的是它后面的一条。',
    gone: '这个位置已经超出会话末尾，下面是会话最后的消息。',
    tooLarge: '前面的消息太大，没法和这些一起取过来。',
    fromStart: '从头阅读',
    kind: { meta: '元信息', compact: '压缩' },
    thinking: '思考过程',
    callFailed: '失败',
    fileOps: { change: '修改', delete: '删除', read: '读取' },
    input: '输入',
    output: '输出',
    nothing: '没有记录输入和输出。',
    callCut: '输入或输出在传过来时被截断了，完整的留在它所在的机器上。',
    subagent: key => `启动了会话 ${key}`,
    dropped: n => `还有 ${n} 个调用太大，没有传过来。`,
  } : {
    sep: ' ',
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
    modeList: 'Sessions',
    modeSearch: 'Search',
    search: 'Filter by title or project name',
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
    // The search of messages, thinking and calls.
    terms: 'Words to find; "words in quotes" stay together',
    file: 'File path, such as importer/rows.go',
    from: 'From',
    to: 'to',
    range: 'Dates…',
    moreConditions: 'More conditions',
    op: 'File operation',
    ops: { '': 'changed or deleted', change: 'changed', delete: 'deleted', read: 'read', any: 'any' },
    withFailed: 'Include failed attempts',
    tool: 'Tool, such as shell',
    kinds: { '': 'Any kind of message', text: 'text', meta: 'meta', compact: 'compaction' },
    onlyFailed: 'Failed calls only',
    group: 'Group by session',
    go: 'Search',
    intro: 'Finds words in the messages, thinking and calls of every machine.',
    needSomething: 'Type words to find, or choose a file, an agent, a project, a machine or a time.',
    searching: 'Searching…',
    found: (n, more, grouped) => (more ? 'The first ' : '') +
      (grouped ? (n === 1 && !more ? '1 session' : `${n} sessions`) : (n === 1 && !more ? '1 match' : `${n} matches`)),
    noHits: 'Nothing found.',
    noHitsShort: 'Nothing found. But the search was not complete, so that does not mean there is nothing.',
    searchFailed: msg => `Could not search: ${msg}`,
    hits: n => n === 1 ? '1 match' : `${n} matches`,
    notAll: 'Not everything found is listed. Narrow the search with a file, a project or a time.',
    wider: 'Show up to 500',
    live: 'searched live',
    states: { copies: 'copies only', none: 'not searched' },
    partial: 'incomplete',
    why: {
      copies: 'It did not answer, so only the sessions read on this machine before were searched.',
      none: 'It did not answer, and none of its sessions was read on this machine before.',
      noneSelf: 'The sessions of this machine could not be searched.',
      unfinished: 'Its search did not complete, so matches from it may be missing.',
      partial: 'Some of what it holds could not be searched in full, so matches may be missing.',
    },
    through: t => `Messages older than ${t} were not searched.`,
    note: note => `It says: ${note}`,
    // A session read around a place that was found.
    earlier: 'Earlier messages',
    match: 'match',
    matchMoved: 'match, may have moved',
    moved: 'This session has changed since the search, so the match may have moved.',
    nearest: 'Its message is gone; the one after it is marked.',
    gone: 'The place is now past the end of the session; its last messages are shown.',
    tooLarge: 'The messages before these are too large to be fetched with them.',
    fromStart: 'Read from the start',
    kind: { meta: 'meta', compact: 'compaction' },
    thinking: 'Thinking',
    callFailed: 'failed',
    fileOps: { change: 'changed', delete: 'deleted', read: 'read' },
    input: 'Input',
    output: 'Output',
    nothing: 'No input or output was recorded.',
    callCut: 'The input or output was cut on the way here; the whole of it stays on its machine.',
    subagent: key => `started the session ${key}`,
    dropped: n => n === 1 ? '1 more call was too large to pass on.' : `${n} more calls were too large to pass on.`,
  };

  const $ = id => document.getElementById(id);
  // el makes an element with a class and text. Text is always set as text.
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  // word is what a table of the page says for a value that came from a
  // session or a node, or else the value itself: never something the table
  // only inherits, such as its constructor.
  const word = (table, key) => Object.hasOwn(table, key) ? table[key] : key;
  // when writes a time the short way: the year only if it is not this one.
  // It takes the times of a list (text) and of messages and searches
  // (milliseconds) alike.
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
  // agentOf is the agent of a session, which its key starts with.
  const agentOf = key => key.includes(':') ? key.slice(0, key.indexOf(':')) : '';
  const PAGE = 50;
  const SPAN = { '24h': 864e5, '7d': 7 * 864e5, '30d': 30 * 864e5 };
  const FOLD = 4000; // characters of a message shown before "show all"
  const BEFORE = 10, AFTER = 10; // messages shown around a place found
  const WIDE = 500; // the most a search answers with

  let nodes = [];
  let next = '';       // continues the list
  let pages = 0;       // pages of the list on screen
  let listSeq = 0;     // the newest list request; older answers are dropped
  let open = null;     // the session being read: {node, key, next, anchor, first, earlier, base, listed}
  let readSeq = 0;
  const listed = new Map(); // the sessions of the list on screen, by node and key
  let mode = 'list';   // the view on the left: the list, or the search
  const scrolled = { list: 0, search: 0 }; // how far each view was scrolled
  let searchSeq = 0;   // the newest search; older answers are dropped
  let shown = null;    // {params, limit} of the search whose answer is on screen
  let pending = null;  // {params, limit} of the newest search, until its answer comes
  let begun = false;   // whether a search was asked for from the form yet
  const found = new Map(); // the sessions of the search on screen, by node and key

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
    note.textContent = notes.join(T.sep);

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

    // The machine filters offer what is known now, and keep their choice.
    for (const id of ['node', 's-node']) {
      const chosen = $(id).value;
      const options = [new Option(T.allMachines, '')].concat(list.map(n => new Option(n.name || n.id, n.id)));
      $(id).replaceChildren(...options);
      $(id).value = list.some(n => n.id === chosen) ? chosen : '';
    }
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

  // termsOf splits what was typed into the terms of a search: each word on
  // its own, and what stands in quotes as one.
  const termsOf = s => [...s.matchAll(/["“”]([^"“”]*)["“”]?|([^\s"“”]+)/g)]
    .map(m => (m[1] ?? m[2]).trim()).filter(Boolean);
  // day is the start of a day of the calendar here, or of the one after it.
  const day = (value, after) => {
    const d = new Date(value + 'T00:00');
    d.setDate(d.getDate() + after);
    return d.getTime();
  };

  // searchParams are the conditions of the search form, as the node takes them.
  function searchParams() {
    const p = {
      q: termsOf($('s-q').value),
      file: $('s-file').value.trim(),
      node: $('s-node').value,
      agent: $('s-agent').value.trim(),
      project: $('s-project').value.trim(),
      tool: $('s-tool').value.trim(),
      kind: $('s-kind').value,
      error: $('s-error').checked ? 'true' : '',
      by_session: $('s-group').checked ? 'true' : '',
      since: '',
      until: '',
    };
    // What was done to a file, and whether failed attempts count, are
    // conditions on the file.
    if (p.file) {
      p.op = $('s-op').value;
      p.failed = $('s-failed').checked ? 'true' : '';
    }
    const span = $('s-when').value;
    if (SPAN[span]) {
      p.since = Date.now() - SPAN[span];
    } else if (span === 'range') {
      if ($('s-from').value) p.since = day($('s-from').value, 0);
      if ($('s-to').value) p.until = day($('s-to').value, 1) - 1;
    }
    return p;
  }
  // asks says whether there is anything to look for. Without it the search
  // would list every message there is.
  const asks = p => Boolean(p.q.length || p.file || p.node || p.agent || p.project || p.tool || p.kind ||
    p.error || p.since || p.until);

  const searching = busy => {
    $('s-go').disabled = busy;
    $('search-wider').disabled = busy;
  };

  // search asks every machine for what params say, at most limit hits or
  // sessions, and shows the answer. A search of what is on screen, for
  // more of it or anew, leaves it there until the answer comes.
  async function search(params, limit) {
    const seq = ++searchSeq;
    const again = shown !== null && params === shown.params;
    searching(false);
    if (!asks(params)) {
      shown = pending = null;
      found.clear();
      $('search-note').textContent = T.needSomething;
      $('search-nodes').hidden = true;
      $('hits').replaceChildren();
      $('search-more').hidden = true;
      return;
    }
    pending = { params, limit };
    searching(true);
    if (!again) {
      $('search-note').textContent = T.searching;
      $('search-nodes').hidden = true;
      $('hits').replaceChildren();
      $('search-more').hidden = true;
    }
    let r;
    try {
      r = await api('sessions/search', { ...params, limit });
    } catch (err) {
      if (seq !== searchSeq) return;
      pending = null;
      searching(false);
      if (!again) shown = null; // what was on screen is gone
      $('search-note').textContent = T.searchFailed(err.message);
      return;
    }
    if (seq !== searchSeq) return; // another search was asked for meanwhile
    pending = null;
    searching(false);
    shown = { params, limit };
    const grouped = Boolean(params.by_session);
    const items = grouped ? (r.sessions || []) : r.hits;
    found.clear();
    showStates(r.nodes);
    $('hits').replaceChildren(...items.map(x => grouped ? group(x) : hit(x, params.q)));
    // No match from a search that was not complete is no proof of none.
    const short = r.nodes.some(n => n.partial || n.searched === 'none');
    $('search-note').textContent = items.length ? T.found(items.length, r.more, grouped) : short ? T.noHitsShort : T.noHits;
    $('search-more').hidden = !r.more;
    $('search-more-note').textContent = T.notAll;
    $('search-wider').hidden = limit >= WIDE;
    $('search-wider').textContent = T.wider;
    mark();
  }

  // showStates says how each machine was searched: live, in the copies
  // kept here, or not at all, and whether what it gave is complete. The
  // machines that answered in full share one line; each other one has a
  // line of its own that says what is missing.
  function showStates(list) {
    const rows = [];
    const whole = list.filter(n => n.searched === 'live' && !n.partial && !n.note);
    if (whole.length) {
      const li = el('li', 'live');
      for (const n of whole) {
        const name = el('span', 'name', n.name || n.id);
        name.dataset.node = n.id;
        name.dataset.searched = n.searched;
        li.append(name);
      }
      li.append(el('span', 'state', T.live));
      rows.push(li);
    }
    for (const n of list) {
      if (whole.includes(n)) continue;
      const li = el('li', 'short');
      li.dataset.node = n.id;
      li.dataset.searched = n.searched;
      if (n.partial) li.dataset.partial = 'true';
      const state = [n.searched === 'live' ? T.live : word(T.states, n.searched)];
      if (n.partial) state.push(T.partial);
      li.append(el('span', 'name', n.name || n.id), el('span', 'state', state.join(' · ')));
      const why = [];
      if (n.searched === 'copies') why.push(T.why.copies);
      if (n.searched === 'none') {
        why.push(nodes.some(x => x.id === n.id && x.self) ? T.why.noneSelf : n.partial ? T.why.unfinished : T.why.none);
      } else if (n.partial) {
        why.push(T.why.partial);
      }
      const cut = through(n.note);
      if (cut) why.push(T.through(when(cut)));
      if (why.length) li.append(el('div', 'why', why.join(T.sep)));
      if (n.note) li.append(el('div', 'why note', T.note(n.note)));
      rows.push(li);
    }
    $('search-nodes').replaceChildren(...rows);
    $('search-nodes').hidden = rows.length === 0;
  }

  // through is how far back a node searched before its budget ran out,
  // when its note says so: the time of the oldest message it reached. The
  // note itself is in English, and shown as it came.
  const through = note => {
    const m = /\bsearched newest messages through (\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ)/.exec(note || '');
    return m && !Number.isNaN(Date.parse(m[1])) ? m[1] : '';
  };

  // place starts the row of a place found: a message, a call in it, or the
  // first match in a session. Choosing it opens the session there.
  function place(s, at, call) {
    found.set(`${s.node_id}/${s.key}`, { node_id: s.node_id, node: s.node, key: s.key, title: s.title,
      agent: s.agent || agentOf(s.key), project: s.project, project_path: s.project_path, updated_at: s.updated_at });
    const li = el('li');
    li.dataset.node = s.node_id;
    li.dataset.key = s.key;
    li.dataset.at = at;
    li.dataset.rev = s.rev;
    if (call !== undefined) li.dataset.call = call;
    li.tabIndex = 0;
    li.append(el('div', 't', s.title || s.key));
    return li;
  }
  // facts is the line under a title: machine, agent and project, time.
  function facts(s, time, count) {
    const m = el('div', 'm');
    m.append(el('span', 'where', s.node || s.node_id));
    m.append(el('span', 'grow', [s.agent || agentOf(s.key), s.project].filter(Boolean).join(' · ')));
    if (count) m.append(el('span', 'count', count));
    m.append(el('span', 'when', when(time)));
    return m;
  }
  function hit(h, terms) {
    const li = place(h, h.id, h.call);
    li.append(facts(h, h.time || h.updated_at));
    if (h.call !== undefined) {
      const c = el('div', 'call');
      c.append(el('span', 'tool', h.tool || '?'));
      for (const f of h.files || []) c.append(file(f, h.project_path || ''));
      li.append(c);
    }
    li.append(snippet(h.snippet, terms));
    return li;
  }
  function group(g) {
    const li = place(g, g.first);
    li.append(facts(g, g.updated_at, T.hits(g.hits)));
    return li;
  }

  // snippet is the text around a match, on as few lines as it takes, with
  // the terms marked. The marks are elements; the text stays text.
  function snippet(text, terms) {
    const p = el('p', 'snip');
    text = text.replace(/\s+/g, ' ').trim();
    const words = terms.map(t => t.replace(/\s+/g, ' ')).filter(Boolean).sort((a, b) => b.length - a.length);
    if (words.length === 0) {
      p.textContent = text;
      return p;
    }
    const re = new RegExp(words.map(w => w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'giu');
    let last = 0;
    for (const m of text.matchAll(re)) {
      p.append(text.slice(last, m.index), el('mark', '', m[0]));
      last = m.index + m[0].length;
    }
    p.append(text.slice(last));
    return p;
  }

  // file names a file a call was given: what was done to it, and its path,
  // said from the project of the session where it is in there. The whole
  // path is there on hover.
  function file(f, base) {
    const span = el('span', 'file');
    const path = el('span', 'path');
    for (const part of inside(f.path, base).split(/(?<=[/\\])/)) path.append(part, el('wbr'));
    span.append(el('span', 'op', word(T.fileOps, f.op)), '\u00a0', path);
    span.title = f.path;
    return span;
  }
  const inside = (path, base) => {
    for (const sep of base ? ['/', '\\'] : []) {
      const root = base.endsWith(sep) ? base : base + sep;
      if (path.startsWith(root) && path.length > root.length) return path.slice(root.length);
    }
    return path;
  };

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

  // fold is a part of a message that is closed at first, such as a call or
  // the thinking. What is inside is made when it is first opened.
  function fold(cls, summary, inner) {
    const d = el('details', cls);
    d.append(summary);
    d.addEventListener('toggle', () => {
      if (d.open && d.childElementCount === 1) d.append(...inner());
    });
    return d;
  }

  // calls lists the calls a message made: each with its tool, whether it
  // failed and the files it was given, its input and output folded away.
  function calls(m, base) {
    const ul = el('ul', 'calls');
    (m.calls || []).forEach((c, i) => {
      const li = el('li', c.error ? 'failed' : '');
      li.dataset.call = i;
      const s = el('summary');
      s.append(el('span', 'tool', c.tool || '?'));
      if (c.error) s.append(el('span', 'bad', T.callFailed));
      for (const f of c.files || []) s.append(file(f, base));
      li.append(fold('', s, () => io(c)));
      ul.append(li);
    });
    if (m.calls_dropped) ul.append(el('li', 'dropped', T.dropped(m.calls_dropped)));
    return ul;
  }
  // io is what a call was given and what it gave back.
  function io(c) {
    const out = [];
    if (c.input) out.push(el('p', 'label', T.input), el('pre', '', c.input));
    if (c.output) out.push(el('p', 'label', T.output), el('pre', '', c.output));
    if (!c.input && !c.output) out.push(el('p', 'label', T.nothing));
    if (c.truncated) out.push(el('p', 'cut', T.callCut));
    if (c.agent) out.push(el('p', 'label', T.subagent(c.agent)));
    return out;
  }

  function message(m, base) {
    const art = el('article', 'msg');
    art.dataset.id = m.id;
    const h = el('div', 'h');
    h.append(el('span', 'role', m.role || '?'));
    h.append(el('span', '', `#${m.seq}`));
    if (m.sidechain) h.append(el('span', '', T.sideThread(m.sidechain)));
    if (m.time) h.append(el('span', '', when(m.time)));
    if (m.kind && m.kind !== 'text') h.append(el('span', 'kind', word(T.kind, m.kind)));
    art.append(h);

    if (m.thinking) art.append(fold('thinking', el('summary', '', T.thinking), () => [el('div', 'text', m.thinking)]));
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
    if ((m.calls && m.calls.length) || m.calls_dropped) art.append(calls(m, base));
    return art;
  }

  // mark shows which session is open in the list, and which place found
  // in the search.
  function mark() {
    for (const li of $('list').children) {
      li.classList.toggle('selected', Boolean(open) && li.dataset.node === open.node && li.dataset.key === open.key);
    }
    const a = open && open.anchor;
    for (const li of $('hits').children) {
      li.classList.toggle('selected', Boolean(a) && li.dataset.node === open.node && li.dataset.key === open.key &&
        li.dataset.at === String(a.id) && (li.dataset.call === undefined ? -1 : Number(li.dataset.call)) === a.call);
    }
  }

  // heading puts what a list, a search or a reading says of a session
  // above it.
  function heading(s) {
    $('title').textContent = s.title || s.key;
    $('meta').textContent = [s.agent, s.node || s.node_id, s.project_path || s.project,
      s.messages === undefined ? '' : T.messages(s.messages), T.updated(when(s.updated_at))].filter(Boolean).join(' · ');
  }

  // In the narrow layout the reader is below the list, out of sight: a
  // session that is opened has to be brought into view, and so has the
  // reason why it cannot be shown.
  const narrow = matchMedia('(max-width: 59.99rem)');
  function toReader() {
    if (narrow.matches) $('reader').scrollIntoView();
  }
  // reveal brings the place found into view, with a little of what came
  // before it above.
  function reveal() {
    const e = $('messages').querySelector('.msg.anchor');
    if (!e) return;
    e.scrollIntoView({ block: 'start' });
    const scroller = narrow.matches ? document.scrollingElement : $('reader');
    scroller.scrollTop -= Math.round(scroller.clientHeight / 5);
  }
  // pin marks the message of a place found, and the call in it if that is
  // the place, which it opens.
  function pin(art, call, moved) {
    art.classList.add('anchor');
    art.querySelector('.h').append(el('span', 'match', moved ? T.matchMoved : T.match));
    const li = [...art.querySelectorAll('.calls > li')].find(c => c.dataset.call === String(call));
    if (li) {
      li.classList.add('anchor');
      li.querySelector('details').open = true;
    }
  }
  const reading = busy => {
    $('more-messages').disabled = busy;
    $('earlier').disabled = busy;
  };

  // read shows a session: from its start, or around a place found in it
  // (anchor: {id, rev, call}), or with more its next page.
  async function read(node, key, more, anchor) {
    if (more && !(open && open.next)) return; // the session is at its end
    const seq = ++readSeq;
    if (!more) {
      const id = `${node}/${key}`;
      open = { node, key, next: 0, anchor: anchor || null, first: 0, earlier: false, base: '',
        listed: listed.get(id) || found.get(id) };
      mark();
      $('reader-empty').hidden = true;
      $('reader-body').hidden = false;
      $('title').textContent = T.loading;
      $('meta').textContent = '';
      $('notes').replaceChildren();
      $('messages').replaceChildren();
      $('more-messages').hidden = true;
      $('earlier').hidden = true;
      toReader();
    }
    reading(true);
    let r;
    try {
      r = await api('sessions/messages', !more && open.anchor
        ? { node, key, around: open.anchor.id, before: BEFORE, after: AFTER, rev: open.anchor.rev }
        : { node, key, after: more ? open.next : 0, limit: PAGE });
    } catch (err) {
      if (seq !== readSeq) return;
      reading(false);
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
    reading(false);

    if (more) {
      $('messages').append(...r.messages.map(m => message(m, open.base)));
    } else {
      open.base = r.session.project_path || '';
      heading(r.session);
      const shownHere = r.messages.map(m => message(m, open.base));
      $('messages').append(...shownHere);
      const notes = [];
      if (r.note) notes.push(r.note);
      else if (r.from === 'cache' && r.cached_at) notes.push(T.fromCopy(when(r.cached_at)));
      if (r.stale) notes.push(T.stale);
      if (r.incomplete) notes.push(T.incomplete);
      const a = open.anchor;
      if (a) {
        // The place is the message with its id or, when the session has
        // changed and it is gone, the next one there is.
        const i = r.messages.findIndex(m => m.id >= a.id);
        const same = i >= 0 && r.messages[i].id === a.id;
        if (r.moved) notes.push([T.moved, i < 0 ? T.gone : same ? '' : T.nearest].filter(Boolean).join(T.sep));
        if (i >= 0) pin(shownHere[i], same ? a.call : -1, Boolean(r.moved));
        // Whether messages come before these, the node says: when they are
        // large it sends fewer than were asked for.
        open.first = r.messages.length ? r.messages[0].id : 0;
        open.earlier = Boolean(r.has_before) && open.first > 0;
        $('earlier').hidden = !open.earlier;
      }
      $('notes').replaceChildren(...notes.map(n => el('p', '', n)));
      if ($('messages').querySelector('.msg.anchor')) {
        reveal();
      } else {
        $('reader').scrollTop = 0;
        toReader();
      }
    }
    open.next = r.next || 0;
    $('more-messages').hidden = !open.next;
    $('more-messages').textContent = T.more;
  }

  // earlier puts the messages before the first one on screen above it.
  async function earlier() {
    if (!open || !open.earlier) return;
    const seq = ++readSeq;
    const { node, key, first } = open;
    reading(true);
    let r;
    try {
      r = await api('sessions/messages', { node, key, around: first, before: PAGE, after: 0 });
    } catch (err) {
      if (seq !== readSeq) return;
      reading(false);
      $('notes').append(el('p', '', T.failed(err.message)));
      return;
    }
    if (seq !== readSeq) return; // another session was opened meanwhile
    reading(false);
    const add = r.messages.filter(m => m.id < first);
    $('messages').prepend(...add.map(m => message(m, open.base)));
    if (add.length) open.first = add[0].id;
    open.earlier = Boolean(r.has_before) && add.length > 0;
    $('earlier').hidden = !open.earlier;
    // There are earlier messages, but the one before the first shown and
    // that one are too large for one answer, which always holds the
    // message it is asked around. From the start, a page holds one.
    if (r.has_before && !add.length) {
      const p = el('p', 'stuck', T.tooLarge + ' ');
      const start = el('button', '', T.fromStart);
      start.type = 'button';
      start.addEventListener('click', () => {
        location.hash = `s=${encodeURIComponent(node)}/${encodeURIComponent(key)}`;
      });
      p.append(start);
      $('notes').append(p);
    }
  }

  // The address remembers which session is open, "#s=<node id>/<key>",
  // and a place found in it: "&at=<message id>&rev=<revision>", with
  // "&call=<index>" when the place is a call.
  function fromHash() {
    const m = /^#s=([^/&]+)\/([^&]+)(.*)$/.exec(location.hash);
    if (!m) return;
    let node, key;
    try {
      node = decodeURIComponent(m[1]);
      key = decodeURIComponent(m[2]);
    } catch {
      return; // not an address this page made
    }
    const more = new URLSearchParams(m[3]);
    const id = Number(more.get('at'));
    const rev = more.get('rev') || '';
    const call = more.has('call') ? Number(more.get('call')) : -1;
    const anchor = Number.isSafeInteger(id) && id > 0
      ? { id, rev: /^\d+$/.test(rev) ? rev : '', call: Number.isSafeInteger(call) ? call : -1 } : null;
    const same = open && open.anchor && anchor ? open.anchor.id === anchor.id && open.anchor.rev === anchor.rev &&
      open.anchor.call === anchor.call : !(open && open.anchor) && !anchor;
    if (!open || open.node !== node || open.key !== key || !same) read(node, key, false, anchor);
  }
  function choose(li) {
    location.hash = `s=${encodeURIComponent(li.dataset.node)}/${encodeURIComponent(li.dataset.key)}`;
  }
  // chooseFound opens a place found. Chosen again, it is brought back into
  // view, which the address alone would not do.
  function chooseFound(li) {
    const d = li.dataset;
    const hash = `#s=${encodeURIComponent(d.node)}/${encodeURIComponent(d.key)}&at=${encodeURIComponent(d.at)}` +
      `&rev=${encodeURIComponent(d.rev)}` + (d.call === undefined ? '' : `&call=${encodeURIComponent(d.call)}`);
    if (location.hash === hash) reveal();
    else location.hash = hash;
  }

  // setMode shows the list or the search on the left, each where it was.
  function setMode(to) {
    if (to === mode) return;
    scrolled[mode] = $('browse').scrollTop;
    mode = to;
    $('list-view').hidden = mode !== 'list';
    $('search-view').hidden = mode !== 'search';
    $('mode-list').ariaPressed = String(mode === 'list');
    $('mode-search').ariaPressed = String(mode === 'search');
    $('browse').scrollTop = scrolled[mode];
    if (mode === 'search' && !narrow.matches) $('s-q').focus();
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
  $('mode-list').textContent = T.modeList;
  $('mode-search').textContent = T.modeSearch;
  $('mode-list').ariaPressed = 'true';
  $('mode-search').ariaPressed = 'false';
  $('q').placeholder = T.search;
  $('agent').placeholder = T.agent;
  $('project').placeholder = T.project;
  $('since').replaceChildren(new Option(T.anyTime, ''), ...Object.keys(SPAN).map(k => new Option(T.last[k], k)));
  $('s-q').placeholder = T.terms;
  $('s-file').placeholder = T.file;
  $('s-agent').placeholder = T.agent;
  $('s-project').placeholder = T.project;
  $('s-when').replaceChildren(new Option(T.anyTime, ''), ...Object.keys(SPAN).map(k => new Option(T.last[k], k)),
    new Option(T.range, 'range'));
  $('s-from-label').textContent = T.from;
  $('s-to-label').textContent = T.to;
  $('s-more-title').textContent = T.moreConditions;
  $('s-op-label').textContent = T.op;
  $('s-op').replaceChildren(...Object.entries(T.ops).map(([value, text]) => new Option(text, value)));
  $('s-failed-label').textContent = T.withFailed;
  $('s-tool').placeholder = T.tool;
  $('s-kind').replaceChildren(...Object.entries(T.kinds).map(([value, text]) => new Option(text, value)));
  $('s-error-label').textContent = T.onlyFailed;
  $('s-group-label').textContent = T.group;
  $('s-go').textContent = T.go;
  $('search-note').textContent = T.intro;
  $('earlier').textContent = T.earlier;
  $('reader-empty').textContent = T.pick;
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
  $('earlier').addEventListener('click', earlier);
  $('refresh').addEventListener('click', async () => {
    $('refresh').disabled = true;
    // The search is asked again too, as it was asked: the one on its way
    // if there is one, else the one on screen.
    const again = mode === 'search' && (pending || shown);
    await Promise.all([refresh(true), again ? search(again.params, again.limit) : null]);
    $('refresh').disabled = false;
  });
  $('list').addEventListener('click', e => {
    const li = e.target.closest('li');
    if (li) choose(li);
  });
  $('list').addEventListener('keydown', e => {
    if (e.key === 'Enter' && e.target.matches('li')) choose(e.target);
  });
  $('mode-list').addEventListener('click', () => setMode('list'));
  $('mode-search').addEventListener('click', () => setMode('search'));
  // A search starts with Enter or the button. After that, a choice in the
  // form asks again at once; typing still waits for Enter.
  $('search').addEventListener('submit', e => {
    e.preventDefault();
    begun = true;
    search(searchParams(), PAGE);
  });
  $('search').addEventListener('change', e => {
    const t = e.target;
    if (t.id === 's-when') $('s-range').hidden = t.value !== 'range';
    if (begun && t.matches('select, [type=checkbox], [type=date]') && !(t.id === 's-when' && t.value === 'range')) {
      search(searchParams(), PAGE);
    }
  });
  $('search-wider').addEventListener('click', () => shown && search(shown.params, WIDE));
  $('hits').addEventListener('click', e => {
    const li = e.target.closest('li');
    if (li) chooseFound(li);
  });
  $('hits').addEventListener('keydown', e => {
    if (e.key === 'Enter' && e.target.matches('li')) chooseFound(e.target);
  });
  addEventListener('hashchange', fromHash);

  loadList(false).then(fromHash);
  setInterval(() => refresh(false), 60000);
})();

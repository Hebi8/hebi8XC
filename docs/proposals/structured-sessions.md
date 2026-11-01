# Proposal: structured sessions

Written 2026-11-02, revised the same day after a review. A design, not an implementation. It takes items 1, 2 and 6 of "What could come next" in [roadmap.md](../roadmap.md) together, because they share one piece of work, and adds a consumer outside this repository: the user's personal data store, *lifelog*, which is to take its session events from h8c instead of parsing agent files itself.

In one sentence: h8c reads the Claude Code and Codex session files itself, keeps a structured index of them (what kind of row, which tool, which files, whether it failed, what it cost), publishes that structure to peers as optional additions to protocol 1, answers `h8c search` across machines with anchors into sessions, and offers the local index to lifelog over the control socket, one device at a time.

Where this document describes real data, it was checked on 2026-11-02 on one of the user's machines, looking at structure and counts only. Numbers are rounded; names and values in examples are made up.

## 1. Where things stand

- **Wake v0.9.2** stores one row per message with role, time and text only, and an FTS5 trigram index over the text. A tool call is rendered into the assistant's text, so searching for a path finds it, but nothing says which rows are calls, which failed, which file was changed, or what a model call cost. Per session it has one token total and the path of the native file. Subagent transcripts of Claude Code are sessions of their own with a parent key.
- **h8c** carries what Wake has and no more. `h8c sessions --text` matches titles and project names. Local sessions are read from the index on demand; other nodes' sessions are copied only when someone reads them.
- **The agents' own files** have the rest. Claude Code writes one JSON record per line: an id, the parent record, the session, the time, the working directory and branch, how the agent was started, and the message's content blocks (text, thinking, a tool call with its input, a tool result with an error flag) with the usage of each API call; subagent transcripts are separate files under the session's directory. Codex writes one record per line with a type and a payload: session metadata (id, working directory, git branch and commit, version, how it was started), per-turn context (model), completed items (user and agent messages, command executions with exit code and duration, file changes, MCP tool calls with status, reasoning summaries) and token usage per model response. Both formats gain record types with every release; a reader must skip what it does not know. On the machine checked: about 900 Claude Code transcripts (1.7 GB, 186 of them subagents), about 300 Codex rollouts (1.0 GB), Wake's index of every agent 196 MB for 2300 sessions.
- **Antigravity ACP** is already read natively for text. Which protobuf fields hold tool calls and thinking was noted during the review of that work but is not in code; it has to be verified against the descriptors the way the text fields were.
- **lifelog** has a collector per device that pushes events to a hub. Cursor on the device; the hub deduplicates on `(device, source, source_id)` and replaces every column on a duplicate. For Claude Code it keeps one event per user or assistant record that has text (`source_id` = the record id, the record's text blocks joined, `session_id` = the file's name, `app` = how the agent was started as the record says it, `payload` = the record minus a few large fields, with non-text content blocks reduced to placeholders) and one session event per file (`source_id` = `session:<file name>`); it does not keep tool calls, results, thinking, meta records, sidechains or subagent files. For Codex it keeps user and agent message items (`source_id` = thread, turn and item id; `app` = the originator; `payload` = the line plus a `session` object built from the file's metadata) and a session event per file, and skips spawned threads entirely. Its stated rules: completeness first; the cursor stays on the device; a changed `source_id` rule means deleting the source's rows and pushing everything again.
- **obelisk** does the single-machine version of this and is where several ideas below come from (ideas only; it is AGPL and none of its code is used): rows for tool calls and results with an error flag and a file path, subagents hanging off the call that spawned them, every answer carrying ids that lead back to the original line, "structure before text" and "evidence before conclusion" as rules for the agent. Its gaps are as instructive: no file linkage for Codex, tool inputs outside the full-text index, a continued session's copied history resolved by whoever wrote last, and no second machine.

## 2. Where the structure comes from

| | A. Native readers in h8c | B. Wake, plus the native file when a session is read | C. Wake only |
| --- | --- | --- | --- |
| Gives | Calls, results, files, usage, subagent links, indexed, for Claude Code and Codex; Wake for the other agents | The same, for one session, while someone reads it | Text |
| "Which sessions changed this file" | One query on the index, across machines | A scan of every native file, this machine only | A text search, this machine only |
| Offline machines | Structure travels with messages and feed entries | Text only | Text only |
| Keeps up with the agents | Two parsers, each knowing a handful of record shapes | The same parsers, no index | Wake's schedule |
| Wake schema risk | Gone for the two main agents | As today | As today |

**Recommendation: A.** The parsers are needed in any case, because lifelog's parity requirement (section 6) cannot be met from Wake's text; B and C would pay for them without the index that answers the questions. The user has decided that collecting sessions is h8c's job for the long term, which argues for owning the readers rather than depending on Wake's roadmap. Wake stays for every other agent, and as the fallback for a file the native reader cannot read: that session is then listed and shown as text, as today, and the failure is logged once per file.

**One session per file**, as Wake has it (its `file_path` is unique). For Claude Code that is a transcript or a subagent transcript; for Codex a rollout file, so a thread that Codex continues in a second file is two sessions, each knowing its thread id (`thread` in section 3). Wake lists the same files: the Wake reader keeps the native file path of each session (local only, never published) and `Combined` drops a Wake session whose file a native reader has indexed. Keys: `claude-code:<file name>` and `codex:<file name>`, which is Wake's key for a transcript and for a rollout that holds a whole thread. Whether Wake keys a continuation file the same way is not known and cannot be learned from `waketest`, which takes whatever keys a test gives it; it is checked by hand on a machine that has such a thread before phase 2 ships. A key that differs from Wake's costs nothing but a deletion and an addition in the feed; the Wake entry is still dropped by file path.

## 3. The model

### Messages

One row per native record. Two exceptions: a Claude Code record with several content blocks gives one row per block, and a Codex item that holds a command and its result gives a call row and a result row. `sessions.Message` keeps its fields with their meaning (`seq` stays the number of the message within the session; the native readers count rows in file order) and gains these, all optional:

| Field | Meaning |
| --- | --- |
| `kind` | `text`, `thinking`, `tool_use`, `tool_result`, `usage`, `compaction`; empty for a row from Wake, which is text as before |
| `tool` | the tool's name on a call and on its result (`Edit`, `Bash`, `exec`, `apply_patch`, `<server>.<tool>` for MCP) |
| `call` | the id that joins a call to its result |
| `error` | true on a result that failed: the agent's error flag, or an exit code other than zero |
| `files` | `[{path, op}]` on a call, `op` one of `change`, `delete`, `read`: the paths the call named, as the agent recorded them. Whether the change happened is the result row's `error` |
| `usage` | `{input, output, cache_read, cache_write}` tokens of one model call, on exactly one row per call |
| `model` | the model of that call, when it differs from the session's |
| `app` | how the agent was started, as the record says it (its CLI, a desktop app, an SDK, an orchestrator); lifelog keeps this per record |
| `turn` | the turn the row belongs to, where the agent numbers turns |
| `duration` | milliseconds a call took, where recorded |
| `agent` | on the call that started a subagent, the key of the subagent's session |
| `native` | the record's own id in the agent's file; the anchor into it, and lifelog's `source_id` |
| `block` | the index of the block within the record, when a record gave more than one row |
| `line` | the line of the record in the agent's file |
| `images` | how many images a record carried; a user record with only an image is a `text` row with empty text and `images: 1` |
| `copy` | true for a row whose record was written in another session and copied into this file (Claude Code continuation); `origin` then names that session's key |
| `meta` | true for rows the agent injected rather than the user or the model wrote: compaction summaries, command output, reminders |

Rules the readers follow:

- The text of a row is the block's text; for a call it is the input rendered as text (so a path or a command is found by full-text search, which is the gap obelisk left); for a result it is the output. Results are cut at 8 000 characters, thinking at 32 000, inputs at 16 000, text at `MaxText` as today, each marked `truncated`. The cuts are constants; they bound the index and what travels between nodes, not the export of section 6, which reads the file. The estimate for a heavy machine is a local index of a few hundred megabytes a month before the agents' own housekeeping.
- Usage is attached to one row per model call. Claude Code writes one record per content block of a call and repeats the call's usage on each; the reader keeps the values of the last record of the call, which also covers the case where they are not identical. Codex writes usage as its own record, which becomes a row of kind `usage` with no text. A session's totals are the sums over calls; cache tokens are kept apart from input tokens, never added into them.
- `files` comes from the tool's input and result: the path of an edit or write, the paths of a Codex file change, the files a shell command edited where the agent recorded that, and the path of a read. A change counts as made when the call has a result row without `error`. Not every edit is visible: a shell command that writes a file without the agent noticing it is not. The index says what the agent recorded.
- A Claude Code record carries the session it was first written in. One whose session is not the file's is a `copy`. Sums skip copies; search returns originals unless `--copies` is given, and a copy whose original is not on this node (gone, or never indexed) is returned marked as a copy. Codex forks are separate sessions with `parent` set; their rows are not marked, because the format gives no per-record origin.
- Subagents: a Claude Code subagent transcript is a session with `parent` and `spawned`, as Wake lists it; the call that started it gets `agent`. A Codex spawned thread is a session with `parent` from its metadata. Each subagent's own usage sums within its own session; a parent's totals do not include its children.
- Rows no reader understands are skipped. The ones known to be skipped on purpose: hook output, queue bookkeeping, titles (which set the session's title instead), snapshots, status records.

`id` grows by row within a session as today. An anchor into a session is therefore `NODE/KEY`, `id` and the session's revision `rev` at the time of the answer; `native` is the stable identity of the record behind the row. Every answer of section 5 carries all three. A reader that comes back with an `id` and a `rev` the session has moved past is told so (`moved: true` in the answer) and can resolve by `native` instead, because a rebuilt session, or one that switched between the Wake and the native reader, numbers its rows anew.

For local rows the store also keeps the byte offset and length of the record and a hash of its bytes, which the export of section 6 checks before it reads the record back.

### Sessions

`sessions.Session` gains, all optional: `tokens` `{input, output, cache_read, cache_write}` summed over the session's calls (copies excluded), `tool_calls` and `tool_errors` counts, `files` (the distinct paths changed by calls that did not fail, at most 500), `app` (the first value the records say), `reader` (`claude`, `codex`, `acp`, `wake`), `thread` (the Codex thread a rollout belongs to), `continues` (for a Claude Code continuation, the key of the session it copied), and `gone` (decision 3 in section 8). Absence means unknown, not zero: the fields are pointers or omitted, never defaulted. Local only, never published: the native file's name.

### The store

`sessions.db` changes in these ways, with a schema number in `meta` and a one-time migration on the first start of the new build:

- `messages` gets an integer row id (it is `WITHOUT ROWID` today) so that FTS5 can use it as an external-content table, and the new columns. Local rows (node = self) are stored for native-read sessions; Wake-read sessions are still read from Wake on demand.
- `messages_fts`, FTS5 with the trigram tokenizer over `text`, so Chinese and substrings work without a tokenizer and the query is the same as Wake's. Trigram needs three characters; a shorter term is answered by a bounded `LIKE` scan within the other filters, or refused with a clear message. Verified: `modernc.org/sqlite` v1.60.1 ships FTS5 with trigram, no new dependency.
- `files (node, key, gen, id, path, base, op, ok)` with indexes on `path` and on `base` (the file name), for the question in the title; `ok` says whether the call's result had no error. 130 000 rows for the net as it is today.
- `native (file, key, size, mtime, head, tail, offset)`: the reader's cursor per file, with a hash of the first 4 KiB and of the 4 KiB before `offset`, the way lifelog's collector guards its cursor. A file that grew and whose head and tail hashes still match is read from `offset`; any other change (shrunk, head or tail changed) makes the file be read again from the start, which replaces its rows under a new generation as copies are replaced today. These files are logs the agents append to; a rewrite in the middle that keeps the size, the head and the tail is not seen by this rule, and is caught per record by the hash check at export.
- `sessions` gets the new columns, and `files` for a session is kept as a JSON list on the row as well, because that is what the feed carries.
- `copies` records, per copied session, the page format (section 4) it was fetched in.

The readers run in `SyncLocal`, once a minute: list the files, compare with `native`, parse what is new, update rows and totals, then let the feed assign revisions as it does now. A title record that arrives after the messages changes the session's entry and gets it a revision like any other change. The first run on a machine with gigabytes of files takes a minute or two and is logged; sessions appear in the feed as they are finished.

## 4. Protocol

Everything here stays within protocol 1 and is tested the way protocol 1 is tested today: the samples in `internal/node/testdata/protocol-v1` are not touched, and new samples recorded from this build are added beside them so that the next build keeps understanding these answers too. No existing field changes its meaning.

- **The page of messages** carries the new fields of section 3 on each message, and one field on the page: `format: 1`, which says the rows carry structure. All are optional; a build of today ignores them, and a page without `format` is a page of plain text rows, as from a build of today.
- **The feed entry** carries the new session fields. A reader that gets an entry without them shows the session as it shows one from Wake: no totals, no files.
- **Copies and the one-time re-publication**, in the first phase, because that is when the rows change shape. After the migration every local session gets a new revision, because its entry has gained fields; peers re-read the entries once (a few thousand small rows), not the messages. A copy records the `format` of the pages it was made from, 0 for copies made before this. When a session is read and its copy's format is below the format its node serves now (known from the last page read from that node, or tried once per reading), the copy is made again if the node is online; if not, the old copy is shown, marked as text only. Copies of nodes that stay on the old build are shown as they are.
- **A new route**, `GET /v1/sessions/search?q=&file=&op=&tool=&kind=&error=&agent=&project=&since=&until=&limit=` on the peer link, answers over the node's own local index: hits with key, `rev`, message id, `native`, time, kind, tool, a snippet and the files. The rule in `design.md` allows "a new field that is optional" within a version and says nothing about a new route. A route that a peer may not serve, where the caller does without it, is the same kind of change: the first phase that adds the route also adds that sentence to the rule in `design.md`, and a test in which a peer answers the route with 404 and the node still answers the search. Nothing in the hello changes: the route itself is the capability.
- Message pages grow by the results and thinking they now carry; the 256 MiB cap on a copy and the page budget stay as they are.

A search answers per node with one of three outcomes, and says which: **live** (the node answered the route), **copies** (the node is offline, or does not serve the route, or did not answer in time, and what this node holds of its sessions was searched instead), **none** (the same, and nothing is held). Copies of format 0 are text only: a term matches them, a file, tool or kind filter cannot, and the answer marks such a node `partial`.

Two builds of the same protocol version must keep working together, in both directions, before any of this is merged: the first phase includes a manual check of one node on the new build against one on the current build, listing and reading each other.

## 5. The query interface

### What agents actually have

Every agent in use here (Claude Code, Codex, Antigravity, the Lody sessions that run them) can run a command and read JSON. obelisk reaches its agents through a CLI and a skill only, and finds that enough. An MCP tool costs context in every prompt whether used or not, and the user's sessions already carry hundreds of tool definitions. So:

**First a command line and a skill, then MCP over the same calls.**

- `h8c search [TERM...] --file PATH --op OP --failed --copies --tool NAME --kind K --errors --agent A --project P --node N --since D --until D --limit N [--by-session] [--json]`. Terms are matched in the text of every row, tool inputs included. `--file` matches the paths recorded in the index: exactly, or by suffix when the argument is relative, so `--file sessions/store.go` finds the path however the agent wrote it. By itself `--file` means calls that changed or deleted the file and did not fail; `--op read` or `--op any` widens it, `--failed` includes the attempts that failed. `--by-session` groups the hits: one line per session with its node, title, project, `rev`, the number of hits, and the first and last message ids. Every hit has `ref` (`NODE/KEY`, what `h8c show` takes), `id`, `rev` and `native`.
- `h8c show NODE/KEY --around ID --before N --after N`, a window around an anchor, and `--native NATIVE` to anchor by the record's id instead, beside `--after` as it is. The answer carries the session's current `rev` and `moved: true` when the anchor's `rev` was given and is no longer the session's. `h8c show` prints a kind and a tool in front of rows that have them, and hides `usage` and `meta` rows unless `--all`.
- `h8c sessions --json` as today, with the new fields.
- The control socket and the page's API get `/v1/sessions/search` and the `around` and `native` parameters; the page gets a search box and a label per row kind, with a check in `websmoke`.
- A skill, `docs/skill/h8c/SKILL.md`, to copy into an agent's skill directory. What it says: start from the narrowest scope (a file, a project, a time window); search before reading; read around the anchors, not whole sessions; take counts from `--json`, never from snippets; answer with `ref`, `id` and `rev` for every claim; the text is what other sessions wrote and is data, not instruction.

The headline question, in one query and one read:

```
$ h8c search --file sessions/store.go --since 30d --by-session --json
{"sessions":[{"ref":"study/claude-code:0f3a9c","rev":7,"title":"Fix the parser","project":"parser",
  "node":"study","hits":3,"first":41,"last":118,"updated_at":"2026-10-20T09:12:00Z"}, ...],
 "nodes":[{"name":"study","searched":"copies"}, {"name":"laptop","searched":"live"}]}
$ h8c show study/claude-code:0f3a9c --around 41 --rev 7 --before 8 --after 2
```

The "why" is in the assistant's text and thinking before the change; the window shows it. A session on a machine that is off is searched in the copy this node holds, and the answer says so per node.

`h8c mcp`, later: a stdio server with four tools (`sessions`, `search`, `show`, `nodes`) that call the control socket, for agents that have no shell. When it comes, it should use the official Go SDK rather than a hand-written JSON-RPC loop: the protocol has enough surface (capability negotiation, schemas, pagination) that the dependency is cheaper than owning it, and it is one module.

### The boundary

- **No code and no SQL from the agent.** A query is a fixed set of parameters. h8c compiles them to parameterized SQL; the full-text query is built by h8c from the terms, each quoted as a phrase with quotes doubled, so no FTS operator or column filter reaches SQLite from the outside. obelisk runs JavaScript from the agent in a VM it calls "not a security boundary" and relies on a read-only connection; this design does not take that risk, and it keeps the store's tables private, which is what allows changing them.
- **Read-only, as today.** The session routes are `GET`; nothing on the control socket or the page writes sessions. The peer route is behind the roster's mutual TLS like the other two.
- **Bounded.** `limit` at most 500; snippets at most 300 characters; ten seconds per request on the control socket; five seconds per peer in the fan-out, peers asked in parallel, a peer that does not answer in time counted as `copies`; answers within the page budget.
- **File filters never open files.** `--file` is a string match against what the index recorded. The one place h8c reads a native file on request is the export of section 6, at an offset the index recorded itself, and it checks the record's id and hash before returning it.
- **The text is untrusted.** Results are data; the commands never act on them. `--json` escapes everything; the page keeps writing every value as text. The skill says the same to the agent, because a session's text can hold instructions aimed at whoever reads it later.

## 6. lifelog on h8c

### One device at a time, from the local node

Two ways to feed lifelog were weighed:

| | Per device: the collector asks its own node | Central: one node pulls the whole net |
| --- | --- | --- |
| What h8c has to do | An export route on the control socket | Mirror every session of every peer eagerly: today a node copies a session only when it is read |
| Offline machines | As today: the device pushes when it is on, cursor on the device | Only what was copied before; the rest when the machine returns, through the puller's cache |
| Completeness | The device's node reads the native files; it can return the raw record, uncut | Copies are bounded (256 MiB, text cut) and never carry the raw record |
| Device identity and dedup | Unchanged | A mapping from node ids to lifelog's device names |
| Cache growth | None | Every session of the net on the puller, for good |

**Recommendation: per device.** It keeps lifelog's own rules (cursor on the device, completeness first), needs no change to how h8c copies sessions, and is the only way the raw record reaches the archive. Central pull would need an eager mirror that h8c does not have and would make the archive depend on the copy's limits. A WSL or container that has no node today is a separate matter (roadmap item 7); the native readers make a node without Wake possible there, which helps later.

### The export

`GET /v1/sessions/export?after=CURSOR&limit=N` on the control socket, and `h8c export --after CURSOR --json`:

- Every write of a local row gets a store-wide, ever-growing position, the **cursor**, distinct from a message's `seq`. The route returns items above `CURSOR` in that order. There are two kinds of item. A **session item** is written whenever a local session's entry changes: it appears, its title arrives, its span or totals move, it is deleted or marked gone; it carries the session's fields, its native file name and, for Codex, the metadata fields the collector copies into its `session` object today. A **message item** carries the row and, for a native-read row, `raw`: the record read back from the file at the recorded offset, checked against the recorded id and hash, uncut, with the same large fields removed that the collector removes today and the content reduced to its text blocks with placeholders for images, which is the shape the collector stores now.
- A message item whose record cannot be read back (file gone, id or hash mismatch) comes without `raw` and with `partial: true`. The collector does not push a partial item; it keeps the item's cursor in a withheld list and asks for those again on its next run with `?at=C1,C2,...`. A hash mismatch also makes the next round read the file again from the start, which writes new rows at new cursors. When the file is gone for good, the collector pushes the withheld items partial, and the hub's upsert keeps a payload that has `raw` over one that has not; that is a one-line change in the hub.
- The answer carries the store's `epoch`. A collector that sees a new epoch starts from 0.
- A file read again from the start produces new rows with new cursors; the collector sends them again; the hub's upsert makes that harmless. A deletion or `gone` comes as a session item that the collector records on the session event and otherwise ignores.
- Only local rows are exported: the device's own files, which is what the device pushed before.

### Keys, so that nothing is lost or doubled

Keep the sources and the `source_id` rules lifelog has, so the switch is an enrichment of the rows the hub already holds, not a second copy of them, and no event the hub holds is replaced by one with less in it:

| lifelog field | From h8c |
| --- | --- |
| `source` | `claude_code` or `codex`, from the session's agent |
| `source_id` | **The event that exists today keeps its id.** Claude Code: the `text` row of a record is `native`, and when a record gave several text rows they are joined into that one event, as the collector joins the blocks today; every other row of the record (`thinking`, `tool_use`, `tool_result`) is `native#<block>`. Codex: the user and agent message items keep `<thread>:<turn>:<native>`; a command or MCP item is `<thread>:<turn>:<native>` for the call and `:out` for its result; a reasoning or usage row is `<thread>:<turn>:<native>` of its own record |
| `type` | `user_message`, `assistant_message` as today for text rows by role; new: `thinking`, `tool_use`, `tool_result`, `usage`, `compaction`, `system`. The collector keeps deriving its own `lody_operation_result` from the text as it does now |
| `session_id` | Claude Code: the file's name; Codex: the thread id, from the session item's `thread` |
| `app` | the row's `app`, as today per record |
| `start` | the row's time |
| `text` | the row's text; an image-only prompt is an empty text with the placeholder in the payload, as today; the collector's cleaning of user text is unchanged |
| `payload` | `raw` where there is one (the same shape as today), plus an `_h8c` object: node, key, rev, id, seq, line, kind, tool, call, error, files, usage, model, turn, agent, meta. For Codex the collector keeps building its `session` object from the session item's metadata |
| session event | `source_id` `session:<file name>` as today, one per file, from the session item's native file name; payload as today plus `_h8c` with the totals, files, parent, spawned, thread, continues and reader |
| copies | rows marked `copy` are not pushed: the event stays with the session that wrote it, instead of moving to whichever file was read last, as happens today. The continuation's session event says `continues` |

What is new for lifelog by this mapping: tool calls and results, thinking, usage, subagent and spawned-thread sessions, continuation handled instead of last-writer-wins, and the Antigravity ACP sessions that no collector sees today. What stays the same: every event it has today keeps its key, its `app`, its `session_id` and its payload shape, so a re-push changes nothing in it but the added `_h8c` object.

### Switching over

1. The hub takes the one-line change: an upsert does not replace a payload that has `raw` by one that has not.
2. Phase 2 delivers `h8c export` on a device. The collector gains a source mode that reads it.
3. On one device, run the old parser and the new mode side by side for a day, both in dry-run against the same files, and compare: the set of `source_id` values, the counts by `(source, type)`, and for every event the old parser produces, that the new mode produces one with the same `session_id`, `app`, `start`, text and payload apart from `_h8c`. Differences are either a reader bug or a documented improvement (a row the old parser skipped).
4. Switch that device; after a day, compare the hub's counts per `(device, source, type)` before and after, and spot-check rows. Then the others, one at a time. The old parser stays in the collector, off, until every device has switched; then it is removed.
5. The cursor state changes shape (one cursor, one epoch and a withheld list per device instead of per-file offsets). The first run with the new mode re-sends what the device holds; the upsert absorbs it. Because the agents keep their files for about a month, the overlap window is bounded and nothing older is touched.

## 7. Phases

Each phase is shippable alone, with every test, `go vet`, `gofmt`, the three builds, and `websmoke` where the page changed, run before it is merged, and the roadmap updated.

### Phase 1: Claude Code read natively, with structure

- The Claude Code reader as a `sessions.Source` writing into the store; the store migration; `Combined` preferring it by file path; the `Message` and `Session` fields; the page `format`, copy refresh and one-time re-publication of section 4; `h8c show` printing kinds; new protocol samples; a fixture set of made-up transcripts covering text, thinking, tool calls with an error, an edit, a write, a shell edit, a failed edit, an image-only prompt, a subagent, a continuation, a title record that arrives last, an unknown record type, and a call whose block records repeat its usage.
- Before it starts: a read-only script over the real transcripts on one machine that reports, as counts only, how many calls repeat identical usage across their block records and how many do not, so the "last record wins" rule is known to be right or is changed.
- Accepted when: `go test ./...` with `-race` is green including `protocol_test.go`; a node on this build and one on the current build list and read each other both ways on two real machines; `h8c show --json` of the fixture sessions gives the expected kinds, tools, files, errors and, per call, usage counted once with the fixture's numbers, and the session totals equal the fixture's sums; a copy made by the current build is refreshed on the next reading when its node is online and shown text-only when it is not; the demo shows a session with tool rows.

### Phase 2: Codex, ACP, the export, and lifelog switched

- The Codex reader (items first, response records for reasoning and for calls not yet completed); ACP tool calls and thinking after verifying the fields; the export with session and message items, the cursor, `?at=`, `partial` and the hash check; the collector's h8c mode in the lifelog repository and the hub's upsert change; the side-by-side comparison and the device-by-device switch of section 6; the manual check of Wake's key for a continued Codex thread.
- Accepted when: the parity comparison in section 6 passes on one device for both agents; after the switch every device's counts per `(device, source, type)` are at or above the counts before it; the new row types appear; a subagent session and a spawned Codex thread are present at the hub; a title that arrived after the messages is on the session event; a withheld item is pushed with `raw` on a later run; the old parser is removed from the collector.

### Phase 3: search across machines

- The FTS table and the `files` table over local rows and copies; `h8c search` with every filter and `--by-session`; `h8c show --around`, `--native` and `--rev`; the peer search route, the sentence in `design.md`, and the fan-out with its timeouts and the three outcomes; the page's search box and row labels with a `websmoke` check; the skill.
- Accepted when: `h8c search --file X --since 30d --by-session --json` lists the sessions with a completed change of X on every node answering live and in the copies of every other node, each with `ref`, `id`, `rev` and `native`, and the answer names each node's outcome; `--op read` and `--failed` widen it as described; `h8c show --around` shows the text before an anchor and says `moved` when the given `rev` is stale; a peer on a build without the route is answered from copies and named so; a fixture session whose text holds quotes, FTS operators and SQL comes back as text; a search of 100 terms or a term of two characters is answered within the limits or refused with a sentence; an agent, given the skill, answers "where was this file changed and why" from the two commands alone.

### Phase 4: MCP, retention, the rest

- `h8c mcp`; the `gone` flag and `h8c forget` (decision 3); export of Wake-only agents for lifelog, keyed by session and `seq`, if wanted; a shorthand such as `h8c changes PATH` if `search --file` proves clumsy.

## 8. Decisions for the user

1. **Own the readers for Claude Code and Codex, keeping Wake for the rest and as fallback** (section 2), or stay on Wake and read the native file only when a session is read. *Recommended: own them.* They are needed for lifelog's parity anyway, and only the index answers the questions across machines.
2. **Feed lifelog per device from the local node** (section 6), or have one node pull the net. *Recommended: per device.* It matches lifelog's rules, needs no eager mirror, and is the only path for the raw record. Central pull can come later if a device cannot run a node.
3. **Keep the index of a session after its file is gone.** Claude Code keeps transcripts for about a month; today Wake drops the session, h8c marks it deleted, and every peer removes its copy. That is right for a session the user deleted and wrong for one the agent cleaned up, and the two cannot be told apart from the file system: keeping means that a session the user deleted on purpose also stays, on every node, until `h8c forget NODE/KEY` removes it. *Recommended: keep it.* A local session whose file is gone is marked `gone` in the feed instead of `deleted`; peers keep what they have; `h8c sessions` shows the mark and `--gone` filters. lifelog is the archive of record either way; this is about what agents can still find.

## 9. Open points

- Which protobuf fields of ACP hold tool calls and thinking is reported, not verified in code.
- The per-file cursor does not see a rewrite in the middle of a file that keeps its size, head and tail; the per-record hash catches it at export, and the agents are not known to rewrite their logs. If one does, the cursor rule needs a periodic full check.
- A shell command that edits a file without the agent recording the edit is not in `files`. The index says what the agent recorded.
- Trigram search needs three characters; two-character Chinese terms go through the `LIKE` path, which is slower and bounded.
- Thinking is kept, cut at 32 000 characters in the index, and indexed, because the reasons a path was abandoned are often only there. If that proves too large or too private for the net, a flag can keep it local.
- Wake's key for a Codex thread continued in a second file is not known; the check is manual and the fallback by file path covers a difference.
- Whether Wake-only agents should reach lifelog at all, and under what `source`, is left to phase 4.

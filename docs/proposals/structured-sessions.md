# Proposal: structured sessions

Written 2026-11-02, revised the same day after a review, after the user asked whether Wake, not h8c, should produce the structure, and after the user's correction that no agent is to be treated as first class: the rules are the same for every agent, and the only agent-specific text is about keeping lifelog's existing rows. A design, not an implementation. It takes items 1, 2 and 6 of "What could come next" in [roadmap.md](../roadmap.md) together, because they share one piece of work, and adds a consumer outside this repository: the user's personal data store, *lifelog*, which is to take its session events from h8c instead of parsing agent files itself.

In one sentence: Wake, in a fork of ours, writes the structure its adapters already parse (tool calls, thinking, kinds; then record ids and usage wherever a source has them) into side tables of its index at indexing time; h8c reads them over the read-only path it has, keeps reading the Antigravity ACP database itself and takes that agent's structure from the transcript beside it, publishes the structure to peers as optional additions to protocol 1, answers `h8c search` across machines with anchors into sessions, and offers the local index to lifelog over the control socket, one device at a time.

Where this document describes real data or code, it was checked on 2026-11-02 on one of the user's machines, looking at structure and counts only, and against Wake's source at v0.10.2. Numbers are rounded; names and values in examples are made up.

## 1. Where things stand

- **Wake's index** stores one row per message with role, time and text, and an FTS5 trigram index over the text. A tool call is rendered into the assistant's text, so searching for a path finds it, but nothing says which rows have calls, which failed, which file was changed, or what a model call cost. Per session it has one token total and the path of the native file.
- **Wake's parsers already know more than its index keeps.** Every adapter produces the same model, `TranscriptMessage`: `seq`, role, kind (`Text`, `Meta`, `CompactSummary`), text, `tool_calls` (id, name, input preview, input, output, `is_error`, and `child_id` for a call that started a subagent), thinking, timestamp, model, images. The index is derived from that model in one place, `ParsedSession::derive`, whose comment says that a new derived field needs no adapter touched; `wake_lookups`, a table of the agent's own calls to Wake, is derived there already. The derivation flattens each call into a line of text and drops thinking, meta and compaction messages. Text is cut at 32 KiB and tool input and output at 16 KiB when parsed. `wake-cli show --tools` prints the calls, by parsing the file again on each call. The model has no record id and no usage per message; `seq` is the message's position after parsing. `wake-core`, with the `wake-cli`, `wake-mcp` and `scan` binaries, depends on no GUI crate; the repository's CI builds releases for the three platforms.
- **What each adapter keeps** is in the table of section 2. In short: every adapter in use here fills tool calls and thinking; most fill the error flag; the sources differ in whether they carry a record id and usage per call, and the adapters keep neither.
- **Antigravity ACP** has two files per session: the database h8c reads today, protobuf, which holds the full text, and since about a month a plain transcript, `brain/<id>/.system_generated/logs/transcript.jsonl`, one step per line with type, status, time, content, thinking, tool calls (name and arguments), input, output and cache-read tokens, exit code and error. On the machine checked: 219 databases, 101 transcripts, every transcript has its database; of the 118 databases without one, 111 are empty, 2 are from before the transcripts existed and 4 have two steps. The transcript cuts a step's content at about 4 KiB and says so (`truncated_fields`), on 519 of 1 974 steps sampled; the database does not. Wake's adapter parses this transcript format for three app directories but not `antigravity-acp`, matches results to calls by position, reads neither the tokens nor the `.meta` file that holds the working directory, and accepts a custom root that ends in `brain`.
- **h8c** reads Wake's `sessions` and `messages`, written against v0.9.2, which is what the nodes run; Wake is at v0.10.2 and its FTS format has moved from 16 to 19. `h8c sessions --text` matches titles and project names. Local sessions are read from the index on demand; other nodes' sessions are copied only when someone reads them.
- **lifelog** has a collector per device that pushes events to a hub. Cursor on the device; the hub deduplicates on `(device, source, source_id)` and replaces every column on a duplicate. For Claude Code it keeps one event per user or assistant record that has text (`source_id` = the record id, `session_id` = the file's name, `app` = how the agent was started as the record says it, `payload` = the record minus a few large fields, with non-text content blocks reduced to placeholders) and one session event per file; it does not keep tool calls, results, thinking, meta records, sidechains or subagent files. For Codex it keeps user and agent message items (`source_id` = thread, turn and the completed item's id; `app` = the originator) and a session event per file, and skips spawned threads. No other agent is collected. Its rules: completeness first; the cursor stays on the device; a changed `source_id` rule means deleting the source's rows and pushing everything again.
- **obelisk** does the single-machine version of this and is where several ideas below come from (ideas only; it is AGPL and none of its code is used): rows for tool calls and results with an error flag and a file path, every answer carrying ids that lead back to the original line, "structure before text" and "evidence before conclusion" as rules for the agent. Its gaps are as instructive: no file linkage for Codex, tool inputs outside the full-text index, and no second machine.

## 2. Where the structure comes from

| | W. Wake writes it, h8c reads it | A. Native readers in h8c (the first draft) | C. Wake as it is |
| --- | --- | --- | --- |
| Covers | Every agent Wake parses, with one change | The two agents someone writes a reader for | Text |
| Parsers to maintain | None in h8c; a fork of Wake until the changes are upstream | One per agent, in Go, following each agent's releases | None |
| Record ids and usage per call | A field each on the model, filled by every adapter whose source has them | Written per reader | Not available |
| Long messages | Index cut at 32 KiB; the export reads the file | Index cut by h8c's own limits; the export reads the file | Index cut at 32 KiB |
| Build and deployment | Rust; `wake-cli` built by the fork's CI for three platforms and put where the stock binary is today | Go, cross-compiled as now | As now |
| Risk | Upstream pace; a machine where the stock Wake app writes the index gets no structure | Every agent the user adopts needs a reader | None |

**Recommendation: W.** One change at the derivation point covers every agent Wake parses, the h8c reader stays the read-only SQLite path it is, and the agents' formats are followed by the project that already follows them. A does the same work once per agent and never catches up with the user's agents. The acceptance criterion that tells the two apart: after phase 1, every agent in the table below shows its tool calls in `h8c show` without a line of parsing added to h8c, Antigravity ACP excepted, whose structure h8c takes from the transcript it reads itself (below).

### Coverage by agent

What Wake's model holds today (from the source at v0.10.2), what the real index on one machine holds (sessions with the value / sessions), and what the sources have that the adapters do not keep. *has*: in the model and the index. *add*: the source has it, the adapter does not keep it, a small change in that adapter. *lacks*: the source does not have it. *verify*: not checked, no session of that agent on the machine checked.

| Agent | Tool calls | Error flag | Thinking | Model | Session tokens | Usage per call | Record id | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| claude-code | has | has | has | has, 903/911 | has, 902/911; adds every assistant record, so a call repeated over its block records may count several times (to measure) | add: usage per API message | add: the record id, read for the copied-history check but not kept | records of one API message merged into one message; copied history folded into one meta row |
| codex | has | add: exit code is in the output, the adapter sets false | has | has, 190/190 | has, 190/190 | add: a usage record per model response | add: the model record's id; the user message's completed-item id is only in another record (section 6) | messages come from the model records |
| antigravity (ACP transcript) | has, ids `step-<n>-<i>` | has: status `ERROR` or an error step; results matched to calls by position | has | has, from the user-input metadata | add: per-step tokens in the transcript; cards have none (0/14) | add: input, output, cache read per step | add: `step_index` | text in the transcript cut at about 4 KiB per step; full text only in the database; `antigravity-acp` not among the app directories; `.meta` (working directory) not read |
| dsh | has | has | has | has, 6/51 | has, 6/51 (36 of 51 sessions have no message) | add: usage per assistant message | add: message id (verify) | |
| hermes | has | verify: the adapter sets none; whether the source has a status is not checked | has | has, 597/597 | has, 586/597 | verify | add: message id (verify) | |
| grok | has | has: status `failed` | has | has, 544/544 | lacks, 0/544 (verify) | lacks (verify) | lacks: timestamps only (verify) | key falls back to `seq` |
| kimi | has | has: `isError` | has | has (verify) | verify | verify | add: call ids; message id (verify) | no session on the machine checked |
| opencode | has | has: status `error` | has | has (verify) | verify | verify | add: message id (verify) | no session on the machine checked |
| pi | has | has | has | has (verify) | has: usage per assistant message | add | add: block id (verify) | no session on the machine checked |
| gemini, kiro | lacks in the adapter (verify the source) | lacks | lacks | has (verify) | lacks (verify) | lacks (verify) | lacks (verify) | no session on the machine checked |

The rules that follow from the table, the same for every agent:

- **Structure fields** are the model's: calls with error flag and child session, thinking, kind, model. They go into the side tables for every agent at once, by the one change in `derive`.
- **Usage** goes on the message when the source has it per call; the session total is the sum of those, or the source's own total when it has only that; absence means unknown. Each adapter that has the data in front of it fills the field; the table says which, and an adapter is updated when its agent is in use, in no other order.
- **The key of a message** is `session key` plus `native_id` when the source has a record id, and `session key` plus `seq` when it does not. `seq` is the message's position after parsing, which is stable while the file only grows and the parser's view of it does not change; a continued Claude Code session whose earlier file is found later renumbers, and so would a rewrite in the middle. Wake then replaces the rows from the first difference on, under new ids, and h8c's `rev` moves, so a reader holding an old anchor is told (section 3). A record id survives all of that, which is why it is preferred where it exists.

### What the fork changes, in three steps that can be upstreamed one by one

1. **Side tables, derived in `derive`, no adapter touched.** `tool_calls (session_key, seq, idx, call_id, name, input, output, is_error, child_id)` and `message_extras (session_key, seq, kind, thinking, model)`, one row per call and one per message that has something beyond its text, keyed by the session's key and the message's `seq`. Written beside the message rows by the same per-session replacement, and read by `wake-cli show --tools` instead of parsing the file again. A key `structure_format` in `schema_meta` names the format; an upgrade flag re-parses every session once, the way an FTS format change does (46 seconds for 4 000 sessions measured on one machine). `messages` and the FTS stay untouched, so Wake's own search behaves as before.
2. **Three fields on the model, filled where the data is.** `native_id` (the id of the message's first record), `records` (every record merged into it) and `usage` (input, output, cache read, cache write) on `TranscriptMessage`, stored in `message_extras`; each adapter whose source has them fills them, per the table. Where usage repeats over the records of one call, it is counted once per call id, which also corrects the session total.
3. **Errors and files.** Adapters whose sources carry a status or exit code set `is_error` from it (Codex; hermes if its source has one). `derive` adds `file_path` and `op` to `tool_calls` from the input by tool name: the path of an edit, write or read for the file tools, the paths named in a patch, nothing for a shell command. The same rule for every agent, since the input is already in the model.

**Antigravity ACP in h8c.** h8c keeps its reader of the database, which is the only full text there is, and reads the transcript beside it for the structure: calls with name and arguments, results with status and exit code, thinking, usage per step, joined to the database's steps by step index (to be verified against both files in phase 1). The result is the same message fields as for every other agent, with `reader: acp`. A session without a transcript shows text only. Teaching Wake's adapter the `antigravity-acp` directory and the `.meta` file is a small change that lets Wake's own users see these sessions, cut at 4 KiB per step; it is offered upstream in phase 2 but h8c does not depend on it. Should Wake one day read the database itself, h8c's reader retires.

**Deployment.** `wake-cli` is already put on every machine by hand beside `h8c`; the fork's CI builds replace it the same way. The fork is based on v0.10.x, so the h8c reader is updated for that version's index at the same time, which it needs anyway. While the Wake desktop app runs, `wake-cli` leaves the index to it; on a machine where the stock app runs, the index therefore has no side tables, and the app must be built from the fork too or not run while structure is wanted. As far as the records say, only `wake-cli` is installed on the user's machines.

**Fallback.** `wake.Index` already checks the tables and columns it needs before reading. The side tables and `structure_format` become a second, optional set: absent, or of a format h8c does not know, means text only, as today, and the node says so once.

**Why not `wake-cli show --tools` or an export command instead of tables.** Both parse the file again on every call (`show` does today), need a cursor of their own, and hand h8c text to parse. The tables are filled once per change by the refresh Wake already runs, cost nothing to read, and serve Wake's own tools too.

## 3. The model

### Messages

One h8c message stays one Wake message, as today: the row that `h8c show` prints and the unit that pages and copies are made of. It keeps its fields with their meaning and gains these, all optional, nested where Wake nests them:

| Field | Meaning |
| --- | --- |
| `kind` | `text`, `meta`, `compact`; empty for a row from an index without side tables |
| `thinking` | the thinking that went with the message, as Wake keeps it, cut at 32 KiB when parsed |
| `calls` | `[{call, tool, input, output, error, files, agent}]`: the calls the message made, in order; `call` is the agent's id for it, `input` and `output` are cut at 16 KiB when parsed, `error` is the agent's flag or an exit code other than zero, `files` is `[{path, op}]` with `op` one of `change`, `delete`, `read`, and `agent` the key of the subagent's session when the call started one |
| `usage` | `{input, output, cache_read, cache_write}` tokens of the model call behind the message, once per call; cache tokens kept apart from input, never added into it |
| `model` | the model of that call, when it differs from the session's |
| `native` | the id of the message's first record in the agent's file, where the source has one: the stable anchor, and lifelog's `source_id` |
| `records` | the ids of every record Wake merged into the message, when more than one |

What is not there, and why: no row for a tool result (it sits in its call), no row for thinking (it sits in its message), no `copy` flag (Wake folds a continued session's copied history into one `meta` row and leaves it out of the totals), no `app` per message (the export takes it from the raw record), no line numbers or offsets (the export finds a record by `native`).

`id` grows by row within a session as today. An anchor is `NODE/KEY`, `id` and the session's revision `rev` at the time of the answer, plus the index of a call when the hit is a call; `native` is the stable identity behind the row where there is one, `seq` otherwise. Every answer of section 5 carries them. A reader that comes back with an `id` and a `rev` the session has moved past is told so (`moved: true`) and can resolve by `native` where it exists, because a session parsed again numbers its rows anew.

### Sessions

`sessions.Session` gains, all optional: `tokens` `{input, output, cache_read, cache_write}` summed over the session's calls where the adapter carries usage, else the source's one total as `tokens.total`; `tool_calls` and `tool_errors` counts; `files` (the distinct paths changed by calls without error, at most 500); `app` (Wake's `source` column: how the agent was started); `reader` (`wake`, `acp`); and `gone` (decision 3 in section 8). Absence means unknown, not zero: the fields are pointers or omitted, never defaulted.

### The store

h8c keeps no index of local messages: local sessions are read from Wake, as today, now with three more tables, and ACP sessions from their two files. `sessions.db` changes in these ways, with a schema number in `meta` and a one-time migration on the first start of the new build:

- `messages` gets an integer row id (it is `WITHOUT ROWID` today) so that FTS5 can use it as an external-content table, and columns for the new fields of copied rows: `kind`, `thinking`, `calls` as JSON, the usage numbers, `model`, `native`, `records`.
- `messages_fts`, FTS5 with the trigram tokenizer over the text of copied rows, so Chinese and substrings work and the query is the same as Wake's. Trigram needs three characters; a shorter term goes through a bounded `LIKE` scan within the other filters, which is also Wake's own rule. Verified: `modernc.org/sqlite` v1.60.1 ships FTS5 with trigram, no new dependency.
- `files (node, key, gen, id, call, path, base, op, ok)` for copied rows, with indexes on `path` and `base`. For local rows the same question goes to Wake's `tool_calls` by its `file_path` index, and to the ACP reader's own small table of the same shape.
- `copies` records, per copied session, the page format (section 4) it was fetched in.
- `export_log` (section 6): one small row per local message that was handed to lifelog.

Local search uses Wake's `messages_fts` only when `schema_meta.fts_format` is the value h8c was built against, as Wake's own read-side tools do, and `LIKE` within the other filters otherwise.

## 4. Protocol

Everything here stays within protocol 1 and is tested the way protocol 1 is tested today: the samples in `internal/node/testdata/protocol-v1` are not touched, and new samples recorded from this build are added beside them so that the next build keeps understanding these answers too. No existing field changes its meaning.

- **The page of messages** carries the new fields of section 3 on each message, and one field on the page: `format: 1`, which says the rows carry structure. All are optional; a build of today ignores them, and a page without `format` is a page of plain text rows, as from a build of today or from a node whose index has no side tables.
- **The feed entry** carries the new session fields. A reader that gets an entry without them shows the session as it shows one today: no totals, no files.
- **Copies and the one-time re-publication**, in the first phase, because that is when the rows change shape. After the migration every local session gets a new revision, because its entry has gained fields; peers re-read the entries once (a few thousand small rows), not the messages. A copy records the `format` of the pages it was made from, 0 for copies made before this. When a session is read and its copy's format is below what its node serves now (known from the last page read from that node, or tried once per reading), the copy is made again if the node is online; if not, the old copy is shown, marked as text only. Copies of nodes that stay on the old build, or whose index has no side tables, are shown as they are.
- **A new route**, `GET /v1/sessions/search?q=&file=&op=&tool=&kind=&error=&agent=&project=&since=&until=&limit=` on the peer link, answers over the node's own local index: hits with key, `rev`, message id, call index, `native`, time, kind, tool, a snippet and the files. The rule in `design.md` allows "a new field that is optional" within a version and says nothing about a new route. A route that a peer may not serve, where the caller does without it, is the same kind of change: the phase that adds the route also adds that sentence to the rule in `design.md`, and a test in which a peer answers the route with 404 and the node still answers the search. Nothing in the hello changes: the route itself is the capability.
- Message pages grow by the calls and thinking they now carry; the 256 MiB cap on a copy and the page budget stay as they are.

A search answers per node with one of three outcomes, and says which: **live** (the node answered the route), **copies** (the node is offline, or does not serve the route, or did not answer in time, and what this node holds of its sessions was searched instead), **none** (the same, and nothing is held). Copies of format 0 are text only: a term matches them, a file, tool or kind filter cannot, and the answer marks such a node `partial`.

Two builds of the same protocol version must keep working together, in both directions, before any of this is merged: the first phase includes a manual check of one node on the new build against one on the current build, listing and reading each other.

## 5. The query interface

### What agents actually have

Every agent in use here (Claude Code, Codex, Antigravity, the Lody sessions that run them) can run a command and read JSON. obelisk reaches its agents through a CLI and a skill only, and finds that enough. An MCP tool costs context in every prompt whether used or not, and the user's sessions already carry hundreds of tool definitions. So:

**First a command line and a skill, then MCP over the same calls.**

- `h8c search [TERM...] --file PATH --op OP --failed --tool NAME --kind K --errors --agent A --project P --node N --since D --until D --limit N [--by-session] [--json]`. Terms are matched in the text of every row, tool inputs included, because Wake's rows carry the call lines. `--file` matches the paths recorded in `tool_calls` and `files`: exactly, or by suffix when the argument is relative, so `--file sessions/store.go` finds the path however the agent wrote it. By itself `--file` means calls that changed or deleted the file and have an output without error; `--op read` or `--op any` widens it, `--failed` includes the attempts that failed. `--by-session` groups the hits: one line per session with its node, title, project, `rev`, the number of hits, and the first and last message ids. Every hit has `ref` (`NODE/KEY`, what `h8c show` takes), `id`, `rev`, `native` where there is one and, for a call, `call`.
- `h8c show NODE/KEY --around ID --before N --after N`, a window around an anchor, and `--native NATIVE` to anchor by the record's id instead, beside `--after` as it is. The answer carries the session's current `rev` and `moved: true` when the anchor's `rev` was given and is no longer the session's. `h8c show` prints a line per call under its message, with the tool and whether it failed, and hides `meta` rows unless `--all`.
- `h8c sessions --json` as today, with the new fields.
- The control socket and the page's API get `/v1/sessions/search` and the `around` and `native` parameters; the page gets a search box and the calls under a message, with a check in `websmoke`.
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

- **No code and no SQL from the agent.** A query is a fixed set of parameters. h8c compiles them to parameterized SQL; the full-text query is built by h8c from the terms, each quoted as a phrase with quotes doubled, so no FTS operator or column filter reaches SQLite from the outside, Wake's or h8c's. obelisk runs JavaScript from the agent in a VM it calls "not a security boundary" and relies on a read-only connection; this design does not take that risk, and it keeps both stores' tables private, which is what allows changing them.
- **Read-only, as today.** The session routes are `GET`; nothing on the control socket or the page writes sessions; Wake's index is opened read-only as it is now. The peer route is behind the roster's mutual TLS like the other two.
- **Bounded.** `limit` at most 500; snippets at most 300 characters; ten seconds per request on the control socket; five seconds per peer in the fan-out, peers asked in parallel, a peer that does not answer in time counted as `copies`; answers within the page budget.
- **File filters never open files.** `--file` is a string match against what the indexes recorded. The one place h8c reads a native file on request is the export of section 6, which looks for a record id the index gave it.
- **The text is untrusted.** Results are data; the commands never act on them. `--json` escapes everything; the page keeps writing every value as text. The skill says the same to the agent, because a session's text can hold instructions aimed at whoever reads it later.

## 6. lifelog on h8c

### One device at a time, from the local node

Two ways to feed lifelog were weighed:

| | Per device: the collector asks its own node | Central: one node pulls the whole net |
| --- | --- | --- |
| What h8c has to do | An export route on the control socket | Mirror every session of every peer eagerly: today a node copies a session only when it is read |
| Offline machines | As today: the device pushes when it is on, cursor on the device | Only what was copied before; the rest when the machine returns, through the puller's cache |
| Completeness | The device's node has the native files; it can return the raw record, uncut | Copies are bounded (256 MiB, text cut) and never carry the raw record |
| Device identity and dedup | Unchanged | A mapping from node ids to lifelog's device names |
| Cache growth | None | Every session of the net on the puller, for good |

**Recommendation: per device.** It keeps lifelog's own rules (cursor on the device, completeness first), needs no change to how h8c copies sessions, and is the only way the raw record reaches the archive. Central pull would need an eager mirror that h8c does not have and would make the archive depend on the copy's limits.

### The export

`GET /v1/sessions/export?after=CURSOR&limit=N` on the control socket, and `h8c export --after CURSOR --json`:

- Wake's message ids are not a cursor: a session parsed again gets new rows whose ids can reuse freed ones. So after each local round, for every session whose revision moved, h8c compares the index's rows with `export_log` (session, index row id, `native` or `seq`, a hash of the row with its extras) and appends a row with the next **cursor** for each message that is new or changed. The route returns items above `CURSOR` in that order. There are two kinds of item. A **session item** is written whenever a local session's entry changes: it appears, its title arrives, its span or totals move, it is deleted or marked gone; it carries the session's fields and its native file name. A **message item** carries the row with its extras and `raw`: the record, or the records in `records`, found in the native file by their ids, uncut, with the same large fields removed that the collector removes today and the content reduced to its text blocks with placeholders for images, which is the shape the collector stores now. For a source without record ids `raw` is the line at the message's position, if the file has lines. The file is read once per session in a batch, not once per record.
- A message item whose records cannot be found (file gone, id not in it) comes without `raw` and with `partial: true`. The collector does not push a partial item; it keeps the item's cursor in a withheld list and asks for those again on its next run with `?at=C1,C2,...`. When the file is gone for good, the collector pushes the withheld items partial, and the hub's upsert keeps a payload that has `raw` over one that has not; that is a one-line change in the hub.
- The answer carries the store's `epoch`. A collector that sees a new epoch starts from 0.
- A session parsed again produces changed rows with new cursors; the collector sends them again; the hub's upsert makes that harmless. A deletion or `gone` comes as a session item that the collector records on the session event and otherwise ignores.
- Only local rows are exported: the device's own files, which is what the device pushed before. Every agent the node has is exported; which ones the collector pushes is its configuration.

### Keys: the general rule, and the one special case

The general rule, for every agent: `source` is the agent id, `source_id` is the message's key of section 2 (`native` where the source has a record id, else `seq`), a call is `<message key>#<idx>`, thinking `<message key>#thinking`, the session event `session:<native session id>`, `payload` is `raw` where there is one plus an `_h8c` object (node, key, rev, id, seq, kind, calls, usage, model), `app` is Wake's `source` for the session unless the raw record has its own.

The special case is migration, not priority: lifelog already holds rows for `claude_code` and `codex` under the rules of its old collector, and those rows must keep their ids and their payload shape, so the switch enriches them instead of doubling them:

| lifelog field | claude_code and codex, as before |
| --- | --- |
| `source_id` | the record id for the text event, as today; this is why those two adapters get `native_id` first. Codex: `<thread>:<turn>:<id>`, where the user message's id is the completed item's, which is only in another record; the adapter pairs it with the user message of the same turn by order, and phase 2 counts how often that holds |
| `type` | `user_message`, `assistant_message` as today; new: `thinking`, `tool_call`, `compaction`. The collector keeps deriving its own `lody_operation_result` from the text |
| `session_id` | Claude Code: the file's name; Codex: the thread id, both from the session item |
| `app` | from the raw record, as today per record |
| `text` | the message's text; an image-only prompt is an empty text with the placeholder in the payload, as today; the collector's cleaning of user text is unchanged |
| `payload` | `raw` of the first record (the same shape as today), the other merged records under `_records`, plus `_h8c` |
| session event | `source_id` `session:<file name>` as today, one per file; payload as today plus `_h8c` with the totals, files, parent, spawned and reader |
| copied history | not pushed: Wake does not index it, so the event stays with the session that wrote it, instead of moving to whichever file was read last, as happens today |

What differs by this mapping, and is accepted: an API message that Claude Code wrote as two text records was two events and is one, with both records in the payload; the parity check counts after merging. What is new for lifelog: tool calls with their results, thinking, usage, subagent and spawned-thread sessions, every other agent Wake parses, and the Antigravity ACP sessions that no collector sees today. What stays the same: every event it has today keeps its key, its `app`, its `session_id` and its payload shape, so a re-push changes nothing in it but the added objects.

### Switching over

1. The hub takes the one-line change: an upsert does not replace a payload that has `raw` by one that has not.
2. Phase 2 delivers `h8c export` on a device. The collector gains a source mode that reads it.
3. On one device, run the old parser and the new mode side by side for a day, both in dry-run against the same files, and compare: the set of `source_id` values, the counts by `(source, type)`, and for every event the old parser produces, that the new mode produces one with the same `session_id`, `app`, `start`, text and payload apart from the added objects. Differences are either a reader bug, the merge above, or a documented improvement.
4. Switch that device; after a day, compare the hub's counts per `(device, source, type)` before and after, and spot-check rows. Then the others, one at a time. The old parser stays in the collector, off, until every device has switched; then it is removed. Other agents are switched on in the collector's configuration afterwards, with no migration, since the hub has no rows for them.
5. The cursor state changes shape (one cursor, one epoch and a withheld list per device instead of per-file offsets). The first run with the new mode re-sends what the device holds; the upsert absorbs it. Because the agents keep their files for about a month, the overlap window is bounded and nothing older is touched.

## 7. Phases

Each phase is shippable alone, with every test, `go vet`, `gofmt`, the three builds, and `websmoke` where the page changed, run before it is merged; the fork's own tests for its part; and the roadmap updated.

### Phase 1: the side tables, read by h8c, and the ACP transcript

- In the fork: step 1 of section 2, the side tables derived in `derive`, `structure_format`, the one-time backfill, and `show --tools` reading them. In h8c: the reader updated for the v0.10 index with the side tables as an optional set; the ACP reader reading the transcript beside the database for calls, results, thinking and usage per step; the `Message` and `Session` fields; the page `format`, copy refresh and one-time re-publication of section 4; `h8c show` printing calls and kinds; new protocol samples; `waketest` writing the side tables and `acptest` writing a transcript, so the fixtures cover text, thinking, calls with an error, a call that started a subagent, a meta row from a continued session, an ACP session with and without a transcript, and an index without the tables.
- What stays out, to keep it small: record ids, usage per call and the error flags that need an adapter change (steps 2 and 3), the export, search. ACP's usage comes in anyway because it is in the transcript h8c reads. If the phase must be smaller still, the transcript reading is the part to move to phase 2; it is in phase 1 because the user names that agent among the main ones.
- Accepted when: the fork's tests are green and `wake-cli index` on one machine fills `tool_calls` for every agent of the table that has sessions there (counts, no content), with no parsing added to h8c; `go test ./...` with `-race` is green including `protocol_test.go`; a node on this build and one on the current build list and read each other both ways on two real machines; `h8c show --json` of the fixture sessions gives the expected kinds, calls, errors and subagent keys; an ACP session shows its calls, errors, thinking and per-step usage from the transcript with the database's full text, and one without a transcript shows text only; a node whose `wake-cli` is the stock build lists and shows text as today and logs once that the index has no structure; a copy made by the current build is refreshed on the next reading when its node is online and shown text-only when it is not; the demo shows a session with calls.

### Phase 2: ids, usage, errors, files, the export, and lifelog switched

- In the fork: steps 2 and 3 of section 2, for every adapter the table marks *add*, and `antigravity-acp` with the `.meta` file in the Antigravity adapter. Before them, a read-only script over the real files on one machine reports, as counts only: how many Claude Code calls repeat identical usage across their block records and how many do not; how many Codex user messages pair with a completed item by order within their turn; and for ACP whether the transcript's step index matches the database's. In h8c: `export_log`, the export with session and message items, `?at=`, `partial` and `raw` by record id or position; `tokens` and `files` on the session. Outside: the collector's h8c mode and the hub's upsert change; the side-by-side comparison and the device-by-device switch of section 6.
- Accepted when: the fixture with a call repeated over three block records yields one usage and session totals equal to the fixture's sums; the parity comparison in section 6 passes on one device for both migrated sources; after the switch every device's counts per `(device, source, type)` are at or above the counts before it; the new event types appear; a subagent session and a spawned Codex thread are present at the hub; a title that arrived after the messages is on the session event; a withheld item is pushed with `raw` on a later run; the old parser is removed from the collector; one agent that was never collected before appears at the hub by configuration alone.

### Phase 3: search across machines

- The FTS table and the `files` table over copies; the local search over Wake's tables with the FTS format check and over the ACP reader's; `h8c search` with every filter and `--by-session`; `h8c show --around`, `--native` and `--rev`; the peer search route, the sentence in `design.md`, and the fan-out with its timeouts and the three outcomes; the page's search box and the calls under a message with a `websmoke` check; the skill.
- Accepted when: `h8c search --file X --since 30d --by-session --json` lists the sessions with a completed change of X on every node answering live and in the copies of every other node, each with `ref`, `id`, `rev` and `native` where there is one, and the answer names each node's outcome; `--op read` and `--failed` widen it as described; `h8c show --around` shows the text before an anchor and says `moved` when the given `rev` is stale; a peer on a build without the route is answered from copies and named so; a fixture session whose text holds quotes, FTS operators and SQL comes back as text; a search of 100 terms or a term of two characters is answered within the limits or refused with a sentence; an agent, given the skill, answers "where was this file changed and why" from the two commands alone.

### Phase 4: MCP, retention, upstream

- `h8c mcp`; the `gone` flag and `h8c forget` (decision 3); the fork's changes offered upstream as pull requests in the order of section 2, and the fork retired when they land; if Wake comes to read the ACP database itself, h8c's reader retired too.

## 8. Decisions for the user

1. **Wake produces the structure, in a fork of ours first, offered upstream after it runs on every machine** (section 2), or the fork only with no pull request, or h8c's own readers as in the first draft. *Recommended: the fork, then upstream.* Opening a pull request on the Wake repository is an action in public and is not taken without the user's word; until the changes land, the fork follows Wake's releases, which is the cost of this direction.
2. **Feed lifelog per device from the local node** (section 6), or have one node pull the net. *Recommended: per device.* It matches lifelog's rules, needs no eager mirror, and is the only path for the raw record.
3. **Keep the index of a session after its file is gone.** Claude Code keeps transcripts for about a month; today Wake drops the session, h8c marks it deleted, and every peer removes its copy. That is right for a session the user deleted and wrong for one the agent cleaned up, and the two cannot be told apart from the file system: keeping means that a session the user deleted on purpose also stays, on every node, until `h8c forget NODE/KEY` removes it. *Recommended: keep it.* A local session whose file is gone is marked `gone` in the feed instead of `deleted`; peers keep what they have; `h8c sessions` shows the mark and `--gone` filters. lifelog is the archive of record either way; this is about what agents can still find.

## 9. Open points

- The table's *verify* cells: hermes's error flag, grok's tokens and ids, and everything about kimi, opencode, pi, gemini and kiro, which have no session on the machine checked. They are filled in when an adapter is touched, from its source.
- ACP: whether the transcript's `step_index` numbers the same steps as the database; Wake's adapter matches results to calls by position, and h8c's reader should do better if the transcript allows it. Sessions from before the transcripts (2 of substance on one machine) and the four two-step ones have text only.
- Codex user messages in lifelog: the completed item's id is not on the record Wake reads; pairing by order within a turn is the plan, and the count in phase 2 says how often it holds. Where it does not, the user event gets the model record's id and the old row stays beside it for that session.
- A Codex thread continued in a second file: Wake keeps one file per key and the collector made one session event per file. None exists on the machine checked.
- Claude Code's usage: whether a call's usage is repeated on each of its block records is measured in phase 2; the adapter counts once per call id either way, and Wake's session total may change by the same fix.
- Messages that Wake does not index (meta, compaction, thinking without text) have no row; their thinking is not reachable. If that matters, the fork can index them as empty rows, which is a change to Wake's own search results and is left out of the first phases.
- Sessions whose files the agents have already cleaned up are not in Wake's index and get no structure; their text lives in h8c copies and in lifelog.
- Wake's cuts at parse time, 32 KiB of text and 16 KiB per tool input or output, bound the index and what travels between nodes; the export reads the file, so lifelog is not bound by them. The ACP transcript's own cut at about 4 KiB per step bounds only what the transcript contributes; the text comes from the database.
- Trigram search needs three characters; two-character Chinese terms go through the `LIKE` path, which is slower and bounded.
- Thinking is kept and indexed because the reasons a path was abandoned are often only there. If that proves too large or too private for the net, a flag can keep it local.

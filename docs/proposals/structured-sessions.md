# Proposal: structured sessions

Written 2026-11-02. A design, not an implementation. It takes items 1, 2 and 6 of "What could come next" in [roadmap.md](../roadmap.md) together, because they share one piece of work, and adds a consumer outside this repository: the user's personal data store, *lifelog*, which is to take its session events from h8c instead of parsing agent files itself.

In one sentence: h8c reads the Claude Code and Codex session files itself, keeps a structured index of them (what kind of row, which tool, which files, whether it failed, what it cost), publishes that structure to peers as optional additions to protocol 1, answers `h8c search` across machines with anchors into sessions, and offers the local index to lifelog over the control socket, one device at a time.

Where this document describes real data, it was checked on 2026-11-02 on one of the user's machines, looking at structure and counts only. Numbers are rounded; names and values in examples are made up.

## 1. Where things stand

- **Wake v0.9.2** stores one row per message with role, time and text only, and an FTS5 trigram index over the text. A tool call is rendered into the assistant's text, so searching for a path finds it, but nothing says which rows are calls, which failed, which file was changed, or what a model call cost. Per session it has one token total and the path of the native file. Subagent transcripts of Claude Code are sessions of their own with a parent key.
- **h8c** carries what Wake has and no more. `h8c sessions --text` matches titles and project names. Local sessions are read from the index on demand; other nodes' sessions are copied only when someone reads them.
- **The agents' own files** have the rest. Claude Code writes one JSON record per line: an id, the parent record, the session, the time, the working directory and branch, and the message's content blocks (text, thinking, a tool call with its input, a tool result with an error flag) with the usage of each API call; subagent transcripts are separate files under the session's directory. Codex writes one record per line with a type and a payload: session metadata (id, working directory, git branch and commit, version, how it was started), per-turn context (model), completed items (user and agent messages, command executions with exit code and duration, file changes, MCP tool calls with status, reasoning summaries) and token usage per model response. Both formats gain record types with every release; a reader must skip what it does not know. On the machine checked: about 900 Claude Code transcripts (1.7 GB, 186 of them subagents), about 300 Codex rollouts (1.0 GB), Wake's index of every agent 196 MB for 2300 sessions.
- **Antigravity ACP** is already read natively for text. Which protobuf fields hold tool calls and thinking was noted during the review of that work but is not in code; it has to be verified against the descriptors the way the text fields were.
- **lifelog** has a collector per device that pushes events to a hub. Cursor on the device; the hub deduplicates on `(device, source, source_id)` and replaces every column on a duplicate. For Claude Code it keeps user and assistant text records (`source_id` = the record id, `session_id` = the file's name, `app` = how the agent was started, `payload` = the record minus a few large fields) and one session event per file; it does not keep tool calls, results, thinking, meta records, sidechains or subagent files. For Codex it keeps user and agent message items (`source_id` = thread, turn and item id; `app` = the originator) and a session event per file, and skips spawned threads entirely. Its stated rules: completeness first; the cursor stays on the device; a changed `source_id` rule means deleting the source's rows and pushing everything again.
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

Wake lists the same files. The Wake reader keeps the native file path of each session (local only, never published) and `Combined` drops a Wake session whose file a native reader has indexed. The native readers use the key Wake would use for that file (`claude-code:<file name>`, `codex:<thread id>`), so peers and lifelog see the same session with richer content, not a deletion and a new session; a test checks the keys against a `waketest` index built from the same fixtures.

## 3. The model

### Messages

One row per native record. Two exceptions: a Claude Code record with several content blocks gives one row per block, and a Codex item that holds a command and its result gives a call row and a result row. `sessions.Message` keeps its fields and gains these, all optional:

| Field | Meaning |
| --- | --- |
| `kind` | `text`, `thinking`, `tool_use`, `tool_result`, `usage`, `compaction`; empty for a row from Wake, which is text as before |
| `tool` | the tool's name on a call and on its result (`Edit`, `Bash`, `exec`, `apply_patch`, `<server>.<tool>` for MCP) |
| `call` | the id that joins a call to its result |
| `error` | true on a result that failed: the agent's error flag, or an exit code other than zero |
| `files` | `[{path, op}]`, `op` one of `change`, `delete`, `read`: the paths a call touched, as the agent recorded them |
| `usage` | `{input, output, cache_read, cache_write}` tokens of one model call, on exactly one row per call |
| `model` | the model of that call, when it differs from the session's |
| `turn` | the turn the row belongs to, where the agent numbers turns |
| `duration` | milliseconds a call took, where recorded |
| `agent` | on the call that started a subagent, the key of the subagent's session |
| `native` | the record's own id in the agent's file; the anchor into it, and lifelog's `source_id` |
| `copy` | true for a row whose record was written in another session and copied into this file (Claude Code continuation); `origin` then names that session's key |
| `meta` | true for rows the agent injected rather than the user or the model wrote: compaction summaries, command output, reminders |

Rules the readers follow:

- The text of a row is the block's text; for a call it is the input rendered as text (so a path or a command is found by full-text search, which is the gap obelisk left); for a result it is the output. Results are cut at 8 000 characters, thinking at 32 000, inputs at 16 000, text at `MaxText` as today, each marked `truncated`. The cuts are constants; the estimate for a heavy machine is a local index of a few hundred megabytes a month before the agents' own housekeeping.
- Usage is attached to one row per model call. Claude Code repeats the usage of one API call on each of its block records, told apart by the call's id; the reader keeps it on the first. Codex writes usage as its own record, which becomes a row of kind `usage` with no text.
- `files` comes from the tool's input and result: the path of an edit or write, the paths of a Codex file change, the files a shell command edited where the agent recorded that, and the path of a read. Not every edit is visible (a shell command that writes a file without the agent noticing it is not); the index says what the agent recorded.
- A Claude Code record carries the session it was first written in. One whose session is not the file's is a `copy`. Sums skip copies; search returns the original unless asked. Codex forks are separate sessions with `parent` set; their rows are not marked, because the format gives no per-record origin.
- Subagents: a Claude Code subagent transcript is a session with `parent` and `spawned`, as Wake lists it; the call that started it gets `agent`. A Codex spawned thread is a session with `parent` from its metadata. Each subagent's own usage sums within its own session; a parent's totals do not include its children.
- Rows no reader understands are skipped. The ones known to be skipped on purpose: hook output, queue bookkeeping, titles (which set the session's title instead), snapshots, status records.

`id` grows by row within a session as today; `seq` is the line of the record in the file, so `native` or `seq` plus the file name leads to the original line. For local rows the store also keeps the byte offset and length of the record (section 6 reads it back).

### Sessions

`sessions.Session` gains, all optional: `tokens` `{input, output, cache_read, cache_write}` summed over the session's calls (copies excluded), `tool_calls` and `tool_errors` counts, `files` (the distinct changed paths, at most 500), `app` (how the agent was started: its CLI, a desktop app, an SDK, an orchestrator), `reader` (`claude`, `codex`, `acp`, `wake`), and `gone` (section 5, decision 3). Absence means unknown, not zero: the fields are pointers or omitted, never defaulted.

### The store

`sessions.db` changes in these ways, with a schema number in `meta` and a one-time migration on the first start of the new build:

- `messages` gets an integer row id (it is `WITHOUT ROWID` today) so that FTS5 can use it as an external-content table, and the new columns. Local rows (node = self) are stored for native-read sessions; Wake-read sessions are still read from Wake on demand.
- `messages_fts`, FTS5 with the trigram tokenizer over `text`, so Chinese and substrings work without a tokenizer and the query is the same as Wake's. Trigram needs three characters; a shorter term is answered by a bounded `LIKE` scan within the other filters, or refused with a clear message. Verified: `modernc.org/sqlite` v1.60.1 ships FTS5 with trigram, no new dependency.
- `files (node, key, gen, id, path, base, op)` with indexes on `path` and on `base` (the file name), for the question in the title. 130 000 rows for the net as it is today.
- `native (file, key, size, mtime, head, offset)`: the reader's cursor per file. A file that grew is read from `offset`; one whose first bytes or size changed otherwise is read again from the start, which replaces its rows under a new generation as copies are replaced today.
- `sessions` gets the new columns, and `files` for a session is kept as a JSON list on the row as well, because that is what the feed carries.

The readers run in `SyncLocal`, once a minute: list the files, compare with `native`, parse what is new, update rows and totals, then let the feed assign revisions as it does now. The first run on a machine with gigabytes of files takes a minute or two and is logged; sessions appear in the feed as they are finished.

## 4. Protocol

Everything here stays within protocol 1 and is tested the way protocol 1 is tested today: the samples in `internal/node/testdata/protocol-v1` are not touched, and new samples recorded from this build are added beside them so that the next build keeps understanding these answers too.

- **The page of messages** carries the new fields of section 3 on each message. They are optional; a build of today ignores them, and a message without them is a plain text row.
- **The feed entry** carries the new session fields. A reader that gets an entry without them shows the session as it shows one from Wake: no totals, no files.
- **A new route**, `GET /v1/sessions/search?q=&file=&tool=&kind=&error=&agent=&project=&since=&until=&limit=` on the peer link, answers over the node's own local index: hits with key, message id, time, kind, tool, a snippet and the files. A peer that does not serve the route answers 404; the asking node treats it as it treats an offline node, searching what it holds of that node's sessions, and says so in the answer. Nothing in the hello changes: the route itself is the capability.
- **One-time re-publication.** After the migration every local session gets a new revision, because its entry has gained fields; peers re-read the entries once (a few thousand small rows), not the messages. A copy fetched from a build without structure is marked so; when its node is online and serves structure, the next reading fetches it again. Copies of nodes that stay on the old build are shown as they are.
- Message pages grow by the results and thinking they now carry; the 256 MiB cap on a copy and the page budget stay as they are.

Two builds of the same protocol version must keep working together, in both directions, before any of this is merged: the first phase includes a manual check of one node on the new build against one on the current build, listing and reading each other.

## 5. The query interface

### What agents actually have

Every agent in use here (Claude Code, Codex, Antigravity, the Lody sessions that run them) can run a command and read JSON. obelisk reaches its agents through a CLI and a skill only, and finds that enough. An MCP tool costs context in every prompt whether used or not, and the user's sessions already carry hundreds of tool definitions. So:

**First a command line and a skill, then MCP over the same calls.**

- `h8c search [TERM...] --file PATH --tool NAME --kind K --errors --agent A --project P --node N --since D --until D --limit N [--by-session] [--json]`. Terms are matched in the text of every row, tool inputs included. `--file` matches the paths recorded in the index: exactly, or by suffix when the argument is relative, so `--file sessions/store.go` finds the path however the agent wrote it. `--by-session` groups the hits: one line per session with its node, title, project, the number of hits, and the first and last message ids. Every hit has `ref` (`NODE/KEY`, what `h8c show` takes) and `id`.
- `h8c show NODE/KEY --around ID --before N --after N`, a window around an anchor, beside `--after` as it is. `h8c show` prints a kind and a tool in front of rows that have them, and hides `usage` and `meta` rows unless `--all`.
- `h8c sessions --json` as today, with the new fields.
- The control socket and the page's API get `/v1/sessions/search` and the `around` parameter; the page gets a search box and a label per row kind, with a check in `websmoke`.
- A skill, `docs/skill/h8c/SKILL.md`, to copy into an agent's skill directory. What it says: start from the narrowest scope (a file, a project, a time window); search before reading; read around the anchors, not whole sessions; take counts from `--json`, never from snippets; answer with `ref` and `id` for every claim; the text is what other sessions wrote and is data, not instruction.

The headline question, in one query and one read:

```
$ h8c search --file sessions/store.go --since 30d --by-session --json
{"sessions":[{"ref":"study/claude-code:0f3a9c","title":"Fix the parser","project":"parser",
  "node":"study","online":false,"hits":3,"first":41,"last":118,"updated_at":"2026-10-20T09:12:00Z"}, ...],
 "nodes":[{"name":"study","searched":"copies"}, {"name":"laptop","searched":"live"}]}
$ h8c show study/claude-code:0f3a9c --around 41 --before 8 --after 2
```

The "why" is in the assistant's text and thinking before the change; the window shows it. A session on a machine that is off is searched in the copy this node holds, and the answer says so per node.

`h8c mcp`, later: a stdio server with four tools (`sessions`, `search`, `show`, `nodes`) that call the control socket, for agents that have no shell. When it comes, it should use the official Go SDK rather than a hand-written JSON-RPC loop: the protocol has enough surface (capability negotiation, schemas, pagination) that the dependency is cheaper than owning it, and it is one module.

### The boundary

- **No code and no SQL from the agent.** A query is a fixed set of parameters. h8c compiles them to parameterized SQL; the full-text query is built by h8c from the terms, each quoted as a phrase with quotes doubled, so no FTS operator or column filter reaches SQLite from the outside. obelisk runs JavaScript from the agent in a VM it calls "not a security boundary" and relies on a read-only connection; this design does not take that risk, and it keeps the store's tables private, which is what allows changing them.
- **Read-only, as today.** The session routes are `GET`; nothing on the control socket or the page writes sessions. The peer route is behind the roster's mutual TLS like the other two.
- **Bounded.** `limit` at most 500; snippets at most 300 characters; ten seconds per request on the control socket; five seconds per peer in the fan-out, peers asked in parallel, a peer that does not answer in time counted as not searched; answers within the page budget.
- **File filters never open files.** `--file` is a string match against what the index recorded. The one place h8c reads a native file on request is the export of section 6, at an offset the index recorded itself, and it checks that the record at the offset has the id the index expects before returning it.
- **The text is untrusted.** Results are data; the commands never act on them. `--json` escapes everything; the page keeps writing every value as text. The skill says the same to the agent, because a session's text can hold instructions aimed at whoever reads it later.

## 6. lifelog on h8c

### One device at a time, from the local node

Two ways to feed lifelog were weighed:

| | Per device: the collector asks its own node | Central: one node pulls the whole net |
| --- | --- | --- |
| What h8c has to do | An export route on the control socket | Mirror every session of every peer eagerly: today a node copies a session only when it is read |
| Offline machines | As today: the device pushes when it is on, cursor on the device | Only what was copied before; the rest when the machine returns, through the puller's cache |
| Completeness | The device's node reads the native files; it can return the raw record | Copies are bounded (256 MiB, text cut at 200 000 characters) and never carry the raw record |
| Device identity and dedup | Unchanged | A mapping from node ids to lifelog's device names |
| Cache growth | None | Every session of the net on the puller, for good |

**Recommendation: per device.** It keeps lifelog's own rules (cursor on the device, completeness first), needs no change to how h8c copies sessions, and is the only way the raw record reaches the archive. Central pull would need an eager mirror that h8c does not have and would make the archive depend on the copy's limits. A WSL or container that has no node today is a separate matter (roadmap item 7); the native readers make a node without Wake possible there, which helps later.

### The export

`GET /v1/sessions/export?after=SEQ&limit=N` on the control socket, and `h8c export --after SEQ --json`:

- Every local row gets a store-wide, ever-growing `seq` when it is written. The route returns rows above `SEQ` in that order, each with its session (key, agent, reader, title, project, path, branch, model, times, parent, spawned, archived, `app`, totals, native file name) and, for a native-read row, `raw`: the record read back from the file at the recorded offset, with the same large fields removed that the collector removes today, and the content reduced to its text blocks. A row whose record cannot be read back (file gone, id mismatch) comes without `raw` and says so.
- The answer carries the store's `epoch`. A collector that sees a new epoch starts from 0.
- A file read again from the start produces new rows with new `seq` values; the collector sends them again; the hub's upsert makes that harmless. A session the agent deleted comes as a deletion marker that the collector ignores.
- Only local rows are exported: the device's own files, which is what the device pushed before.

### Keys, so that nothing is lost or doubled

Keep the sources and the `source_id` rules lifelog has, so the switch is an enrichment of the rows the hub already holds, not a second copy of them:

| lifelog field | From h8c |
| --- | --- |
| `source` | `claude_code` or `codex`, from the session's agent |
| `source_id` | Claude Code: `native` for the first row of a record, `native#<n>` for a further block. Codex: `<thread>:<turn>:<native>`, with `:out` on a result row |
| `type` | `user_message`, `assistant_message` as today for text rows by role; new: `thinking`, `tool_use`, `tool_result`, `usage`, `compaction`, `system`. The collector keeps deriving its own `lody_operation_result` from the text as it does now |
| `session_id` | Claude Code: the file's name; Codex: the thread id, both from the session's key |
| `app` | the session's `app` |
| `start` | the row's time |
| `text` | the row's text, with the collector's cleaning of user text unchanged |
| `payload` | `raw` where there is one (the same shape as today), plus an `_h8c` object: node, key, id, seq, kind, tool, call, error, files, usage, model, turn, agent, copy, origin, meta |
| session event | `source_id` `session:<name>` as today; payload as today plus `_h8c` with the totals, files, parent, spawned and reader |

What is new for lifelog by this mapping: tool calls and results, thinking, usage, subagent and spawned-thread sessions, continuation marked by `copy`/`origin` instead of silently winning, and the Antigravity ACP sessions that no collector sees today. What stays the same: every row it has today keeps its key and its payload shape.

### Switching over

1. Phase 1 or 2 delivers `h8c export` on a device. The collector gains a source mode that reads it; the hub needs no change (it accepts any `type`).
2. On one device, run the old parser and the new mode side by side for a day, both in dry-run against the same files, and compare: the set of `source_id` values, the counts by `(source, type)`, and the payload of a sample of rows. Every `source_id` the old parser produces must be produced by the new mode with the same `session_id`, `app`, `start` and text. Differences are either a reader bug or a documented improvement (a row the old parser skipped).
3. Switch that device; after a day, compare the hub's counts per `(device, source, type)` before and after, and spot-check rows. Then the others, one at a time. The old parser stays in the collector, off, until every device has switched; then it is removed.
4. The cursor state changes shape (one `seq` and one epoch per device instead of per-file offsets). The first run with the new mode re-sends what the device holds; the upsert absorbs it. Because the agents keep their files for about a month, the overlap window is bounded and nothing older is touched.

## 7. Phases

Each phase is shippable alone, with every test, `go vet`, `gofmt`, the three builds, and `websmoke` where the page changed, run before it is merged, and the roadmap updated.

### Phase 1: Claude Code read natively, with structure, and the export

- The Claude Code reader as a `sessions.Source` writing into the store; the store migration; `Combined` preferring it by file path; `Message` and `Session` fields; `h8c show` printing kinds; the export route and `h8c export`; new protocol samples; a fixture set of made-up transcripts covering text, thinking, tool calls with an error, an edit, a write, a shell edit, a subagent, a continuation, a title record, an unknown record type.
- Accepted when: `go test ./...` with `-race` is green including `protocol_test.go`; a node on this build and one on the current build list and read each other both ways on two real machines; `h8c show --json` of a real session on the machine shows kinds, tools, files and usage with per-call usage counted once; the sum of usage over a session matches Wake's total within what copies and skipped records explain; `h8c export` gives the same rows across a restart, and new `seq` values after a file is rewritten; the demo shows a session with tool rows.

### Phase 2: Codex, ACP, and lifelog switched

- The Codex reader (items first, response records for reasoning and for calls not yet completed); ACP tool calls and thinking after verifying the fields; the collector's h8c mode in the lifelog repository; the side-by-side comparison and the device-by-device switch of section 6.
- Accepted when: the parity comparison in section 6 passes on one device for both agents; after the switch every device's counts per `(device, source, type)` are at or above the counts before it; the new row types appear; a subagent session and a spawned Codex thread are present at the hub; the old parser is removed from the collector.

### Phase 3: search across machines

- The FTS table and the `files` table over local rows and copies; `h8c search` with every filter and `--by-session`; `h8c show --around`; the peer search route and the fan-out with its timeouts; the one-time re-publication; the page's search box and row labels with a `websmoke` check; the skill.
- Accepted when: `h8c search --file X --since 30d --by-session --json` lists the sessions on every online node and the copies of every offline node, each with anchors, and the answer names which nodes were searched live and which from copies; `h8c show --around` shows the text before an anchor; a peer on a build without the route is listed as not searched and the search still answers; a fixture session whose text holds quotes, FTS operators and SQL comes back as text; a search of 100 terms or a term of two characters is answered within the limits or refused with a sentence; an agent, given the skill, answers "where was this file changed and why" from the two commands alone.

### Phase 4: MCP, retention, the rest

- `h8c mcp`; the `gone` flag (decision 3); export of Wake-only agents for lifelog, keyed by session and `seq`, if wanted; a shorthand such as `h8c changes PATH` if `search --file` proves clumsy.

## 8. Decisions for the user

1. **Own the readers for Claude Code and Codex, keeping Wake for the rest and as fallback** (section 2), or stay on Wake and read the native file only when a session is read. *Recommended: own them.* They are needed for lifelog's parity anyway, and only the index answers the questions across machines.
2. **Feed lifelog per device from the local node** (section 6), or have one node pull the net. *Recommended: per device.* It matches lifelog's rules, needs no eager mirror, and is the only path for the raw record. Central pull can come later if a device cannot run a node.
3. **Keep the index of a session after the agent's housekeeping removed its file.** Claude Code keeps transcripts for about a month; today Wake drops the session, h8c marks it deleted, and every peer removes its copy. That is right for a session the user deleted and wrong for one the agent cleaned up, and the two cannot be told apart from the file system. *Recommended: keep it.* A local session whose file is gone is marked `gone` in the feed instead of `deleted`; peers keep what they have; `h8c sessions` shows the mark and `--gone` filters; `h8c forget NODE/KEY` removes one on purpose. lifelog is the archive of record either way; this is about what agents can still find.

## 9. Open points

- Which protobuf fields of ACP hold tool calls and thinking is reported, not verified in code.
- Whether Claude Code repeats identical usage on every block record of a call is what the reader assumes; the fixture test pins it and a check on real files precedes the switch.
- A shell command that edits a file without the agent recording the edit is not in `files`. The index says what the agent recorded.
- Trigram search needs three characters; two-character Chinese terms go through the `LIKE` path, which is slower and bounded.
- Thinking is kept, cut at 32 000 characters, and indexed, because the reasons a path was abandoned are often only there. If that proves too large or too private for the net, a flag can keep it local.
- Copies of sessions read before this change have no structure until their node is online and they are read again.
- The export reads native files on request; a file the agent rewrote in place between indexing and export is detected by the id check and reported without `raw`, and re-indexed by the next round.
- Wake's keys for a Codex thread split over several files are not known; the fallback by file path covers it.

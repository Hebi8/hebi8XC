# Where it stands and what could come next

Updated on 2026-11-10. How things work is in [design.md](design.md); this page says what exists, what has been tried for real, and what is left.

## What exists

- **Nodes.** Each machine has a node with a fixed id, a name (`h8c name`) and an Ed25519 key.
- **Same user = same GitHub account.** `h8c login` registers the node's key on the account as an SSH signing key titled `hebi8xc: <name> (<id>)`. Nodes read the account's public key list, keep a cache of it and need no token afterwards. Keys can also be trusted by hand for machines without internet access.
- **Discovery.** mDNS on the LAN, the Tailscale peer list, remembered addresses and `--peer`. Addresses that do not answer are asked less often.
- **Connection.** Nodes connect directly over mutual TLS 1.3 and accept only keys from the roster. No hub.
- **Protocol compatibility.** Builds of the same protocol version work together, so machines are updated one at a time. Samples of the wire format are kept as tests.
- **Sessions.** A node reads the index that [Wake](https://github.com/iAmCorey/Wake) keeps of the agent sessions on its machine and keeps it fresh with `wake-cli`. Nodes exchange their lists; the text of a session is fetched when someone reads it and kept as a copy. A machine that is off is still listed, and what was read before can still be read.
- **Antigravity ACP.** Reads the native per-session SQLite databases alongside Wake, including live WAL data and `.meta` project paths. Native IDs deduplicate overlapping Wake entries. Shares user and assistant text with bounded paging and titles from the first user message (one line, 80 characters). Unchanged files reuse cached metadata; read failures retain the last successful result until the file is deleted. Wake and ACP retain their last successful lists independently, and message reads work before the first listing. The optional transcript adds calls, results, errors, thinking and per-step usage by database step index; tool-only planner steps get empty-text host messages. Attachments are not included.
- **Structured sessions, Phase 1.** Reads the Wake fork's optional side tables, with full local text, kind, thinking, tool calls, error flags and child-session keys. Stale or absent structure falls back to indexed text with one-time logging. Protocol 1 gains optional fields and a page format; copies retain structure with wire bounds, old copies refresh when online with probes tracked per session and revision, and existing local entries are republished once. Upgrading replays remote metadata while retaining copies from the same epoch; malformed ACP transcript lines leave database text available. `h8c show` prints kinds and calls. The demo includes structured calls, readable with its printed CLI command and, since Phase 3, on the page.
- **Structured sessions, Phase 2 (h8c).** Reads Wake structure format 2 usage and file arguments while retaining format 1. Session token summaries prefer per-message sums, otherwise the known source total; file summaries contain up to 500 distinct non-error change/delete paths. ACP maps observed explicit file tools and aggregates its existing usage. Protocol 1, feed storage, JSON commands and plain `show` carry the additions. A schema-2 local export log provides immutable session/message snapshots, generations, transactional scan progress, persisted initial watermark, epoch and acknowledgement pruning over the control socket and `h8c export`. The web and peer routes do not expose export. Wake adapter changes, lifelog collection and deployment are separate work. Export retains rich snapshots during stale structure, binds acknowledgements to the supplied epoch, and commits bounded batches with a savepoint per session; stamp-only and metadata-only baseline changes do not export history. A first real baseline change emits every current row even during degradation, and initialization includes message timestamps at or after the watermark when the source has advanced beyond the feed. Structure levels protect existing exports across a format-2 to format-1 rollback. Deployment order: upgrade h8c on all five machines, upgrade Wake to format 2 and complete backfill, then initialize export on each machine. Old h8c reads format 2 as text-only and its copies lack call files.
- **Structured sessions, Phase 3 (Go core).** Wake trigram/CJK and bounded side-table search, bounded ACP scan, additive FTS/file indexes over copies, `h8c search` with all filters and grouping, and `show --around --rev` are implemented. Peer fan-out reports live/copies/none and partial sources; 404 and injected timeout tests exercise fallback. Searches retain matches at budgets, sources/copies run concurrently, and copied data uses its own revision for moved. Indexed queries keep FTS driving the session scope; side-only truncation reports scanned_since instead of partial. Copies record optional file-structure declarations, and missing structure carries a session-counted note. Around exposes has_before; show accepts a unique key without a node. Control and authenticated web APIs expose search and around. Schema stays 2 with old-writer-compatible triggers and atomic backfill.
- **Structured sessions, Phase 3 (page).** A search view beside the list: terms, file, agent, project, machine, a recent span or a range of dates, and under "more conditions" operation, failed attempts, tool, kind and failed calls only; places found one by one or grouped by session; how each machine was searched (live, copies only, not searched) and whether its part is incomplete, with the node's note; a note when more was found than listed, and up to 500 on request. A place found opens its session there with `around`: the message marked and in view, a call opened, earlier and later messages a click away, the place kept in the address, and a note when the session has changed since. Under each message its calls with tool, failure and files, input, output and thinking folded until clicked; all of it written as text. The demo now has calls with files and thinking on desk and laptop, a laptop session that reads the file a desk session changed, and an attic index of text only, so its search answers from a copy and is incomplete; its structure stamp, which still said format 1 after Phase 2 moved the fixtures to format 2, had kept every demo session text-only. `websmoke` checks all of this, in a narrow Chinese window too. The agent skill remains separate work.
- **Three ways to use it.** `h8c sessions`, `h8c search` and `h8c show`; `--json` on each for programs; a read-only page in the browser (`h8c web`).
- **Platforms.** Linux, macOS and Windows, as a service on each: see [running-as-a-service.md](running-as-a-service.md).
- **Tools for working on it.** `go run ./scripts/demo` and `go run ./scripts/websmoke`, described in `AGENTS.md`.

## What has been run for real

Five machines of one user across Linux, macOS and Windows run it as services. The original four-machine checks began on 2026-10-13, with about 6500 sessions between them. Checked on them:

- logging in on each machine and finding each other with no key added by hand, over mDNS and over Tailscale;
- a node going away and coming back;
- listing and reading sessions in every direction, and reading a copy while the other machine was off;
- a new build on one machine against the previous build on another;
- the page in a browser on macOS.

A read-only ACP adapter check listed 218 of 219 local database files: 107 with messages and 111 empty. One file lacked the expected tables and was skipped. Two sessions were compared against the installed ACP protobuf descriptors and historical replay rules; message text, roles and timestamps matched. No running node was restarted or updated for this check.

Phase 1 checks on one machine: 107 ACP transcripts, 2,314 steps, all matching database step indices, types and creation times to the transcript's second precision. A fresh temporary fork index held 5,480 sessions across six agents and 135,860 calls. `h8c show --json` returned calls for one real session per agent and the longest full message intact (474,847 characters, 554,120 UTF-8 bytes). Two loopback nodes, this build and `85465b9`, listed and read each other both ways; a copy made by the old build refreshed after the reader was upgraded. Phase 1 was deployed to four machines on 2026-11-03 and the fifth on 2026-11-10. A later counts-only transcript check covered 109 files and 2,436 steps: 708 planner steps had input tokens, none had output tokens without input tokens, and five non-planner steps with tokens were checkpoints. The other non-message types were six checkpoints and seven system messages, not omitted tool-result types. Checkpoint usage is not exposed by the Phase 1 user/planner message reader. A fresh stock Wake 0.9.2 temporary index (2,316 sessions) was also readable as text, with the structure fallback logged once. No installed node or index was changed.

Phase 3 final-round measurements (2026-11-10), counts/timings only. SQLite backups were read without modifying the online index or service. The Wake backup held 5,786 local sessions and 91,539 messages; a full read-only timestamp check found **zero messages in zero sessions** later than sessions.updated_at. Pure filters therefore use that verified session upper bound. Queries used LocalSearch/Combined with 5,908 published keys, 299 ACP keys, limit 50 and a four-second budget. Before each cold query, backup files were synced and evicted with POSIX_FADV_DONTNEED; the same query then ran hot in the same process.

| Query | Cache | ms | Returned hits | partial | scanned_since | Note categories |
| --- | --- | ---: | --- | --- | ---: | --- |
| tool Bash | cold | 396 | 50 | yes | absent | missing structure |
| tool Bash | hot | 383 | 50 | yes | absent | missing structure |
| file store.go | cold | 4001 | 1 | yes | absent | time budget, missing files |
| file store.go | hot | 3299 | 1 | yes | absent | missing files |
| error, by session | cold | 4004 | 770 in 50 groups | no | 1793971939295 | side coverage |
| error, by session | hot | 915 | 776 in 50 groups | no | 1793779738129 | side coverage |
| error, tool Bash | cold | 4004 | 50 | yes | 1794276425866 | missing structure, side coverage |
| error, tool Bash | hot | 3177 | 50 | yes | 1792589251800 | missing structure, side coverage |

All eight calls returned without an error. The cold pure-filter calls retained matches; a side-only timeout after indexed body completion did not make the grouped query partial. Structural notes include affected-session counts and newest timestamps. Deterministic tests cancel reads in a later session batch and during side scanning, skip one unreadable ACP session while retaining another, reject raw-error disclosure, remove contradictory completeness wording, exercise BUSY with an injected short wait and BEGIN EXCLUSIVE, and assign a fixed file mtime to an empty ACP fixture. All temporary databases and probes were removed after measurement. No export was run.

The earlier review confirmed the schema-2 rollback chain and the cross-version protocol. This round adds no protocol field and leaves every original protocol sample unchanged.


Not yet tried on a real machine: the firewall commands in the README's Windows section, removing a machine by deleting its key, and a machine that only has hand-trusted keys.

## What could come next

Roughly in the order that seems most useful. Structured sessions Phase 1, the Go portions of Phases 2 and 3 and the Phase 3 page are implemented; external integration, the Phase 3 skill and later phases remain. Items 1, 2 and 6 are designed together in [proposals/structured-sessions.md](proposals/structured-sessions.md), which also says how a personal data store, lifelog, would take its session events from h8c; the fork, per-device lifelog collection and eventual retention choices have been accepted. Retention remains Phase 4.

1. **Agent skill.** The Go core, the APIs and the page's search and calls are implemented, with websmoke checks. The skill and the agent evaluation remain.
2. **An MCP server for agents.** `h8c mcp`, read-only, with list, search and read over the whole net, so an agent on one machine can pick up what was done on another. What it returns comes from session text and must be treated as untrusted.
3. **A client inside an agent app.** An app such as Lody could list sessions from the local node (`h8c sessions --json`, or the page's API) instead of importing them project by project. Continuing a session would still have to happen on the machine that holds it.
4. **Reading from a phone or another device.** The page is for the browser on the same machine. Serving it to other devices needs its own answer to who may read.
5. **Releases.** A version tag, built binaries, and `h8c service install` in place of the hand-written service files. Until there is a tag, builds are named by their commit.
6. **More of a session.** The h8c Phase 2 readers and export are implemented. Wake adapter usage/error completion and the lifelog switch are separate work; remaining Phase 3 work adds the skill, followed by MCP. Wake still omits meta, compaction and thinking-only messages; calls retain the fork's input/output cuts.
7. **Sessions that Wake on the host does not see**, such as those inside WSL or containers.
8. **Continuing a session on its machine**, and handing work from one machine to another. This means driving agents, not only reading what they wrote, and is a larger step.
9. **More than one user.** Out of scope so far: every node in the roster is the same person and is trusted fully. Sharing between people needs per-peer permissions, which nothing here has.

## Small things left open

- Deferred S8: a read transaction can slow writers. Measured contention is acceptable for now; WAL or shorter read transactions can be evaluated later.
- Deferred ACP two-step “no calls” declarations: only two of four observed cases could support that declaration, and the rule would be empirical. Wait for documented ACP format semantics.

- Export generations restart at 0 after store loss, so a previously renumbered session can overwrite old generation-0 collector rows; Wake also omits Claude Code inputs queued while the agent is busy (six messages across four devices in a one-week lifelog comparison). See the design limits.

- A new node refuses peers for the few seconds before its first roster fetch finishes; they log one line and recover on the next round.
- Whether Windows needs the UDP 5353 firewall rule for mDNS has not been checked.
- Copies of sessions are never removed from a node's cache.
- The link from `h8c web` stays in the browser's history, and there is no way to log a browser out short of deleting `web.token`.
- The ACP adapter depends on an undocumented protobuf/SQLite format; unreadable individual files retain their last successful metadata, or are skipped if never read successfully. Native titles and non-text steps are not reconstructed.
- The Wake adapter supports stock v0.9.2/v0.10.2 and optional fork structure formats 1 and 2. A row-count mismatch falls back to text; the fork currently does not repair that mismatch unless another stale-stamp condition triggers reparsing. A CLI-only installation has no full-retry entrypoint for a transient structure failure. A newer Wake may change its index; its last successful list then stays while ACP updates. Without a successful Wake listing, the local listing fails and the adapter needs a look.
- Reading the roster uses GitHub's allowance for requests without a token, 60 an hour per public address, shared by every machine behind the same router. A node uses 8 an hour.

The longer list is under "Known limits" in [design.md](design.md#known-limits).

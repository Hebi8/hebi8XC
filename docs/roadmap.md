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
- **Structured sessions, Phase 1.** Reads the Wake fork's optional side tables, with full local text, kind, thinking, tool calls, error flags and child-session keys. Stale or absent structure falls back to indexed text with one-time logging. Protocol 1 gains optional fields and a page format; copies retain structure with wire bounds, old copies refresh when online with probes tracked per session and revision, and existing local entries are republished once. Upgrading replays remote metadata while retaining copies from the same epoch; malformed ACP transcript lines leave database text available. `h8c show` prints kinds and calls. The demo includes structured calls, readable with its printed CLI command; page rendering of calls remains Phase 3.
- **Structured sessions, Phase 2 (h8c).** Reads Wake structure format 2 usage and file arguments while retaining format 1. Session token summaries prefer per-message sums, otherwise the known source total; file summaries contain up to 500 distinct non-error change/delete paths. ACP maps observed explicit file tools and aggregates its existing usage. Protocol 1, feed storage, JSON commands and plain `show` carry the additions. A schema-2 local export log provides immutable session/message snapshots, generations, transactional scan progress, persisted initial watermark, epoch and acknowledgement pruning over the control socket and `h8c export`. The web and peer routes do not expose export. Wake adapter changes, lifelog collection and deployment are separate work. Export retains rich snapshots during stale structure, binds acknowledgements to the supplied epoch, and commits bounded batches with a savepoint per session; stamp-only and metadata-only baseline changes do not export history. A first real baseline change emits every current row even during degradation, and initialization includes message timestamps at or after the watermark when the source has advanced beyond the feed. Structure levels protect existing exports across a format-2 to format-1 rollback. Deployment order: upgrade h8c on all five machines, upgrade Wake to format 2 and complete backfill, then initialize export on each machine. Old h8c reads format 2 as text-only and its copies lack call files.
- **Structured sessions, Phase 3 (Go core).** Wake trigram/CJK and bounded side-table search, bounded ACP scan, additive FTS/file indexes over copies, `h8c search` with all filters and grouping, and `show --around --rev` are implemented. Peer fan-out reports live/copies/none and partial sources; 404 and injected timeout tests exercise fallback. Searches retain matches at budgets, sources/copies run concurrently, and copied data uses its own revision for moved. Indexed queries keep FTS driving the session scope; side-only truncation reports scanned_since instead of partial. Copies record optional file-structure declarations, and missing structure carries a session-counted note. Around exposes has_before; show accepts a unique key without a node. Control and authenticated web APIs expose search and around. Schema stays 2 with old-writer-compatible triggers and atomic backfill. The page search box, call display and agent skill remain separate work.
- **Three ways to use it.** `h8c sessions` and `h8c show`; `--json` on both for programs; a read-only page in the browser (`h8c web`).
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

Phase 3 third-round measurements (2026-11-10), counts/timings only. devNuc used SQLite backups of Wake and the store. The Wake backup was already structure format 2 (5,774 sessions); no rebuild or online write was needed. Queries used **LocalSearch/Combined**, including all 5,895 published keys and 296 native ACP keys, with limit 50. The allow-list is one JSON parameter; no ACP exclusions are added. EXPLAIN regression coverage includes format-2 scope, hundreds of ACP keys and a 10,001-row group limit. The side range is evaluated once, capped at 20,000 messages.

| Query | ms | Returned hits | Wake hits in returned result | partial | scanned_since (Unix ms) |
| --- | ---: | ---: | ---: | --- | --- |
| error | 3780 | 50 | 50 | no | 1793736073749 |
| test, tool Bash | 4938 | 50 | 50 | yes | 1792475254263 |
| two-character CJK | 453 | 50 | 50 | no | 1793736073749 |
| error, by session | 1183 | 726 in 50 groups | 717 | no | 1793736073749 |
| file suffix | 595 | 1 | 1 | yes | absent |

Both partial rows were caused by four ACP sessions missing relevant structure; Wake completed them without partial. Side coverage alone no longer makes the other rows partial. Repeated-run timings varied with load/cache state (for example error also completed in 494 ms); these are the last full-matrix measurements, not only direct Wake timings. A real-data peer-route call returned 50 Wake hits in 762 ms; a virtual-clock test verifies retention at the four-second peer deadline.

On mini, SQLite backup copied a 113,233,920-byte store; the largest copied node held 15,972 messages. A darwin/arm64 probe ran searches concurrently with calibrated exclusive write transactions, using repeated metadata updates rather than sleeps. For the same approximately 1.8-second write contention, f59d8bb failed 10/10 queries with BUSY (1015–1026 ms, zero hits); the final implementation failed 0/10 (1842–2235 ms, 500 total hits, no partial or raw errors). Writer durations were 1786–1855 ms for the baseline and 1749–1808 ms for the final run. A deliberately longer 5560-ms writer produced a 5081-ms partial answer with the human retry note and no raw error. This is a controlled contention comparison, not an estimate of production failure probability.

No running service, online index or export was changed. Temporary databases, probes and the baseline archive were deleted on both machines. New message-page structure declarations remain additive to protocol 1, with old content writers invalidating the declaration. The previously reviewed rollback chain remains schema-2 compatible. Tests cover missing/lowered declarations, old-writer invalidation and refresh, counted partial reasons excluding users, ACP historical windows, clipped around context, node coverage propagation, the 4-second peer budget, Windows errno 10061/10013 and the parameter-count/query-plan regressions.


Not yet tried on a real machine: the firewall commands in the README's Windows section, removing a machine by deleting its key, and a machine that only has hand-trusted keys.

## What could come next

Roughly in the order that seems most useful. Structured sessions Phase 1 and the Go portions of Phases 2 and 3 are implemented; external integration, Phase 3 page/skill work and later phases remain. Items 1, 2 and 6 are designed together in [proposals/structured-sessions.md](proposals/structured-sessions.md), which also says how a personal data store, lifelog, would take its session events from h8c; the fork, per-device lifelog collection and eventual retention choices have been accepted. Retention remains Phase 4.

1. **Search UI and agent skill.** The Go core and APIs are implemented. Add the page search box and calls below messages with a websmoke check, then the skill and agent evaluation.
2. **An MCP server for agents.** `h8c mcp`, read-only, with list, search and read over the whole net, so an agent on one machine can pick up what was done on another. What it returns comes from session text and must be treated as untrusted.
3. **A client inside an agent app.** An app such as Lody could list sessions from the local node (`h8c sessions --json`, or the page's API) instead of importing them project by project. Continuing a session would still have to happen on the machine that holds it.
4. **Reading from a phone or another device.** The page is for the browser on the same machine. Serving it to other devices needs its own answer to who may read.
5. **Releases.** A version tag, built binaries, and `h8c service install` in place of the hand-written service files. Until there is a tag, builds are named by their commit.
6. **More of a session.** The h8c Phase 2 readers and export are implemented. Wake adapter usage/error completion and the lifelog switch are separate work; remaining Phase 3 work adds the page and skill, followed by MCP. Wake still omits meta, compaction and thinking-only messages; calls retain the fork's input/output cuts.
7. **Sessions that Wake on the host does not see**, such as those inside WSL or containers.
8. **Continuing a session on its machine**, and handing work from one machine to another. This means driving agents, not only reading what they wrote, and is a larger step.
9. **More than one user.** Out of scope so far: every node in the roster is the same person and is trusted fully. Sharing between people needs per-peer permissions, which nothing here has.

## Small things left open

- Export generations restart at 0 after store loss, so a previously renumbered session can overwrite old generation-0 collector rows; Wake also omits Claude Code inputs queued while the agent is busy (six messages across four devices in a one-week lifelog comparison). See the design limits.

- A new node refuses peers for the few seconds before its first roster fetch finishes; they log one line and recover on the next round.
- Whether Windows needs the UDP 5353 firewall rule for mDNS has not been checked.
- Copies of sessions are never removed from a node's cache.
- The link from `h8c web` stays in the browser's history, and there is no way to log a browser out short of deleting `web.token`.
- The ACP adapter depends on an undocumented protobuf/SQLite format; unreadable individual files retain their last successful metadata, or are skipped if never read successfully. Native titles and non-text steps are not reconstructed.
- The Wake adapter supports stock v0.9.2/v0.10.2 and optional fork structure formats 1 and 2. A row-count mismatch falls back to text; the fork currently does not repair that mismatch unless another stale-stamp condition triggers reparsing. A CLI-only installation has no full-retry entrypoint for a transient structure failure. A newer Wake may change its index; its last successful list then stays while ACP updates. Without a successful Wake listing, the local listing fails and the adapter needs a look.
- Reading the roster uses GitHub's allowance for requests without a token, 60 an hour per public address, shared by every machine behind the same router. A node uses 8 an hour.

The longer list is under "Known limits" in [design.md](design.md#known-limits).

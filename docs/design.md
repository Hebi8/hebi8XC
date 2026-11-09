# Design

hebi8XC connects the machines of one user. Each machine runs a node. There is no hub and no server: nodes connect pairwise.

This document covers discovery, the handshake, the roster, the node list and the first service on top of them: the sessions of the AI coding agents on each machine.

## Discovery and authentication are separate

Discovery produces candidate addresses and nothing more. A candidate becomes a peer only when the handshake succeeds. A wrong or hostile candidate costs one failed connection.

Sources are pluggable (`discovery.Source`) and used together:

| Source | Candidates |
| --- | --- |
| `mdns` | Nodes announcing `_hebi8xc._tcp` on the local link with our network tag. At most 64 are remembered. |
| `tailscale` | Every online peer in `tailscale status --json`, on port 4892. Silent when Tailscale is missing or stopped; skipped for the round when the CLI takes more than 3 s. |
| `remembered` | Every address a known peer has answered on (`peers.json`). Tried from the first round after a restart. |
| `static` | `h8c up --peer host[:port]`. |
| `inbound` | The address a peer that called us can be called back on (its source IP and the port it reported). |

An mDNS announcement holds the node id (as instance name and host name), the port, and `net=<tag>`. No key, no user name, no machine name. The **network tag** is the first 8 bytes of `SHA-256("hebi8xc network v1\0" + user id)` in hex. It keeps nodes from probing other users' nodes on a shared LAN. It is a filter, not a secret.

mDNS runs on IPv4 on one LAN interface, so a machine with many virtual bridges announces one address. A LAN interface is up, does multicast, is neither loopback nor point-to-point, and has a private IPv4 address (10/8, 172.16/12, 192.168/16); an interface whose address is in 100.64.0.0/10, such as Tailscale's, is not one. The choice is the interface the OS routes multicast through if that is a LAN interface, which it is on an ordinary Linux or macOS machine. Otherwise, as on a Windows machine that prefers its Tailscale adapter, it is the LAN interface with the lowest index. With no LAN interface at all, mDNS is off and the node says so.

The **user id** names the user in the roster and in the network tag. On a node that is logged in it is `github:<numeric account id>`; the id, unlike the login, never changes. On a node that is not, it is `--user`, defaulting to the OS user name. The two kinds have different tags, so they do not find each other over mDNS.

## Identity

Created on first start, in the data directory. The id and the key never change:

- `node.json`: node id (8 random bytes, hex) and name. The name is the host name until `h8c name` sets another; it is the one part that may change. A running node keeps the name it started with.
- `node.key`: Ed25519 private key, PKCS#8 PEM, mode 600.

Public keys are written in OpenSSH format (`ssh-ed25519 AAAA...`), the format GitHub uses for SSH signing keys.

If one of the two files is missing the node refuses to start instead of making a new identity. The files are created exclusively and never overwritten: of two first starts at the same moment, one creates the identity and the other loads it.

## Roster

The roster answers "which public keys belong to this user". A node holding one of those keys is one of the user's machines.

```go
type Roster interface {
    Keys(user string) ([]ed25519.PublicKey, error)   // from the local cache, called in every handshake
    Refresh(ctx context.Context, user string) error  // reload the cache from the source of truth
}
```

A node trusts a key that is in either of two rosters:

- `roster.File`, the local one: the file `trusted_keys`, one key per line, managed with `h8c trust`. It is for machines that cannot reach GitHub. The node re-reads it at the start of every discovery round, and at once when `h8c trust` tells it to: a removed key is refused by the time `h8c trust remove` returns. The file is replaced in one step.
- `roster.GitHub`, on a node that is logged in: the SSH signing keys of the GitHub account.

### GitHub roster

`h8c login` runs the device flow of the project's GitHub App, asks GitHub which account the token belongs to, and registers the node's public key on it as an SSH signing key titled `hebi8xc: <node name> (<node id>)`, unless the key is already there. It stores the account's numeric id and login in `account.json`. The token and the refresh token are used in memory and thrown away; nothing later needs one. Requests that carry the token do not follow redirects. Running it again changes nothing.

A refresh makes two requests, both without a token:

1. `GET /users/{login}/ssh_signing_keys` lists the signing keys under the login the account had at the last refresh, or at `h8c login` before the first one. A list of 1000 keys or more is not read to the end and counts as a failed refresh.
2. `GET /user/{id}` returns the login the account has now.

Keys can only be listed by login, and after a rename the old login may belong to someone else. So the list is used only if the login of step 2 is the one it was read under. If not, the list is thrown away and both steps are repeated with the new login; if the login has changed again by then, the refresh fails and the cache stays.

Of those, the node keys are the Ed25519 keys whose title starts with `hebi8xc: `. The keys the user signs commits with have other titles and are not nodes. The result is written to `github_keys.json` (mode 600), replaced in one step, and loaded from there at start. A cache file that cannot be read as one is ignored with a warning and replaced by the next refresh.

The node refreshes at start and then every 15 minutes, beside the discovery rounds, so GitHub being slow does not delay a heartbeat. The one exception is a start with no cached keys, as on the first start after `h8c login`: the first round waits for the first fetch to end, at most 10 seconds, because until then every node of the account would fail the trust check and be logged as a stranger. Without a token GitHub allows 60 requests per hour per IP address, shared by all nodes behind one router, and a conditional request that changes nothing still counts; 15 minutes is 8 requests per node and hour. `h8c roster refresh` makes the running node refresh now and returns when it has: a key that is no longer listed is refused from then on.

When a refresh fails, for lack of network or of requests, the node keeps the cached keys and logs how old they are. The cache does not expire.

Consequences:

- A machine that logs in is accepted by the nodes already running at their next refresh, at most 15 minutes later.
- The title of a node's key holds the node's name and is public. `h8c name` does not touch GitHub; the title changes when the key is deleted there and `h8c login` registers it again.
- Removing a machine means deleting its key on GitHub, then refreshing the others. GitHub's public list still showed a deleted key more than 20 seconds later in a test, so the removal is not immediate even with a refresh.
- Whoever can add a signing key to the account can add a node.

Facts and sources are in [github-roster.md](github-roster.md).

No token or other secret is ever sent to a peer.

## Connection

Every node runs an HTTPS server with mutual TLS 1.3 (`internal/link`). Calls between nodes are plain HTTP requests.

- Each side presents a certificate self-signed with its node key. The TLS handshake proves it holds the private key.
- There is no CA. Hostname, chain and validity are not checked. Each side takes the public key from the other's certificate and looks it up in its roster (`VerifyConnection`, so it also runs on resumed sessions).
- The client checks the server before it sends its own certificate, so a node does not reveal its key to a server it does not trust. The server's certificate is visible to anyone who connects.
- The server checks the roster again on every request, and the client opens a new connection per call. Removing a key takes effect on the next request.
- Trust must be mutual. If A trusts B but B does not trust A, B refuses A's certificate with a TLS alert and A reports "trusted but refused"; when B dials A, B reports the unknown key. Neither lists the other. A connection that just breaks is reported as a connection failure, not as a refusal.
- The client does not follow redirects, so only the node that passed the handshake can answer a call.
- Request and reply bodies are capped at 64 KiB. A request has 10 s to arrive.

`POST /v1/hello` is how nodes greet: both sides send `{id, name, port, protocol, version}`. A service is more routes on the same mux, as the [session module](#sessions) shows; a handler gets the caller's key from `link.PeerKey`.

### Protocol compatibility

Machines are updated one at a time, so two nodes rarely run the same build. What nodes say to each other, the hello, the session feed and the reading of messages, has a **protocol version**, a whole number that starts at 1. The rule:

- **Any two builds of the same protocol version work together.** Not "should": it is tested, see below.
- Within a version a new field that is optional is allowed. A new route that a peer may not serve, where the caller can work without it, is the same kind of change. A node must do without any field its peer does not send, and ignores fields it does not know. Removing a field, renaming it, requiring a field that used to be optional or was not there, and changing what a field means are not allowed.
- Anything else needs the next protocol version.

Each node says its protocol version and its software version in the hello. A node that says neither runs a build from before these fields existed; all of those speak protocol 1 and are treated so. The node list keeps both values for every peer, and `h8c peers` shows them.

When two nodes speak different versions, nothing fails mysteriously: they still find each other and show each other as online, the node logs once that the peer speaks protocol N while it speaks M, `h8c peers` and `h8c sessions` mark the peer as incompatible with both numbers, and no sessions are read from it. Sessions and copies that were read before stay and are shown.

The samples in `internal/node/testdata/protocol-v1` are the requests and answers of build `3dc8c5a`, the last one before nodes said their version. Two tests hold the rule in place: this build must understand every one of those answers, and its own answers must still contain every field in them, with the same value for the same data. A change that breaks one of them is a new protocol version, not a reason to edit a sample.

One field was added within version 1 so far: `epoch` in a page of messages. A reader that gets a page without it, from an older build, skips the comparison of that page's epoch with the feed on record. What is left is the check when the copy is put in use, which compares the feed on record then with the one the copy was started under. The case no longer caught at once is a node that starts a new feed in the middle of a copy and whose new feed this node has not read yet; the copy is then kept until that feed is read, which removes it.

Default port: **4892** TCP (`h8xc` on a phone keypad).

## Node list

One entry per node id, stored in `peers.json`: id, name, public key, known addresses (each with the sources that suggested it and when it last worked), all sources, time of last contact, online or offline, and the protocol and software version it stated in its last hello.

- A discovery round runs every 10 s: collect candidates, send hello to each address that is due, at most 16 at a time, record the answers. The round is also the heartbeat.
- An address that does not answer is probed less often: the gap doubles from 20 s up to 5 min. An answer from it, or a hello from the peer behind it, puts it back on every round. A node that answers but fails the trust check stays on every round.
- A hello received from a peer counts as contact too, so a node that can only dial out still shows as online.
- A peer with no contact for 30 s is marked offline and kept. The next contact brings it back.
- The same node found through several sources or addresses is one entry. At most 8 addresses are kept per peer.
- Online state is not stored; after a restart every peer is offline until contacted.

## Sessions

Every node makes the agent sessions of its machine visible to the others, and any node answers for the whole network: which sessions there are, and what is in one. What a node has read from another it keeps, so a machine that is switched off still shows its sessions, and those that were read before can still be read.

```
agent session files -> Wake index -> feed of this node --link--> copy on each peer
                        (wake.db)     (sessions.db)               (its sessions.db)
                                                                        |
h8c sessions / h8c show --control socket--> node: own feed + copies + cached messages
```

### The index of a machine

[Wake](https://github.com/iAmCorey/Wake) parses session files for some twenty agents and keeps the result in a SQLite database. `internal/sessions/wake` reads it:

- It opens the database read-only, with a busy timeout, the way Wake's own side tools do. A reader does not block Wake's writes. It never changes the journal mode and never writes.
- The database is Wake's internal format. Everything that depends on it is in that one package, checked against Wake v0.9.2 and v0.10.2. Before reading, it checks that the tables and columns it uses exist. If not, it reports that it cannot make out the structure of the index, and Wake's last successful list is kept while ACP can still update. If Wake has never been read successfully, the local listing fails; other machines' sessions are still listed. It looks again every round, so a later repair is picked up.
- Sessions that Wake mirrored from other hosts (`host` not empty) are left out. They belong to another machine.
- The fork's optional format-1 side tables add calls, thinking, model and full text. Format 2 additionally reads nullable per-message usage from `message_extras` and file arguments from `tool_files`; format 1 remains readable. They are read in the same SQLite snapshot as the indexed rows. Structure is used only when the session's structure stamp matches its file size, modification time, FTS format and actual message-row count. Missing or unknown structure is logged once per index; stale structure once per session, and both show indexed text only. The structure stamp joins the feed fingerprint, so a backfill or a change beyond the indexed text moves the revision.
- What it reads of a session: key (`<agent>:<id>`), agent, title, project name and path, branch, model, creation and update time, message count, parent session, whether an agent spawned it, whether it is archived. Of a message: Wake's text for it, role, time, number, side thread. This is the text Wake indexes, not a rendering of the session file.

Where `wake-cli` is found, on PATH or through `--wake-cli`, the node runs `wake-cli refresh` every 2 minutes, and `wake-cli index` first if there is no index. While the Wake app runs, both commands leave the index alone and succeed, because the app keeps it current. A `refresh` that takes more than 2 minutes, or an `index` that takes more than 10, is stopped; the round goes on with the index as it is and with the peers. Without `wake-cli` an existing index is read as it is. The index is taken from Wake's default location unless `--wake-db` names another, and it does not have to exist when the node starts: it is looked for every round.

`internal/sessions/antigravity` also reads `~/.gemini/antigravity-acp/conversations/*.db` directly (override with `--acp-dir`), with the project path from the adjacent `.meta` file's `cwd`. Each database is opened read-only with a busy timeout and one read transaction, including committed WAL data. It never changes journal mode. A missing directory is optional before its first successful listing. Session metadata is cached by filename and the sizes and modification times of the database, its WAL, the `.meta` file and the optional adjacent brain transcript. Unchanged files reuse their cached metadata without reopening SQLite. A failed read keeps the last successful metadata and stamp so the file is retried next round; a never-readable file is skipped. Only a file absent from a successful directory listing is removed. Empty databases with the expected tables are listed as empty sessions.

The ACP protobuf fields were verified against the descriptors and historical replay in `agy_acp_server.par` v1.0.0 (`92521fc3cbd964bd`):

- `steps.step_type` 14 is user input; `step_payload` field 19.2 is `user_response`.
- Type 15 is the planner's response; field 20.8 (`modified_response`) takes precedence over 20.1 (`response`), as in ACP's replay. With a transcript, a tool-only response is an empty-text message that hosts its calls; without one it is skipped. Thinking, tools and attachments are not message text.
- `steps.metadata` field 1 is creation time, field 8 completion time; both are protobuf timestamps (seconds 1, nanoseconds 2). Session times span the steps, also using `trajectory_metadata_blob.data` field 2 when it provides creation time.
- `steps.metadata` field 24.8 is `ModelInfo.model_name`; the last available name is used. Numeric model enums are not guessed into names. No title is stored in the inspected tables: the first user message supplies a title, with whitespace collapsed to one line and cut at 80 Unicode characters. Without a user message it stays empty. Branch and unsupported relationship fields stay empty.

The optional `brain/<id>/.system_generated/logs/transcript.jsonl` contributes calls, results, errors, thinking and per-step usage, joined by step index. Result records carry no call id, so results follow the pending calls in order within a planner step. Text still comes from the database because transcript content is truncated. A missing transcript gives text only. A malformed transcript line is skipped without hiding the database session or its text.

Message IDs are `idx + 1` so the zero-based first step is above the initial cursor 0; `seq` remains the original step index. Protobuf fields are read through bounded SQLite blob slices, skipping unknown fields by their length. A page stops at the item or JSON budget, and local structured text is read whole; peer text is cut at `MaxText` Unicode characters. Main database and WAL modification stamps invalidate cached copies even when an in-progress response changes without a new step.

The rest of the module only sees `sessions.Source`: list local sessions and read one. `sessions.Combined` merges Wake and ACP. ACP keys are `antigravity-acp:<id>`; Wake's existing keys stay unchanged. For the same native ID in `antigravity:<id>` and `antigravity-acp:<id>`, only the ACP entry is published, because the native database does not lag a Wake refresh. Titles and paths are never used to deduplicate. Reads route by key prefix even before the first listing: `antigravity-acp:` goes to ACP, everything else to Wake. The feed lookup rejects hidden duplicates before routing. Each source keeps its last successful list independently: a failed source retains that list while other sources update. Before a source has succeeded, `ErrNoIndex` is optional, but any other error still fails the whole listing.

### The feed

Wake's index has no counter that says what changed since when; a session can change in ways its update time does not show, and old sessions can appear late. So the node keeps its own record: the **feed**, in `sessions.db` in the data directory. Every minute it compares the index with the feed. A session that is new or different gets the next **revision** number; one that is gone keeps its key and gets a revision too, as a mark that it was deleted. An index that cannot be read changes nothing: it is not an index without sessions.

A peer reads the feed with a cursor: "everything above revision N". It therefore gets every change once, deletions included, and nothing has to be compared or guessed. The feed has an **epoch**, a random id made with the store. A node whose store was lost starts a new feed; its peers see the new epoch, drop what they had from it and read from the start. That is the only time a feed is read again in full.

### Between nodes

Two routes on the peer link, so only roster members reach them:

| Route | Answer |
| --- | --- |
| `GET /v1/sessions?after=REV&limit=N` | `{epoch, changes: [{rev, deleted, key, agent, title, ...}], more}`, in order of revision |
| `GET /v1/sessions/messages?key=KEY&after=ID&limit=N` | `{epoch, rev, messages: [{id, seq, role, time, text, sidechain, truncated}], more}`, in order |

- Message pages optionally carry `format: 1`. Messages optionally carry `kind`, `thinking`, `calls`, `usage` and `model`; session entries add `app`, `reader`, call/error counts, `tokens` and `files` when known. Calls additionally carry optional `files: [{path, op}]`, where `op` is `change`, `delete` or `read`. Old readers ignore them. Call input and output are each cut at 4 KiB on the wire and marked `truncated`, thinking at 32 KiB, and calls at 512 KiB of JSON per message, with `calls_dropped` reporting omitted calls. Local reads keep the source text and structure.
- An answer holds at most 500 items and about 1 MiB of JSON, whichever comes first, but always one item. Message text is cut at 200 000 characters and marked. The reader accepts answers up to 8 MiB.
- Source readers load full local messages while filling a page; the peer handler applies wire truncation afterwards. A large message can therefore occupy more memory than its transmitted form.
- Only sessions in the feed can be read. A key is a value in a query, never part of a path or of SQL.
- The reader checks that the answer came from the key the peer is known by. An address can pass from one node to another.

Every minute, and as soon as a peer becomes reachable, a node reads what is new in the feed of each peer that is online at a known address, and applies it to its copy: changed sessions are replaced, deleted ones removed together with their cached messages. A feed that does not move forward, or lists more than 200 000 sessions, is refused.

### Reading and the cache

A session of this machine is read straight from the index. A session of another node is read from a copy in `sessions.db`:

- The first time, the node fetches the whole session from its machine, page by page, and stores it. Later readings use the copy.
- The first schema migration republishes local entries once and resets remote feed cursors to replay metadata without dropping copies from the same epoch. Copies record the page format, with existing copies at format 0. An online node probes an older-format copy on reading; a probe that finds no newer structure is remembered for that session and revision. An offline copy retains its format and text. A copy spanning a revision change keeps the first revision and the lowest page format so later readings can refresh it.
- The copy records the revision it was made from. When the feed shows a newer revision, the next reading fetches the session again. The new copy replaces the old one only when it has been fetched to the end; if the fetch fails, the old copy is shown and marked as out of date.
- One copy of a session is made at a time. A second reader waits for the first one's copy and uses it. Whatever is fetched for a copy that is then not used is removed again.
- A copy belongs to the feed it was made from. If the node starts a new feed while a copy is being made, revisions mean something else: the copy is dropped, and the reader is asked to try again.
- When the other node is offline, the copy is shown with the time it was made. A session that was never read has no copy, and the reader is told so.
- A copy stops at 256 MiB, counted in whole messages, or after 2000 answers, and is then marked as not reaching the end. Such a copy is used as it is until the session changes.

Nothing is ever removed from the cache except what its machine deleted.

### Usage and files

All new fields are optional additions to protocol 1. Missing usage means unknown, not zero. With per-message usage, session `tokens` sums `input`, `output`, `cache_read` and `cache_write` separately across those messages. Without it, Wake's known `tokens_used` becomes `tokens: {"total": N}`; without either source the field is absent. Four NULL usage columns mean no message usage. Message usage retains the existing numeric categories; session totals preserve source nullability separately. A category whose values are all unknown is omitted from `tokens`, including cache-write usage absent from ACP and some Wake adapters. A known zero remains zero.

A call's `files` lists explicit file arguments, including reads and failed calls. Session `files` contains at most the first 500 distinct paths in non-error calls with `op: change` or `delete`. Empty output does not imply failure. Empty `files` is omitted: when `tool_calls` is present, no `files` means no changed file was identified. Wake resolves relative paths using the session cwd; when no cwd is available it retains the path. ACP follows the same rule for the observed file tools: `client_create_file` and `client_edit_file` (`target_file`), `write_to_file` and `replace_file_content` (`TargetFile`), `client_view_file` (`absolute_path`) and `view_file` (`AbsolutePath` or `absolute_path`). ACP decodes file argument values that themselves contain a JSON-encoded string before resolving the path. Shell commands are not interpreted as file operations.

The format marker and structure freshness stamp participate in the session fingerprint. A format-1 to format-2 upgrade and a later successful backfill move the feed revision even when the indexed message text is unchanged. Schema 2 also republishes local metadata once and replays remote metadata while retaining cached copies. `h8c sessions --json` and `h8c show --json` retain these fields; plain `h8c show` prints session totals and paths, and file operations beneath each call.

### Asking the node

On the control socket: `GET /v1/sessions` with `node`, `agent`, `project`, `since`, `text` (repeatable) and `limit` lists the sessions of all nodes, newest first, each with its node and whether that node is online, plus one entry per node with the time its sessions were last read. `GET /v1/sessions/messages` with `node`, `key`, `after` and `limit` reads a page of a session. `h8c sessions` and `h8c show` are these two calls; with `--json` they print the answer as it is, for programs. A session is named `NODE/KEY`, with the node's name where that is unambiguous and its id otherwise.

A session that cannot be read is answered with an error status and a sentence in English, which is what `h8c show` prints. For the reasons a program may want to put in its own words the answer also has the header `H8c-Error` with one word: `not-found`, `offline-not-read` (the node is offline and there is no copy), `incompatible-not-read` (it speaks another protocol and there is no copy) or `feed-restarted` (try again in a moment). The header is optional: a reason without a word has none, and a reader that does not know a word shows the sentence. The page uses it to give these reasons in the reader's language.

### Search and anchors

`h8c search [TERM...] --file PATH --op OP --failed --tool NAME --kind KIND --errors --agent AGENT --project PROJECT --node NODE --since WHEN --until WHEN --limit N --by-session --json` searches across nodes. Terms are literal substrings combined with AND within one field (body, thinking, or one call input). Call filters may match that call's input or its parent message's body/thinking. Agent, tool and kind use equality, project matches a name/path substring, and node accepts a unique name or id. Since/until are inclusive message-time bounds; missing message times use session update time. CLI durations (`30d`, `36h`), dates and RFC3339 times become milliseconds. A date-only `--until` includes the entire local calendar day, including daylight-saving boundaries.

File predicates run in SQL as exact paths or separator-aligned relative suffixes. Backslashes become slashes, repeated leading `./` is removed, and Windows drive letters fold case; the rest of a path remains case-sensitive. `store.go` cannot match `xstore.go`. By default only non-error change/delete calls qualify. `--op read|change|delete|any` selects operations and `--failed` includes failed attempts. `--errors` with `--file` automatically includes failed attempts. Empty output does not imply failure. No requested path is opened, and shell command text is not interpreted as file operations.

Wake validates the trigram external-content `messages_fts` DDL, then uses it for terms of three or more characters. Exactly two CJK characters use `messages_cjk` only with its expected DDL, `cjk_format=1`, and no `cjk_backfill` marker; otherwise they use the bounded LIKE path. Other short terms also use bounded LIKE. Every metadata, message-time and structural filter is applied in SQL before the bounded scan. Indexed body results are collected first. Thinking/full input scans and unindexed body scans then visit at most the newest 2,048 eligible messages, in batches, and report the oldest scanned timestamp when stopped. Body search covers Wake's indexed first 32 KiB; long `full_text` is not searched. Candidate message/call data is loaded in batches, with two queries per batch rather than per-message schema/side-table reads. Grouped Wake searches aggregate these bounded batches; grouped copies count hits with SQL GROUP BY.

ACP uses the published local session metadata snapshot to skip empty and out-of-window sessions before opening their databases, sorts sessions newest first and reads bounded pages. In particular, ACP's session update time must itself lie within the requested window, before individual message times are checked. No local message rows are persisted. The local feed's key allow-list is applied before either source limits its results; it also suppresses native ACP/Wake duplicates. An unpublished source key cannot consume the limit and hide a published match.

Copies retain the additive `search_docs`, trigram `search_fts` and `search_files` schema. Each body, thinking and call input contributes at most 4 KiB. Triggers maintain writes from both current and Phase 2 binaries. Empty text/thinking/input and null calls create no search document. An independent `meta.search_schema=2` marker upgrades these triggers and removes old empty documents once; `meta.schema` remains 2 and existing message/feed/export tables are unchanged. Search uses dedicated read-only connections and bounded batches, leaving the store's sole business connection available for feed/list/Lookup operations. The FTS rowid membership predicate deliberately uses unary `+` to prevent SQLite from selecting the quadratic lookup plan.

Each search request has a total server budget of ten seconds; CLI search waits twelve seconds so it can receive the server's explanation. Wake, ACP, copies and peer requests run concurrently. Each local source gets at most eight seconds and each peer five seconds. Copy queries start alongside peer requests; a live peer wins and cancels the speculative copy query. Offline, incompatible, unreachable, timed-out and 404 peers use held copies, or `none` if none exist. Notes never claim copies were searched when none were held. Protocol stays 1; hello is unchanged, and the peer route is the capability.

Time, candidate or scan exhaustion returns already collected hits/groups, `partial: true`, and a note explaining how to narrow the query. A node can answer `live` and still be partial. Source failures do not erase another source's results. The ten-second collector returns completed sources and marks outstanding nodes partial instead of replacing the whole answer with a timeout error. Groups on partial sources count only the examined part; zero hits on a partial source do not establish absence. `more` independently indicates result truncation by limit or page budget. Broad Wake queries commonly have partial side-table coverage even when indexed body search completed.

Missing structure marks partial only when a structural filter (file, tool, kind, errors) could be affected in the selected metadata/time window. For copies this means a relevant format-0 or incomplete copy; a complete text-only copy is sufficient for a text query. Empty ACP databases do not make a query partial. Wake file queries over relevant format-1/stale sessions are partial. A copies answer always covers only previously read sessions, even when not partial.

Limits remain 32 nonempty literal terms / 4096 term bytes, up to 500 results (default 50), 300 Unicode characters per snippet and the approximate 1 MiB page budget. Oversized input is rejected at the boundary. Indexed/grouped candidate processing and ACP scanning are capped at 10,000 messages, independently of Wake's 2,048-message side-scan cap. All SQL values are parameters; MATCH is built only from quoted phrases with embedded quotes doubled. Results are untrusted data. Hits sort by effective message time descending, then node id, key, message id and call index; groups sort by session update time descending, then node id/key. A body/thinking hit counts once, and a matching call counts separately. Grouping counts before limiting groups. There is no search pagination cursor; narrow the scope when partial or more is true.

`h8c show [NODE/]KEY --around ID --before N --after N [--rev R]` returns a window, default eight before and two after, at most 500 messages. Without a node, the store resolves the exact key: one node is read, several produce an error listing usable refs, and none produces a not-found sentence. The control/web messages APIs support the same resolution when `node` is omitted. No agent-key rewriting is inferred; callers convert a remembered source id to its exact indexed key themselves.

`rev` describes the data actually read: the local feed revision for local data, the response revision for a freshly fetched peer copy, or `copy_rev` for held data. `moved` compares the supplied revision to this value, never to a newer feed description of an older copy. A live search's newer anchor revision forces a peer refresh even when the reader's feed has not caught up. Missing anchor ids and a revision changing during the window read also set moved. A missing id centers on the next id; beyond the end, the last messages are returned, including when before is zero. `messages` is always an array. `has_before` on an around answer explicitly reports whether earlier messages precede the returned window, including context removed by the byte budget. No ten-second deadline is added to message reads or around: first reads of large remote sessions may finish copying. Around retains a strict 10,000-message traversal cap and returns the collected window with partial/note at the cap. CLI `--around 0` and `--rev` without `--around` are errors. Meta rows are hidden by CLI show unless `--all`; APIs retain them.

#### API for page and control clients

GET `/v1/sessions/search` on the control socket, `/api/v1/sessions/search` on the authenticated web API:

| Parameter | Meaning |
| --- | --- |
| `q` (repeatable) | One literal term/phrase per value; omitted for filters-only queries. |
| `file`, `op`, `failed` | Recorded path, operation, include failed attempts. `error=true` with file implies failed. |
| `tool`, `kind`, `error` | Exact tool/kind; failed calls only. CLI `--errors` maps to error. |
| `agent`, `project`, `node` | Agent equality, project substring, node name/id. No node searches all known nodes. |
| `since`, `until` | Optional inclusive Unix millisecond timestamps; CLI converts date-only until to day end. |
| `limit`, `by_session` | Default 50, maximum 500 hits/groups; grouping boolean, default false. |

Response: `{hits: [], sessions?: [], nodes: [], more: boolean}`. Grouped mode returns `hits: []` and sessions (omitted when empty).

- Hit fields: `ref`, `key`, `node`, `node_id`, `id`, `rev`, `time`, `snippet`, `title`, `project`, `updated_at`; optional `agent`, `project_path`, `kind`, `tool`, `call` (zero-based position in the message calls array), `files: [{path, op}]`. Times are Unix milliseconds.
- Group fields: `ref`, `key`, `node`, `node_id`, `title`, `project`, `rev`, `hits`, `first`, `last`, `updated_at`.
- Node fields: `id`, `name`, `searched` (`live|copies|none`), optional `partial: true`, English `note`. Treat zero hits with partial as incomplete evidence. Invalid parameters return HTTP 400.

The mutual-TLS peer route searches only local sources and returns `{hits: [], sessions?: [], more, partial?, note?}`, without fan-out. It ignores node. Unsupported routes may return 404, triggering copies at the caller.

GET `/v1/sessions/messages` (control) or `/api/v1/sessions/messages` (web): `key`, optional `node`, optional `around` (positive id), `before` (default 8), `after` (default 2 with around; otherwise exclusive cursor), optional `rev`. With around, before/after are nonnegative, sum plus one at most 500, and limit is ignored. Without around, the existing limit/paging behavior remains. Response fields: `session`, `messages`, `rev`, `copy_rev?`, `moved?`, `has_before` (meaningful for around), `partial?`, `format?`, `next`, `from` (`local|peer|cache`), `cached_at?`, `stale?`, `incomplete?`, `note?`. `session`/`messages` retain the existing Entry/Message fields, including calls, thinking, usage and files. `next` continues an ordinary page; zero means no continuation. The peer messages protocol is unchanged.

Control connection failures distinguish absence (`ENOENT`, `ECONNREFUSED`: "the node is not running") from denial (`EPERM`, `EACCES`: "control socket access denied; a sandbox may be blocking it"). Other dial failures preserve their cause. A denied socket is not evidence that h8c is stopped.

### Local export

`GET /v1/sessions/export?epoch=E&after=N&limit=N&ack=N&since=W` exists only on the private control socket. It is not registered on the peer server or the web server. The CLI equivalent is `h8c export --epoch E --after N --since W [--ack N] [--limit N] --json`. `since` is an absolute Unix millisecond timestamp, not a rolling duration. It is optional; omitting it on the first request includes all local sessions.

The first request saves its watermark and the maximum local feed revision (`initial_rev`) in `sessions.db`. Unscanned sessions at or below that revision whose `updated_at` is before W get a baseline but no snapshots. This cutoff survives failures and restarts. After reading an initially excluded session, a message timestamp at or after W makes it eligible immediately: the source may have advanced beyond the feed snapshot during initialization. Sessions discovered later are included even with older timestamps. Later requests retain the first watermark. Before initialization, normal refreshes publish the feed but do not build an export history.

After initialization, each local refresh scans sessions whose feed revision has advanced. It appends a session snapshot for each changed entry, including title, span, counts, tokens, files and deletion, then a snapshot for each new or changed message. For a baseline session never emitted, an unchanged set of message fingerprints stays a baseline even if its stamp or metadata changes, including title and archived state. A downgrade alone is not a content change. An actual message change emits its whole current contents: if some historical seqs are currently degraded, their available text is included in this first emission and their lower structure levels are recorded, so recovery later emits their full structure. For a session already emitted, a stamp-only revision can emit a session item. Message fingerprints exclude the index's paging ID. Reading the export serves immutable snapshots; replacing an index row does not change an earlier item. Only this machine's source rows are included, never cached peer rows.

A temporary loss of source structure must not replace an already archived full message with a lower-quality version. Local readers and `export_messages` track structure levels: 0 is text-only, 1 is Wake format 1, and 2 is Wake format 2 or an ACP transcript with usage/file support. These levels are local bookkeeping, not new protocol fields. For an already-emitted session, a seq now below its recorded level retains its existing identity and fingerprint and is not emitted again until recovery; new seqs are still exported. A never-emitted baseline keeps its richer identities unless a real change triggers the complete first emission described above. The session's stored level protects summaries during degradation, including zero-message sessions and after ack prunes the log. At level 1, current `tool_calls` and `tool_errors` remain available while previous `tokens` and `files` are retained; at level 0 all four fields are retained. The stored level remains the previous higher level. Renumbering of an already-emitted session during degradation waits for recovery, so its new generation can be sent whole.

The database schema number advances once to 2. `export_log` holds snapshots with an AUTOINCREMENT cursor; `export_scan` holds each session's last scanned revision, generation, emission status, session snapshot and structure level; `export_messages` retains message identity, fingerprints and structure level. Source reads, comparison and snapshot serialization happen without holding the store connection. Plans commit in batches of at most 100 sessions or approximately 1 MiB of snapshots and baseline records; one larger session is kept whole. Each session uses a savepoint inside the batch, so its appends and scan state remain atomic and a failure rolls back only that session. A transaction-wide failure, such as disk exhaustion, an I/O error or a failed commit, rolls back the entire batch; every session in that batch is retried next round. The connection is released between batches. The next local refresh retries failed sessions even if the feed revision is already current. Export failures are logged separately and do not mark the local feed as unshared. Cancellation stops scanning the remaining sessions; already committed batches stay, and an unfinished batch rolls back. Memory holds the current source session plus the prepared batch, not every source session's text.

The response is `{epoch, items, next, more}`. `after` is exclusive. `next` is the last delivered cursor, or the effective `after` on an empty page; it remains nonzero at the end. On a new store's first request, or an epoch mismatch, an empty page has `next: 0`. `limit` defaults to 100 and is capped at 500. The usual approximate 1 MiB page budget applies, but one complete item is always returned even if it exceeds that budget. Message text and structured fields are not cut by export; source-level cuts still apply.

Every item has `cursor`, `type`, `key`, `agent`, `g`, `seq` and `rev`. A `session` item has a nested `session` snapshot and `seq: 0`; a `message` item has a nested `message` snapshot and its original `seq`. `deleted: true` appears only on session tombstones. Session/message times are Unix milliseconds. `rev` is the local feed revision at scanning, not the export cursor. The message's `id` is only its source paging cursor; consumers identify messages by `(key, g, seq)`.

Generation `g` begins at 0. If a previously observed seq now has a different role or time, the generation increases once and the entire current session is emitted again. The session item identifies the new generation. Old-generation log items remain unchanged until acknowledged; consumers keep their old-generation records. A changed body with the same role and time is an update in the current generation.

`ack=N` deletes retained snapshots at or below N in this epoch, keeping scan baselines and the monotonic cursor. Send it only after the destination has durably accepted those items. There is one acknowledgement stream per store, not separate retention for multiple collectors. Send the response's epoch with subsequent requests as `epoch=E` (CLI `--epoch E`). If it differs from the store epoch, the server treats `after` as 0 and ignores ack. This also protects against a lost first response or a manual export that initialized the new store before the collector contacted it. Without an epoch, requests retain the original behavior: only the first request of a new store ignores after/ack. The response always carries the current epoch; collectors reset their saved cursor when it changes.

Deployment order across the five machines: upgrade h8c to Phase 2 everywhere; then upgrade the Wake fork to format 2 and finish its backfill everywhere; only then initialize each machine's export. An old h8c treats format 2 as unsupported and reads the whole index as text-only, and copies it made lack call files. Initializing export after backfill also keeps the one-time addition of structure from appearing as new historical content in baseline sessions. Rolling Wake back to format 1 removes per-message usage and call files from live reads and newly fetched peer copies. Export treats that as a level-2 to level-1 downgrade and preserves already-emitted richer snapshots and session summaries. New rows and a baseline's first complete emission use what the rolled-back source can provide, then receive fuller updates after format 2 is restored.

A complete example page (all values are fictional):

```json
{
  "epoch": "0123456789abcdef",
  "items": [
    {
      "cursor": 1, "type": "session", "key": "codex:example", "agent": "codex",
      "g": 0, "seq": 0, "rev": 1,
      "session": {
        "key": "codex:example", "agent": "codex", "title": "Fix the parser",
        "project": "parser", "project_path": "/home/alice/src/parser",
        "created_at": 1791803500000, "updated_at": 1791803600000, "messages": 1,
        "app": "terminal", "reader": "wake", "tool_calls": 1, "tool_errors": 0,
        "tokens": {"input": 7, "output": 5, "cache_read": 3, "cache_write": 0},
        "files": ["/home/alice/src/parser/main.go"]
      }
    },
    {
      "cursor": 2, "type": "message", "key": "codex:example", "agent": "codex",
      "g": 0, "seq": 1, "rev": 1,
      "message": {
        "id": 1, "seq": 1, "role": "assistant", "time": 1791803600000,
        "text": "Fixed the last token.", "kind": "text", "thinking": "Check the loop boundary.",
        "model": "model-x",
        "usage": {"input": 7, "output": 5, "cache_read": 3, "cache_write": 0},
        "calls": [{"call": "edit-1", "tool": "edit", "input": "{}", "output": "done",
          "files": [{"path": "/home/alice/src/parser/main.go", "op": "change"}]}]
      }
    }
  ],
  "next": 2,
  "more": false
}
```

A later deletion emits a session item with `deleted: true`, the deletion revision and the latest `g`. When a previous scan snapshot exists, its last metadata is retained, including title, project, project path, app, creation/update times, message and call counts, tokens and files; ack does not remove this saved snapshot. A session deleted before any scan can only carry the feed tombstone available at initialization. No message snapshots are deleted except through ack. `gone` and `forget` remain a later phase.

## Web interface

The node serves a page for a browser on the same machine (`internal/web`): the machines, the list of sessions, a session to read. It is read-only and it is the command line's twin: the page calls `GET /api/v1/sessions` and `GET /api/v1/sessions/messages`, which are the handlers of the control socket's routes of the same name, so the page and `h8c sessions`, `h8c show` get the same answers from the same code. Nothing else of the control socket is offered: no status, no peer list, nothing that writes. The files of the page are plain HTML, CSS and JavaScript, embedded in the binary; there is no build step and nothing is loaded from another server.

`h8c up --web ADDR` sets the address, by default `127.0.0.1:4893`; `--web none` turns the page off. The host is `127.0.0.1`, `localhost` or `[::1]`, the names the Host check below knows; anything else is an error at start, also another loopback address such as `127.0.0.2`, where the page would listen and refuse every request. A port that is taken costs the page, not the node.

A list of sessions is continued with a cursor: an answer that is not the last one carries `next`, and passing it as `after` gives the following page. The cursor is the position of the last session in the order of the list (update time, node, key), so a walk shows each session once. The same parameter exists on the control socket and as `h8c sessions --after`.

### Who can read it

What the page shows is private. A port on 127.0.0.1 is reachable by every program of the machine, and by every web page open in a browser on it. So:

- **The Host header must name this server by a loopback address** (`127.0.0.1`, `localhost` or `[::1]`, with its port). A page that reaches the port under a name of its own, as with DNS rebinding, is refused.
- **Every request for data must carry the token**, 32 random bytes kept in `web.token` in the data directory, mode 600, made on first use. `h8c web` prints `http://127.0.0.1:4893/?token=...`. The server answers that address by setting a cookie (`HttpOnly`, `SameSite=Strict`, valid a month) and redirecting to `/`, so the token leaves the address bar and the page's script never sees it. A program may send it as `Authorization: Bearer` instead. Without it the page is a note saying to run `h8c web`, in Chinese if the browser's `Accept-Language` prefers that to English, and the API answers 401.
- **No answer allows another origin to read it**: no CORS header is ever sent, and `Cross-Origin-Resource-Policy: same-origin` keeps other sites from embedding the answers. With `SameSite=Strict` the browser does not even send the cookie on a request another site started.
- The token is never logged, and `Referrer-Policy: no-referrer` keeps addresses out of Referer headers. Answers are `Cache-Control: no-store`.
- Only GET is answered. Every other method, HEAD too, gets 405.

### What it shows cannot be trusted

Titles, project names, message text and node names come from session files and from other nodes. They may contain anything, markup and script included.

- The page's script writes every such value as text (`textContent`), never as markup. It builds two things from message text, both as elements: fenced code blocks, and links for addresses that parse as `http:` or `https:`, opened in a new tab with `noopener noreferrer`.
- The `Content-Security-Policy` is `default-src 'none'` with only this server's own script, style and requests allowed: no inline script or style, no other origin, no frames (`frame-ancestors 'none'`, `X-Frame-Options: DENY`), no forms, no `<base>`. If markup did get into the page, it could not run or call out.
- `X-Content-Type-Options: nosniff` on everything.

A test keeps the script free of the calls that turn strings into markup. What the script does in a browser is checked by `go run ./scripts/websmoke`, which is not part of `go test`: it opens the page of a demo node in headless Chrome and checks that a session whose title, project, text and node name try to run script shows all of it as text, that "load more" is gone at the end of the list and of a session and nothing is shown twice, that an answer which arrives late does not bring back the place in a list that is no longer asked for, and that a narrow window brings a session that cannot be read into view with its title and, in a Chinese browser, the reason in Chinese. With `-shots DIR` it writes pictures of the page, narrow and wide, English and Chinese, to look at after a change of the layout. `go run ./scripts/demo` starts the same demo to look at: three made-up machines on loopback, one of them offline.

## Control interface

The running node listens on the Unix socket `control.sock` in the data directory (mode 600) and speaks HTTP: `GET /v1/status`, `GET /v1/peers`, `POST /v1/refresh` (re-read the local roster, answer, then run a round) and `POST /v1/roster/refresh` (fetch the GitHub roster, answer, then run a round). `h8c peers`, `h8c trust` and `h8c roster refresh` use it; the MCP server and desktop client will too.

One node runs per data directory. `h8c up` takes the lock file `lock` (flock, `LockFileEx` on Windows) and holds it while it runs; a second `h8c up` on the same directory exits. A socket left behind by a crash is removed by the next node, after it has the lock.

`control.Listen` and `control.Dial` are the only places that know the transport. Go supports Unix sockets on Windows 10 and later, so the same code is used there, and it works on Windows 11.

## Data directory

`--data-dir`, default `hebi8xc` under `os.UserConfigDir()`: `~/.config/hebi8xc` on Linux, `~/Library/Application Support/hebi8xc` on macOS, `%AppData%\hebi8xc` on Windows. Every command except `h8c peers` sets the directory to mode 700 every time, also when it already existed.

| File | Content |
| --- | --- |
| `node.json`, `node.key` | Identity |
| `trusted_keys` | Local roster |
| `account.json` | GitHub account the node is logged in to: id and login |
| `github_keys.json` | Cache of the GitHub roster |
| `peers.json` | Node list |
| `sessions.db` | Session feed of this node, copies of the others' feeds, cached messages and local export snapshots/baselines (SQLite, mode 600) |
| `web.token` | What opens the web interface (mode 600) |
| `control.sock` | Control socket, while the node runs |
| `lock` | Held by the running node |

## Known limits

- Search sees only previously read copies when a peer cannot answer live. Copies index 4 KiB per field; Wake body search covers the indexed 32 KiB. Broad queries retain partial results at time/work bounds; missing structure only affects relevant structural queries. Around compares the actual read revision, not a newer feed description. Search UI and the agent skill are separate Phase 3 work.

- Export: rebuilding a lost store restarts numbering generations at `g=0`. If a session had previously been renumbered, it is exported again under generation-0 keys and can overwrite older generation-0 rows at the collector. This requires both loss of the store and a previously renumbered session; epoch-bound acknowledgements do not preserve generation history.
- Sessions: Wake omits Claude Code user inputs queued while the agent is busy, so h8c and its export omit them too. A lifelog comparison found six such messages across four devices over one week.

- mDNS uses one interface and IPv4, and announces the address it had at start.
- The Tailscale source only tries port 4892.
- A peer that comes back is seen at once when it can reach us, because it says hello. When only we can reach it, its address may still be backing off, for up to 5 minutes.
- Any key in the roster may claim any node id, and any number of them: the node list has no size limit. All keys belong to the same user, so this is not a boundary today.
- Two `h8c trust` commands at the same moment are not coordinated; the later write wins.
- A node only notices a login made while it runs after a restart.
- The token of a login is not revoked when `h8c login` is done; it expires after 8 hours.
- A first start that races another one may fail once with an incomplete identity and works when run again.
- A refusal is recognized by the TLS alert the other node sends. If the alert is lost on the way, the failure counts as a broken connection and the "trusted but refused" hint is not logged. On Linux the alert arrived in every test.
- Sessions: the cache only grows. Sessions and messages copied from a node stay until that node deletes them, also when the node leaves the roster.
- Sessions: a node that cannot be called, such as one behind a firewall that only lets it call out, cannot share its sessions. It can still read the others'.
- Sessions: news travels with a delay. A change reaches the index within 2 minutes when `wake-cli` does the refreshing, the feed within another minute, and a peer within one more.
- Sessions: the first reading of a long session copies all of it before anything is shown. Message text beyond 200 000 characters is not transferred.
- Sessions: the marks for deleted sessions stay in the feed for good.
- Sessions: an answer from a peer is limited by size, 8 MiB. That it holds at most 500 items is checked after it has been decoded, so a node that set out to could make a peer allocate far more memory than that. Nodes in the roster are the user's own and are trusted not to.
- Sessions: ACP uses an undocumented local database format. A changed schema or malformed protobuf retains that file's last successful metadata until it can be read again or is deleted; a file never read successfully is skipped. Native titles and attachments are not included; structure requires the optional transcript; titles are derived from the first user message.
- Sessions: a newer Wake may change its database. Then Wake's last successful list is kept while ACP updates; without a prior successful Wake listing, local listing fails until the reader is updated.
- Web: a cookie belongs to a host, not to a port. Another program that serves http on 127.0.0.1 and that this browser is pointed at gets the cookie sent along and could use it against the page's port. Processes of the same user can read the token file anyway; this matters for a server run by another user of the machine.
- Web: the link from `h8c web` holds the token and stays in the browser's history. The token only changes when `web.token` is deleted and the node restarted; there is no log-out.
- Web: a session that is updated while a list is being walked moves to the top and is not met again further down; the next refresh shows it.
- Web: plain http. Fine on loopback, and the reason the page is not offered to other devices.
- On Windows the private key file relies on the profile directory's ACL; there are no mode bits.
- On Windows 11 `h8c id`, `login`, `up`, `peers` and `roster` have been run, with the control socket and the directory lock. mDNS has not been seen working there yet, and inbound connections need firewall rules that `h8c` does not create; see the README.

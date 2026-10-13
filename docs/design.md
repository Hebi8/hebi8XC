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
- Within a version the only change allowed is a new field that is optional. A node must do without any field its peer does not send, and ignores fields it does not know. Removing a field, renaming it, requiring a field that used to be optional or was not there, and changing what a field means are not allowed.
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

`h8c` does not parse session files. [Wake](https://github.com/iAmCorey/Wake) does that for some twenty agents and keeps the result in a SQLite database. `internal/sessions/wake` reads it:

- It opens the database read-only, with a busy timeout, the way Wake's own side tools do. A reader does not block Wake's writes. It never changes the journal mode and never writes.
- The database is Wake's internal format. Everything that depends on it is in that one package, written against Wake v0.9.2. Before reading, it checks that the tables and columns it uses exist. If not, it reports that it cannot make out the structure of the index, and this machine's sessions are not shared; the rest of the node is unaffected, and other machines' sessions are still listed. It looks again every round, so a later repair is picked up.
- Sessions that Wake mirrored from other hosts (`host` not empty) are left out. They belong to another machine.
- What it reads of a session: key (`<agent>:<id>`), agent, title, project name and path, branch, model, creation and update time, message count, parent session, whether an agent spawned it, whether it is archived. Of a message: Wake's text for it, role, time, number, side thread. This is the text Wake indexes, not a rendering of the session file.

Where `wake-cli` is found, on PATH or through `--wake-cli`, the node runs `wake-cli refresh` every 2 minutes, and `wake-cli index` first if there is no index. While the Wake app runs, both commands leave the index alone and succeed, because the app keeps it current. A `refresh` that takes more than 2 minutes, or an `index` that takes more than 10, is stopped; the round goes on with the index as it is and with the peers. Without `wake-cli` an existing index is read as it is. The index is taken from Wake's default location unless `--wake-db` names another, and it does not have to exist when the node starts: it is looked for every round.

The rest of the module only sees an interface, `sessions.Source`: list the sessions of this machine, read the messages of one. Wake is its first implementation.

### The feed

Wake's index has no counter that says what changed since when; a session can change in ways its update time does not show, and old sessions can appear late. So the node keeps its own record: the **feed**, in `sessions.db` in the data directory. Every minute it compares the index with the feed. A session that is new or different gets the next **revision** number; one that is gone keeps its key and gets a revision too, as a mark that it was deleted. An index that cannot be read changes nothing: it is not an index without sessions.

A peer reads the feed with a cursor: "everything above revision N". It therefore gets every change once, deletions included, and nothing has to be compared or guessed. The feed has an **epoch**, a random id made with the store. A node whose store was lost starts a new feed; its peers see the new epoch, drop what they had from it and read from the start. That is the only time a feed is read again in full.

### Between nodes

Two routes on the peer link, so only roster members reach them:

| Route | Answer |
| --- | --- |
| `GET /v1/sessions?after=REV&limit=N` | `{epoch, changes: [{rev, deleted, key, agent, title, ...}], more}`, in order of revision |
| `GET /v1/sessions/messages?key=KEY&after=ID&limit=N` | `{epoch, rev, messages: [{id, seq, role, time, text, sidechain, truncated}], more}`, in order |

- An answer holds at most 500 items and about 1 MiB of JSON, whichever comes first, but always one item. Message text is cut at 200 000 characters and marked. The reader accepts answers up to 8 MiB.
- A page of messages is cut while it is read from the index. The node that answers never holds more of a session in memory than it sends.
- Only sessions in the feed can be read. A key is a value in a query, never part of a path or of SQL.
- The reader checks that the answer came from the key the peer is known by. An address can pass from one node to another.

Every minute, and as soon as a peer becomes reachable, a node reads what is new in the feed of each peer that is online at a known address, and applies it to its copy: changed sessions are replaced, deleted ones removed together with their cached messages. A feed that does not move forward, or lists more than 200 000 sessions, is refused.

### Reading and the cache

A session of this machine is read straight from the index. A session of another node is read from a copy in `sessions.db`:

- The first time, the node fetches the whole session from its machine, page by page, and stores it. Later readings use the copy.
- The copy records the revision it was made from. When the feed shows a newer revision, the next reading fetches the session again. The new copy replaces the old one only when it has been fetched to the end; if the fetch fails, the old copy is shown and marked as out of date.
- One copy of a session is made at a time. A second reader waits for the first one's copy and uses it. Whatever is fetched for a copy that is then not used is removed again.
- A copy belongs to the feed it was made from. If the node starts a new feed while a copy is being made, revisions mean something else: the copy is dropped, and the reader is asked to try again.
- When the other node is offline, the copy is shown with the time it was made. A session that was never read has no copy, and the reader is told so.
- A copy stops at 256 MiB, counted in whole messages, or after 2000 answers, and is then marked as not reaching the end. Such a copy is used as it is until the session changes.

Nothing is ever removed from the cache except what its machine deleted.

### Asking the node

On the control socket: `GET /v1/sessions` with `node`, `agent`, `project`, `since`, `text` (repeatable) and `limit` lists the sessions of all nodes, newest first, each with its node and whether that node is online, plus one entry per node with the time its sessions were last read. `GET /v1/sessions/messages` with `node`, `key`, `after` and `limit` reads a page of a session. `h8c sessions` and `h8c show` are these two calls; with `--json` they print the answer as it is, for programs. A session is named `NODE/KEY`, with the node's name where that is unambiguous and its id otherwise.

A session that cannot be read is answered with an error status and a sentence in English, which is what `h8c show` prints. For the reasons a program may want to put in its own words the answer also has the header `H8c-Error` with one word: `not-found`, `offline-not-read` (the node is offline and there is no copy), `incompatible-not-read` (it speaks another protocol and there is no copy) or `feed-restarted` (try again in a moment). The header is optional: a reason without a word has none, and a reader that does not know a word shows the sentence. The page uses it to give these reasons in the reader's language.

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
| `sessions.db` | Session feed of this node, copies of the others' feeds, cached messages (SQLite, mode 600) |
| `web.token` | What opens the web interface (mode 600) |
| `control.sock` | Control socket, while the node runs |
| `lock` | Held by the running node |

## Known limits

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
- Sessions: a newer Wake may change its database. Then this machine's sessions stop being shared until the reader is updated.
- Web: a cookie belongs to a host, not to a port. Another program that serves http on 127.0.0.1 and that this browser is pointed at gets the cookie sent along and could use it against the page's port. Processes of the same user can read the token file anyway; this matters for a server run by another user of the machine.
- Web: the link from `h8c web` holds the token and stays in the browser's history. The token only changes when `web.token` is deleted and the node restarted; there is no log-out.
- Web: a session that is updated while a list is being walked moves to the top and is not met again further down; the next refresh shows it.
- Web: plain http. Fine on loopback, and the reason the page is not offered to other devices.
- On Windows the private key file relies on the profile directory's ACL; there are no mode bits.
- On Windows 11 `h8c id`, `login`, `up`, `peers` and `roster` have been run, with the control socket and the directory lock. mDNS has not been seen working there yet, and inbound connections need firewall rules that `h8c` does not create; see the README.

# Design

hebi8XC connects the machines of one user. Each machine runs a node. There is no hub and no server: nodes connect pairwise.

This document covers discovery, the handshake, the roster and the node list. Session indexing comes later as routes on the same connection.

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

The only route so far is `POST /v1/hello`: both sides send `{id, name, port}`. A new service is a new route on the same mux; the handler gets the caller's key from `link.PeerKey`.

Default port: **4892** TCP (`h8xc` on a phone keypad).

## Node list

One entry per node id, stored in `peers.json`: id, name, public key, known addresses (each with the sources that suggested it and when it last worked), all sources, time of last contact, online or offline.

- A discovery round runs every 10 s: collect candidates, send hello to each address that is due, at most 16 at a time, record the answers. The round is also the heartbeat.
- An address that does not answer is probed less often: the gap doubles from 20 s up to 5 min. An answer from it, or a hello from the peer behind it, puts it back on every round. A node that answers but fails the trust check stays on every round.
- A hello received from a peer counts as contact too, so a node that can only dial out still shows as online.
- A peer with no contact for 30 s is marked offline and kept. The next contact brings it back.
- The same node found through several sources or addresses is one entry. At most 8 addresses are kept per peer.
- Online state is not stored; after a restart every peer is offline until contacted.

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
- On Windows the private key file relies on the profile directory's ACL; there are no mode bits.
- On Windows 11 `h8c id`, `login`, `up`, `peers` and `roster` have been run, with the control socket and the directory lock. mDNS has not been seen working there yet, and inbound connections need firewall rules that `h8c` does not create; see the README.

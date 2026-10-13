# Where it stands and what could come next

Written on 2026-10-14. How things work is in [design.md](design.md); this page says what exists, what has been tried for real, and what is left.

## What exists

- **Nodes.** Each machine has a node with a fixed id, a name (`h8c name`) and an Ed25519 key.
- **Same user = same GitHub account.** `h8c login` registers the node's key on the account as an SSH signing key titled `hebi8xc: <name> (<id>)`. Nodes read the account's public key list, keep a cache of it and need no token afterwards. Keys can also be trusted by hand for machines without internet access.
- **Discovery.** mDNS on the LAN, the Tailscale peer list, remembered addresses and `--peer`. Addresses that do not answer are asked less often.
- **Connection.** Nodes connect directly over mutual TLS 1.3 and accept only keys from the roster. No hub.
- **Protocol compatibility.** Builds of the same protocol version work together, so machines are updated one at a time. Samples of the wire format are kept as tests.
- **Sessions.** A node reads the index that [Wake](https://github.com/iAmCorey/Wake) keeps of the agent sessions on its machine and keeps it fresh with `wake-cli`. Nodes exchange their lists; the text of a session is fetched when someone reads it and kept as a copy. A machine that is off is still listed, and what was read before can still be read.
- **Three ways to use it.** `h8c sessions` and `h8c show`; `--json` on both for programs; a read-only page in the browser (`h8c web`).
- **Platforms.** Linux, macOS and Windows, as a service on each: see [running-as-a-service.md](running-as-a-service.md).
- **Tools for working on it.** `go run ./scripts/demo` and `go run ./scripts/websmoke`, described in `AGENTS.md`.

## What has been run for real

Four machines of one user (two Linux, one macOS, one Windows) have run it as services since 2026-10-13, about 6500 sessions between them. Checked on them:

- logging in on each machine and finding each other with no key added by hand, over mDNS and over Tailscale;
- a node going away and coming back;
- listing and reading sessions in every direction, and reading a copy while the other machine was off;
- a new build on one machine against the previous build on another;
- the page in a browser on macOS.

Not yet tried on a real machine: the firewall commands in the README's Windows section, removing a machine by deleting its key, and a machine that only has hand-trusted keys.

## What could come next

Roughly in the order that seems most useful. None of it is started.

1. **Search in the text of sessions, across machines.** Today the search matches titles and project names only. Each machine's Wake index already holds the full text, so this is a query sent to every node and the answers merged: a new route between nodes (an addition to protocol 1), `h8c search`, and the search box of the page. To work out: how to query Wake's full-text tables safely, and what to do about machines that are off (only their copies can be searched).
2. **An MCP server for agents.** `h8c mcp`, read-only, with list, search and read over the whole net, so an agent on one machine can pick up what was done on another. What it returns comes from session text and must be treated as untrusted.
3. **A client inside an agent app.** An app such as Lody could list sessions from the local node (`h8c sessions --json`, or the page's API) instead of importing them project by project. Continuing a session would still have to happen on the machine that holds it.
4. **Reading from a phone or another device.** The page is for the browser on the same machine. Serving it to other devices needs its own answer to who may read.
5. **Releases.** A version tag, built binaries, and `h8c service install` in place of the hand-written service files. Until there is a tag, builds are named by their commit.
6. **More of a session.** The text shown is what Wake indexed; tool calls and other details are thinner than in the agent's own files.
7. **Sessions that Wake on the host does not see**, such as those inside WSL or containers.
8. **Continuing a session on its machine**, and handing work from one machine to another. This means driving agents, not only reading what they wrote, and is a larger step.
9. **More than one user.** Out of scope so far: every node in the roster is the same person and is trusted fully. Sharing between people needs per-peer permissions, which nothing here has.

## Small things left open

- A new node refuses peers for the few seconds before its first roster fetch finishes; they log one line and recover on the next round.
- Whether Windows needs the UDP 5353 firewall rule for mDNS has not been checked.
- Copies of sessions are never removed from a node's cache.
- The link from `h8c web` stays in the browser's history, and there is no way to log a browser out short of deleting `web.token`.
- The Wake adapter is written against v0.9.2. A newer Wake may change its index; the node then stops sharing its own sessions and says so, and the adapter needs a look.
- Reading the roster uses GitHub's allowance for requests without a token, 60 an hour per public address, shared by every machine behind the same router. A node uses 8 an hour.

The longer list is under "Known limits" in [design.md](design.md#known-limits).

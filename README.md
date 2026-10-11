# hebi8XC

A LAN-first network of one user's own machines, with no central server. Every machine runs a node; nodes find each other, prove they belong to the same user and connect directly. Later each node will share what it knows about the AI coding agent sessions on its machine.

This first step does discovery, the handshake and the node list. The command is `h8c`.

## Build

Needs Go 1.27 or newer. No cgo.

```sh
go build -o h8c ./cmd/h8c

# for another machine
GOOS=darwin  GOARCH=arm64 go build -o dist/h8c-darwin-arm64 ./cmd/h8c
GOOS=windows GOARCH=amd64 go build -o dist/h8c.exe ./cmd/h8c
```

## Run it on two machines

Nodes trust each other through a roster of public keys. For now the roster is a local file, so each machine has to be told the other's key once.

```sh
# on each machine: print the node's id, name and public key
h8c id

# on each machine: trust the "key" line the other machine printed
h8c trust add ssh-ed25519 AAAA... othermachine

# on each machine: run the node in the foreground
h8c up

# from another terminal
h8c peers
```

```
NAME   ID                STATE   LAST SEEN            VIA                    ADDRESSES
n100   8c8a147501a15d27  online  2026-10-11 22:20:06  inbound,mdns,tailscale 100.64.0.2:4892 192.168.1.20:4892
```

Nodes on the same LAN find each other over mDNS, nodes in the same Tailscale network through `tailscale status`. Where neither works, name the other node: `h8c up --peer host[:port]`. One side knowing the other is enough.

Trust has to be mutual. If only one side has the other's key, `h8c up` on both sides logs what is missing. `h8c trust add` and `remove` take effect on a running node at once.

Things to know:

- The peer link listens on TCP port **4892** on all interfaces (`--listen` changes it). Tailscale discovery only probes 4892.
- mDNS only matches nodes with the same user id. It defaults to the OS user name; if your machines use different names, pass the same `--user ID` everywhere.
- State lives in the data directory (`--data-dir`, default `hebi8xc` under the OS config directory, e.g. `~/.config/hebi8xc`). The node name defaults to the host name and can be edited in `node.json`.

More in [docs/design.md](docs/design.md). The plan for replacing the local roster with GitHub is in [docs/github-roster.md](docs/github-roster.md).

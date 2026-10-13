# hebi8XC

A LAN-first network of one user's own machines, with no central server. Every machine runs a node; nodes find each other, prove they belong to the same user and connect directly. Later each node will share what it knows about the AI coding agent sessions on its machine.

So far it does discovery, the handshake and the node list. "The same user" means the same GitHub account. The command is `h8c`.

## Install

Needs Go 1.27 or newer. No cgo.

```sh
go install github.com/Hebi8/hebi8XC/cmd/h8c@latest
```

Or from a checkout:

```sh
go build -o h8c ./cmd/h8c

# for another machine
GOOS=darwin  GOARCH=arm64 go build -o dist/h8c-darwin-arm64 ./cmd/h8c
GOOS=windows GOARCH=amd64 go build -o dist/h8c.exe ./cmd/h8c
```

## Run it on two machines

On each machine, once:

```sh
h8c name laptop   # optional: the node's name, the host name by default
h8c login
```

`h8c login` prints a code to enter at `https://github.com/login/device`, then registers the node's public key on your GitHub account as an SSH signing key titled `hebi8xc: <name> (<id>)`. A signing key cannot log in to GitHub or push; it only says "this key is mine". No token is stored.

The signing keys of an account, titles included, are public. Pick the name before logging in if the host name should not be seen. Renaming later does not change the title on GitHub: delete the key there and log in again.

Then, on each machine:

```sh
h8c up          # runs the node in the foreground

# from another terminal
h8c peers
```

```
NAME     ID                STATE   LAST SEEN            VIA                     ADDRESSES
desktop  0123456789abcdef  online  2026-10-11 22:20:06  inbound,mdns,tailscale  100.64.0.2:4892 192.168.1.20:4892
```

Nodes on the same LAN find each other over mDNS, nodes in the same Tailscale network through `tailscale status`. Where neither works, name the other node: `h8c up --peer host[:port]`. One side knowing the other is enough.

A running node reads the account's node keys from GitHub at start and every 15 minutes. So a machine that logs in later is accepted by the ones already running within 15 minutes, or at once after `h8c roster refresh` on them.

```sh
h8c roster           # the trusted keys and where each comes from
h8c roster refresh   # fetch the GitHub list now
h8c id               # this node's id, name, key and account
```

### Removing a machine

Delete its `hebi8xc: ...` key at <https://github.com/settings/keys>, then run `h8c roster refresh` on the other machines, or wait for their next refresh. GitHub keeps listing a deleted key for a while, in a test for more than 20 seconds, so repeat the refresh if the key is still shown. There is no `h8c logout`.

### A machine without internet access

Keys can also be trusted by hand. A node accepts a key that is on the GitHub account or in its local list.

```sh
h8c id                                             # on the machine to add: copy the "key" line
h8c trust add ssh-ed25519 AAAA... othermachine     # on the others, and the other way round
```

Trust has to be mutual. If only one side has the other's key, `h8c up` on both sides logs what is missing. `h8c trust add` and `remove` take effect on a running node at once.

Without any login, hand-added keys are all there is, and everything works as before.

### Windows

`h8c id`, `login`, `up`, `peers` and `roster` have been run on Windows 11. One thing needs doing by hand: letting other machines connect in.

**What you see.** On the other machines, `h8c peers` lists the Windows machine as online but only `VIA inbound` and with no address. They cannot call it; it stays online only because it keeps calling them.

**Why.** The first time a program listens, Windows asks whether to allow it. If nobody answers "allow" as an administrator, Windows creates block rules for that program, typically one for TCP and one for UDP. A block rule wins over any allow rule, so adding a rule for the port is not enough: the block rules have to go first.

**What to do**, in PowerShell run as Administrator:

```powershell
$exe = "C:\path\to\h8c.exe"   # the full path; wildcards do not work

# The rules Windows created for it. Expect two with Action Block.
Get-NetFirewallApplicationFilter -Program $exe | Get-NetFirewallRule |
    Format-Table DisplayName, Direction, Action, Enabled

# Delete them.
Get-NetFirewallApplicationFilter -Program $exe | Get-NetFirewallRule | Remove-NetFirewallRule

# Let nodes on private networks reach the peer link.
New-NetFirewallRule -DisplayName "hebi8XC peer link" -Direction Inbound -Action Allow `
    -Protocol TCP -LocalPort 4892 -Program $exe -Profile Private

# Let mDNS announcements of other nodes in. See the note below.
New-NetFirewallRule -DisplayName "hebi8XC mDNS" -Direction Inbound -Action Allow `
    -Protocol UDP -LocalPort 5353 -Program $exe -Profile Private
```

Notes:

- These commands were written from Microsoft's documentation ([firewall rules](https://learn.microsoft.com/en-us/windows/security/operating-system-security/network-security/windows-firewall/rules), [Get-NetFirewallApplicationFilter](https://learn.microsoft.com/en-us/powershell/module/netsecurity/get-netfirewallapplicationfilter), [Remove-NetFirewallRule](https://learn.microsoft.com/en-us/powershell/module/netsecurity/remove-netfirewallrule), [New-NetFirewallRule](https://learn.microsoft.com/en-us/powershell/module/netsecurity/new-netfirewallrule), read on 2026-10-13). They have not been run on a real machine yet. The cause, block rules created for `h8c.exe`, was seen on one.
- If the first command lists nothing, the path does not match the one Windows recorded; look for the rules in "Windows Defender Firewall with Advanced Security" under Inbound Rules.
- The rules apply to networks Windows has marked Private. `Get-NetConnectionProfile` shows how each network is marked. Tailscale sets its own adapter to Private; a home network that Windows has marked Public is not covered until it is changed to Private.
- The rules name the program by path. A copy of `h8c.exe` somewhere else is a new program to Windows, with a new prompt and new block rules.
- Whether mDNS needs the UDP 5353 rule has not been tested. Inbound traffic is blocked unless a rule allows it, and the automatic block rules cover UDP as well, so the rule is expected to be needed. The mDNS rule that ships with Windows is assumed to cover only Windows' own service; that was not checked either. Without working mDNS the machine is still found through Tailscale, `--peer`, or by calling the others itself.
- mDNS runs on one network adapter. On a machine with several, `h8c` takes the one Windows sends multicast through when that is a LAN adapter, otherwise the LAN adapter with the lowest index, and never a VPN adapter.

Things to know:

- The peer link listens on TCP port **4892** on all interfaces (`--listen` changes it). Tailscale discovery only probes 4892.
- An address that does not answer is tried less and less often, down to once in 5 minutes. A node that starts later announces itself, so it normally shows up within seconds anyway.
- One node runs per data directory; a second `h8c up` on the same directory exits.
- mDNS only matches nodes of the same user. Logged in, that is the GitHub account. Not logged in, it is the OS user name, or `--user ID` if your machines use different names. A logged-in node and one that is not do not find each other over mDNS.
- Without network, a node keeps using the keys it fetched last and says in its log how old they are.
- State lives in the data directory (`--data-dir`, default `hebi8xc` under the OS config directory, e.g. `~/.config/hebi8xc`). The node name defaults to the host name; `h8c name` shows and changes it.

More in [docs/design.md](docs/design.md). What was checked about GitHub is in [docs/github-roster.md](docs/github-roster.md).

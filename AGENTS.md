# hebi8XC

Nodes on one user's machines that find each other, authenticate and connect directly. Go, single binary `h8c`. Read [docs/design.md](docs/design.md) before changing behavior.

Module `github.com/Hebi8/hebi8XC`, default branch `master`.

## Commands

Go may not be on PATH: `export PATH=$HOME/.local/go/bin:$PATH`.

```sh
go build -o h8c ./cmd/h8c     # build
go test ./...                 # test (add -race when touching node, link or peers)
go vet ./...                  # vet
gofmt -l .                    # must print nothing; fix with gofmt -w .
GOOS=darwin go build ./... && GOOS=windows go build ./...   # must keep building
```

## Rules

- Discovery carries no trust. Only a completed handshake in `internal/link` makes a peer.
- Use TLS from the standard library. Do not write cryptographic protocols.
- Never send a token or private key to a peer, and never put one in a discovery announcement.
- Tests are deterministic: inject the clock, use fake sources and `t.TempDir()`, drive rounds by hand, never sleep, touch no network beyond loopback.
- Test data and documentation use made-up values only: `100.64.0.x` for Tailscale, `192.168.1.2x` for the LAN, `alice` as the user. No real address, host name, user name or node id of any machine, also not in pasted output.
- No token is written to disk or to a log.
- Tests build their own Wake index with `waketest` and never run `wake-cli` or open a real index. Real sessions are private: no title or text of one in a commit, a document or a report.
- Keep dependencies few; say why when adding one.
- Do not install `h8c` as a system service or touch other services on the machine when testing. Use temporary data directories and foreground processes, and clean up.
- Commits follow Conventional Commits.

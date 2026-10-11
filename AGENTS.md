# hebi8XC

Nodes on one user's machines that find each other, authenticate and connect directly. Go, single binary `h8c`. Read [docs/design.md](docs/design.md) before changing behavior.

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
- Keep dependencies few; say why when adding one.
- Do not install `h8c` as a system service or touch other services on the machine when testing. Use temporary data directories and foreground processes, and clean up.
- Commits follow Conventional Commits.

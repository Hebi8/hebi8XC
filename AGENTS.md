# hebi8XC

Nodes on one user's machines that find each other, authenticate and connect directly. Go, single binary `h8c`. Read [docs/design.md](docs/design.md) before changing behavior. What is done and what is left is in [docs/roadmap.md](docs/roadmap.md); keep it current when a listed item is finished or a new limit is found.

Module `github.com/Hebi8/hebi8XC`, default branch `master`.

## Commands

Go may not be on PATH: `export PATH=$HOME/.local/go/bin:$PATH`.

```sh
go build -o h8c ./cmd/h8c     # build
go test ./...                 # test (add -race when touching node, link or peers)
go vet ./...                  # vet
gofmt -l .                    # must print nothing; fix with gofmt -w .
GOOS=darwin go build ./... && GOOS=windows go build ./...   # must keep building
go run ./scripts/websmoke     # the page in headless Chrome; run it after any change in internal/web/static
go run ./scripts/demo         # three made-up machines to look at the page with; prints the link, Ctrl-C cleans up
```

Both scripts build `h8c` from the working tree and run it in temporary directories on loopback ports of their own, with indexes made by `waketest`; they touch no installed node. `websmoke` needs Chrome (`-chrome PATH` if it is not on PATH) and is not part of `go test`; with `-shots DIR` it also writes pictures of the page, narrow and wide, English and Chinese, to look at after a change of the layout (a directory under `/tmp`, never in the repository). The demo holds about a hundred sessions of several agents and projects with English and Chinese titles, one of many pages, one with code blocks and long lines, two projects with very long names, an offline machine, and of that machine one session that was read before and others that cannot be read; `-port N` moves it from 24892–24895.

## Rules

- Discovery carries no trust. Only a completed handshake in `internal/link` makes a peer.
- Any two builds of the same protocol version must work together; nodes are updated one at a time. Within a version only add optional fields to what nodes say to each other, and handle their absence. Removing or renaming a field, making one required, or changing what one means needs the next version (`node.Protocol`). The samples in `internal/node/testdata/protocol-v1` are never edited to make a test pass, and `protocol_test.go` must stay green. "All machines must run the same build" is never an acceptable requirement.
- Use TLS from the standard library. Do not write cryptographic protocols.
- Never send a token or private key to a peer, and never put one in a discovery announcement.
- Tests are deterministic: inject the clock, use fake sources and `t.TempDir()`, drive rounds by hand, never sleep, touch no network beyond loopback.
- Test data and documentation use made-up values only: `100.64.0.x` for Tailscale, `192.168.1.2x` for the LAN, `alice` as the user. No real address, host name, user name or node id of any machine, also not in pasted output.
- No token is written to disk or to a log.
- Tests build their own Wake index with `waketest` and never run `wake-cli` or open a real index. Real sessions are private: no title or text of one in a commit, a document or a report.
- Keep dependencies few; say why when adding one.
- Do not install `h8c` as a system service or touch other services on the machine when testing. Use temporary data directories and foreground processes, and clean up.
- The page's script is not covered by `go test`: after a change in `internal/web/static`, `go run ./scripts/websmoke` has to pass. A new behavior of the page gets a check there.
- Commits follow Conventional Commits.

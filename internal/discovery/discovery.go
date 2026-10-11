// Package discovery finds addresses where one of the user's nodes might be
// listening. A candidate carries no trust: it only becomes a peer after the
// handshake in package link succeeds.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Source names, as recorded in the peer table.
const (
	MDNS       = "mdns"
	Tailscale  = "tailscale"
	Static     = "static"
	Remembered = "remembered"
	Inbound    = "inbound"
)

// Candidate is an address worth probing and the source that suggested it.
type Candidate struct {
	Addr   string // host:port
	Source string
}

// Source lists the candidates it currently knows. It is asked once per
// discovery round.
type Source interface {
	Name() string
	Candidates(ctx context.Context) ([]Candidate, error)
}

// NetworkTag derives the opaque label nodes broadcast so that they only
// probe nodes of the same user. It is a filter, not a secret: anyone who can
// guess the user id can compute it.
func NetworkTag(user string) string {
	sum := sha256.Sum256([]byte("hebi8xc network v1\x00" + user))
	return hex.EncodeToString(sum[:8])
}

// Func adapts a function to a Source.
func Func(name string, list func() []string) Source {
	return funcSource{name: name, list: list}
}

type funcSource struct {
	name string
	list func() []string
}

func (s funcSource) Name() string { return s.name }

func (s funcSource) Candidates(context.Context) ([]Candidate, error) {
	var out []Candidate
	for _, addr := range s.list() {
		out = append(out, Candidate{Addr: addr, Source: s.name})
	}
	return out, nil
}

// StaticSource returns the addresses the user typed in.
func StaticSource(addrs []string) Source {
	return Func(Static, func() []string { return addrs })
}

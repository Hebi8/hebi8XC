package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os/exec"
	"runtime"
	"strconv"
)

// TailscaleSource suggests every online peer of the local Tailscale client on
// a fixed port. Without Tailscale it suggests nothing.
type TailscaleSource struct {
	Port int

	// status returns the output of "tailscale status --json".
	status func(ctx context.Context) ([]byte, error)
}

func NewTailscaleSource(port int) *TailscaleSource {
	return &TailscaleSource{Port: port, status: tailscaleStatus}
}

func (s *TailscaleSource) Name() string { return Tailscale }

func (s *TailscaleSource) Candidates(ctx context.Context) ([]Candidate, error) {
	raw, err := s.status(ctx)
	if errors.Is(err, exec.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var status struct {
		BackendState string
		Peer         map[string]struct {
			TailscaleIPs []netip.Addr
			Online       bool
		}
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, err
	}
	if status.BackendState != "Running" {
		return nil, nil
	}
	var out []Candidate
	for _, peer := range status.Peer {
		if !peer.Online {
			continue
		}
		for _, ip := range peer.TailscaleIPs {
			if ip.Is4() {
				out = append(out, Candidate{
					Addr:   net.JoinHostPort(ip.String(), strconv.Itoa(s.Port)),
					Source: Tailscale,
				})
				break
			}
		}
	}
	return out, nil
}

// macAppCLI is where the macOS app keeps the CLI when it is not on PATH.
const macAppCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

func tailscaleStatus(ctx context.Context) ([]byte, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil && runtime.GOOS == "darwin" {
		bin, err = exec.LookPath(macAppCLI)
	}
	if err != nil {
		return nil, exec.ErrNotFound
	}
	return exec.CommandContext(ctx, bin, "status", "--json").Output()
}

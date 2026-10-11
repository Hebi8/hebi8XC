package link

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
)

// Client makes authenticated requests to other nodes.
type Client struct {
	cfg *tls.Config
}

func NewClient(a *Auth) (*Client, error) {
	cfg, err := a.tlsConfig()
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

// Call sends one JSON request to the node at addr and decodes its JSON reply
// into out. It returns the peer's public key once the peer has passed the
// roster check, even if the call then fails.
//
// Every call opens a fresh connection, so each one is a full handshake against
// the current roster. Redirects are not followed: only the node that passed
// the handshake may answer.
func (c *Client) Call(ctx context.Context, addr, method, path string, in, out any) (ed25519.PublicKey, error) {
	var trusted ed25519.PublicKey
	cfg := c.cfg.Clone()
	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if err := verify(cs); err != nil {
			return err
		}
		trusted, _ = peerKey(cs)
		return nil
	}
	hc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		// A peer that rejects our certificate does so after our side of the
		// TLS 1.3 handshake is done, so its alert arrives here.
		if trusted != nil && isCertificateAlert(err) {
			return trusted, fmt.Errorf("%w: %v", ErrRejected, err)
		}
		return trusted, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return trusted, ErrRejected
	}
	if resp.StatusCode != http.StatusOK {
		return trusted, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return trusted, json.NewDecoder(io.LimitReader(resp.Body, MaxMessage)).Decode(out)
}

// isCertificateAlert reports whether err is a TLS alert from the peer saying
// it does not accept our certificate. crypto/tls returns remote alerts as a
// net.OpError wrapping an unexported uint8 type, so the alert number is read
// by kind.
func isCertificateAlert(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "remote error" {
		return false
	}
	v := reflect.ValueOf(op.Err)
	if v.Kind() != reflect.Uint8 {
		return false
	}
	switch v.Uint() {
	case 42, 43, 44, 45, 46, 48, 116: // the certificate alerts of RFC 8446, section 6.2
		return true
	}
	return false
}

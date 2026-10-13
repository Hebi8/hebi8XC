// Package link is the connection between two nodes: HTTP over mutual
// TLS 1.3. Each side presents a certificate self-signed with its node key;
// the TLS handshake proves possession of that key, and instead of a CA chain
// each side checks that the other's key is in its roster.
package link

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/Hebi8/hebi8XC/internal/roster"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

// Auth is what a node needs to authenticate itself and its peers.
type Auth struct {
	Key    ed25519.PrivateKey
	Roster roster.Roster
	User   string
}

// UntrustedError reports a peer whose key is not in our roster.
type UntrustedError struct {
	Key ed25519.PublicKey
}

func (e *UntrustedError) Error() string {
	return "peer key is not in the roster: " + sshkey.Marshal(e.Key)
}

// ErrRejected reports a peer that is in our roster but said it does not
// accept our key, which is what a peer does when we are not in its roster.
var ErrRejected = errors.New("peer is trusted but refused this node")

// MaxMessage bounds the JSON body of a request or reply between nodes.
const MaxMessage = 64 << 10

// tlsConfig serves both directions: a server ignores InsecureSkipVerify and a
// client ignores ClientAuth.
func (a *Auth) tlsConfig() (*tls.Config, error) {
	cert, err := selfSigned(a.Key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		// There is no CA and no hostname to check. VerifyConnection replaces
		// both with the roster lookup, and runs on resumed sessions too.
		InsecureSkipVerify: true,
		VerifyConnection:   a.verify,
	}, nil
}

func (a *Auth) verify(cs tls.ConnectionState) error {
	key, err := peerKey(cs)
	if err != nil {
		return err
	}
	if !roster.Trusts(a.Roster, a.User, key) {
		return &UntrustedError{Key: key}
	}
	return nil
}

func peerKey(cs tls.ConnectionState) (ed25519.PublicKey, error) {
	if len(cs.PeerCertificates) == 0 {
		return nil, errors.New("peer sent no certificate")
	}
	key, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("peer certificate is not Ed25519")
	}
	return key, nil
}

// selfSigned wraps key in a certificate. Nothing but the public key is ever
// checked, so the other fields are fixed and the validity never ends: how
// long a key is trusted is the roster's decision.
func selfSigned(key ed25519.PrivateKey) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hebi8xc node"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// StatusError is an answer from a peer other than success.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	text := fmt.Sprintf("peer answered %d %s", e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

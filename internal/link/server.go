package link

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"hebi8xc/internal/roster"
)

type peerKeyCtx struct{}

// PeerKey returns the authenticated public key of the node that sent r.
func PeerKey(r *http.Request) ed25519.PublicKey {
	key, _ := r.Context().Value(peerKeyCtx{}).(ed25519.PublicKey)
	return key
}

// NewServer returns an HTTP server that only lets roster members reach h.
// Start it with ServeTLS(listener, "", "").
func NewServer(a *Auth, h http.Handler) (*http.Server, error) {
	cfg, err := a.tlsConfig()
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Handler:           a.requireTrusted(h),
		TLSConfig:         cfg,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(handshakeFilter{os.Stderr}, "", log.LstdFlags),
	}, nil
}

// requireTrusted re-checks the roster on every request, so removing a key
// also cuts off connections that were opened while it was still trusted.
func (a *Auth) requireTrusted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := peerKey(*r.TLS)
		if err != nil || !roster.Trusts(a.Roster, a.User, key) {
			http.Error(w, "untrusted", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKeyCtx{}, key)))
	})
}

// handshakeFilter drops net/http's "TLS handshake error" lines: strangers
// probing the port are expected and not worth a log line each.
type handshakeFilter struct {
	w io.Writer
}

func (f handshakeFilter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}
	return f.w.Write(p)
}

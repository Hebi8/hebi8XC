package link

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"hebi8xc/internal/roster"
	"hebi8xc/internal/sshkey"
)

type side struct {
	auth   *Auth
	roster *roster.File
}

func (s *side) pub() ed25519.PublicKey { return s.auth.Key.Public().(ed25519.PublicKey) }

func newSide(t *testing.T) *side {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := roster.OpenFile(filepath.Join(t.TempDir(), "trusted_keys"))
	if err != nil {
		t.Fatal(err)
	}
	return &side{auth: &Auth{Key: key, Roster: r, User: "me"}, roster: r}
}

func (s *side) trust(t *testing.T, other *side) {
	t.Helper()
	if _, err := s.roster.Add(other.pub(), ""); err != nil {
		t.Fatal(err)
	}
}

// serve starts s as a server on loopback that answers GET /whoami with the
// caller's key, and returns its address.
func (s *side) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(PeerKey(r))
	})
	srv, err := NewServer(s.auth, mux)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func (s *side) whoami(t *testing.T, addr string) (seenAs, peer ed25519.PublicKey, err error) {
	t.Helper()
	c, err := NewClient(s.auth)
	if err != nil {
		t.Fatal(err)
	}
	peer, err = c.Call(context.Background(), addr, "GET", "/whoami", nil, &seenAs)
	return seenAs, peer, err
}

func TestMutualTrustConnects(t *testing.T) {
	a, b := newSide(t), newSide(t)
	a.trust(t, b)
	b.trust(t, a)

	seenAs, peer, err := a.whoami(t, b.serve(t))
	if err != nil {
		t.Fatal(err)
	}
	if !peer.Equal(b.pub()) {
		t.Error("client did not learn the server's key")
	}
	if !seenAs.Equal(a.pub()) {
		t.Error("server did not learn the client's key")
	}
}

func TestClientRejectsServerMissingFromRoster(t *testing.T) {
	a, b := newSide(t), newSide(t)
	b.trust(t, a)

	_, peer, err := a.whoami(t, b.serve(t))
	var untrusted *UntrustedError
	if !errors.As(err, &untrusted) {
		t.Fatalf("err = %v, want UntrustedError", err)
	}
	if !untrusted.Key.Equal(b.pub()) {
		t.Error("UntrustedError does not carry the server's key")
	}
	if peer != nil {
		t.Error("an untrusted peer was reported as authenticated")
	}
}

// One-way trust is not enough: a trusts b, but b's roster does not list a.
func TestServerRejectsClientMissingFromRoster(t *testing.T) {
	a, b := newSide(t), newSide(t)
	a.trust(t, b)

	_, peer, err := a.whoami(t, b.serve(t))
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if !peer.Equal(b.pub()) {
		t.Error("the rejecting peer should still be identified")
	}
}

func TestNeitherTrustsFails(t *testing.T) {
	a, b := newSide(t), newSide(t)
	if _, _, err := a.whoami(t, b.serve(t)); err == nil {
		t.Fatal("strangers connected")
	}
}

// A client whose certificate key is not the one it can sign with never gets
// as far as the roster check.
func TestClientMustHoldThePrivateKey(t *testing.T) {
	a, b, thief := newSide(t), newSide(t), newSide(t)
	a.trust(t, b)
	b.trust(t, a)
	thief.trust(t, b)
	addr := b.serve(t)

	// The thief presents a's certificate but signs with its own key.
	stolen, err := selfSigned(a.auth.Key)
	if err != nil {
		t.Fatal(err)
	}
	stolen.PrivateKey = thief.auth.Key
	c, err := NewClient(thief.auth)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.Certificates[0] = stolen

	var seenAs ed25519.PublicKey
	if _, err := c.Call(context.Background(), addr, "GET", "/whoami", nil, &seenAs); err == nil {
		t.Fatal("server accepted a certificate without proof of its private key")
	}
}

func TestRemovingAKeyTakesEffectOnTheNextCall(t *testing.T) {
	a, b := newSide(t), newSide(t)
	a.trust(t, b)
	b.trust(t, a)
	addr := b.serve(t)

	if _, _, err := a.whoami(t, addr); err != nil {
		t.Fatal(err)
	}
	if _, err := b.roster.Remove(sshkeyOf(a)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.whoami(t, addr); !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected after removal", err)
	}
}

func sshkeyOf(s *side) string { return sshkey.Marshal(s.pub()) }

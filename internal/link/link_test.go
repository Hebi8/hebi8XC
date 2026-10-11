package link

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/roster"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
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

// serve starts s as a server on loopback and returns its address. Besides
// any extra routes it answers GET /whoami with the caller's key.
func (s *side) serve(t *testing.T, routes ...func(*http.ServeMux)) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(PeerKey(r))
	})
	for _, add := range routes {
		add(mux)
	}
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

func (s *side) call(t *testing.T, addr, method, path string, in, out any) error {
	t.Helper()
	c, err := NewClient(s.auth)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Call(context.Background(), addr, method, path, in, out)
	return err
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

// A trusted node must not be able to hand the call to someone else: the
// answer of a redirect target would arrive under the trusted node's key.
func TestRedirectsAreNotFollowed(t *testing.T) {
	a, b := newSide(t), newSide(t)
	a.trust(t, b)
	b.trust(t, a)

	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`"forged"`))
	}))
	defer plain.Close()

	addr := b.serve(t, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /moved", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL, http.StatusTemporaryRedirect)
		})
	})

	var out string
	err := a.call(t, addr, "POST", "/moved", "hello", &out)
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Errorf("err = %v, want the 307 reported as a failure", err)
	}
	if out != "" || hits.Load() != 0 {
		t.Errorf("redirect was followed: answer %q, %d requests reached the target", out, hits.Load())
	}
}

func TestMessagesAreCapped(t *testing.T) {
	a, b := newSide(t), newSide(t)
	a.trust(t, b)
	b.trust(t, a)
	big := strings.Repeat("x", MaxMessage)

	var received atomic.Int64
	addr := b.serve(t, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
			var in string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			received.Store(int64(len(in)))
			json.NewEncoder(w).Encode(in)
		})
		mux.HandleFunc("GET /big", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(big)
		})
	})

	var out string
	if err := a.call(t, addr, "POST", "/echo", "small", &out); err != nil || out != "small" {
		t.Fatalf("small message: %q, %v", out, err)
	}
	if err := a.call(t, addr, "POST", "/echo", big, &out); err == nil {
		t.Error("server accepted a request body over MaxMessage")
	}
	if received.Load() == int64(len(big)) {
		t.Error("server read the whole oversized body")
	}
	if err := a.call(t, addr, "GET", "/big", nil, &out); err == nil {
		t.Error("client accepted a reply over MaxMessage")
	}
}

// A trusted peer that just hangs up is a broken connection, not a refusal:
// telling the user to fix the roster would send them the wrong way.
func TestDroppedConnectionIsNotARefusal(t *testing.T) {
	a, b := newSide(t), newSide(t)
	a.trust(t, b)
	b.trust(t, a)
	addr := b.serve(t, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /drop", func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
		})
	})

	var out string
	err := a.call(t, addr, "GET", "/drop", nil, &out)
	if err == nil || errors.Is(err, ErrRejected) {
		t.Errorf("err = %v, want a plain connection failure", err)
	}
}

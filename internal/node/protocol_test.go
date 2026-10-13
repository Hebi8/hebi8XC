package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/identity"
	"github.com/Hebi8/hebi8XC/internal/link"
	"github.com/Hebi8/hebi8XC/internal/peers"
	"github.com/Hebi8/hebi8XC/internal/roster"
	"github.com/Hebi8/hebi8XC/internal/sessions"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
)

// The files in testdata/protocol-v1 are what nodes of build 3dc8c5a said to
// each other. These tests keep this build able to work with that one, in
// both directions. If one of them fails, the protocol was changed in a way
// that needs a new protocol version, not a new sample.

func sample(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "protocol-v1", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sampleRequests(t *testing.T) (feed, messages string) {
	t.Helper()
	var paths struct{ Sessions, Messages string }
	if err := json.Unmarshal(sample(t, "requests.json"), &paths); err != nil {
		t.Fatal(err)
	}
	return paths.Sessions, paths.Messages
}

// recorded is a node that answers with fixed bytes, as a build of another
// age would. It has a key of its own, which the node under test trusts.
type recorded struct {
	addr string
	id   *identity.Identity

	mu    sync.Mutex
	asked []string
}

func (r *recorded) requests(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, path := range r.asked {
		if strings.HasPrefix(path, prefix) {
			count++
		}
	}
	return count
}

// recordedNode starts a node that answers hello, the first page of its
// feed and the first page of a session with the given bytes, and everything
// after those first pages with an empty page of the same kind. tn trusts it.
func recordedNode(t *testing.T, tn *testNode, hello, feed, messages []byte) *recorded {
	t.Helper()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tn.roster.Add(id.Public(), "recorded"); err != nil {
		t.Fatal(err)
	}
	trusted, err := roster.OpenFile(filepath.Join(t.TempDir(), "trusted_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trusted.Add(tn.id.Public(), tn.id.Name); err != nil {
		t.Fatal(err)
	}
	r := &recorded{id: id}
	// What follows the first page: the same feed, nothing more in it.
	var page struct{ Epoch string }
	json.Unmarshal(feed, &page)
	rest := map[string]string{
		"/v1/sessions":          fmt.Sprintf(`{"epoch":%q,"changes":[],"more":false}`, page.Epoch),
		"/v1/sessions/messages": fmt.Sprintf(`{"epoch":%q,"rev":1,"messages":[],"more":false}`, page.Epoch),
	}
	first := map[string][]byte{"/v1/sessions": feed, "/v1/sessions/messages": messages}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.asked = append(r.asked, req.URL.Path)
		r.mu.Unlock()
		switch {
		case req.URL.Path == "/v1/hello":
			w.Write(hello)
		case first[req.URL.Path] == nil:
			http.NotFound(w, req)
		case req.URL.Query().Get("after") == "0":
			w.Write(first[req.URL.Path])
		default:
			w.Write([]byte(rest[req.URL.Path]))
		}
	})
	srv, err := link.NewServer(&link.Auth{Key: id.Key, Roster: trusted, User: "me"}, mux)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	r.addr = ln.Addr().String()
	return r
}

// This build reads from a node of build 3dc8c5a: it greets it, takes it for
// a node of protocol 1, lists its sessions and reads one. The same with a
// page of messages from before that page carried the epoch.
func TestThisBuildUnderstandsProtocol1Answers(t *testing.T) {
	for _, messages := range []string{"messages.response.json", "messages.response.no-epoch.json"} {
		c := newClock()
		laptop := sessionNode(t, c, "laptop")
		old := recordedNode(t, laptop, sample(t, "hello.response.json"),
			sample(t, "sessions.response.json"), sample(t, messages))
		laptop.static = []string{old.addr}
		laptop.round()

		var list []peers.Peer
		if err := laptop.control("/v1/peers", &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || !list[0].Online || list[0].ID != "fedcba9876543210" || list[0].Name != "desk" ||
			list[0].Protocol != 0 || list[0].Version != "" || list[0].Speaks() != 1 {
			t.Fatalf("%s: the old node as a peer = %+v, want it online, protocol not stated and taken as 1", messages, list)
		}

		laptop.sessions.Round(context.Background())
		l := laptop.listing("")
		if got := refs(l); !slices.Equal(got, []string{"desk/claude:0f3a9c online"}) {
			t.Fatalf("%s: listed %v, want the one session that is not deleted", messages, got)
		}
		s := l.Sessions[0]
		if s.Title != "Fix the parser" || s.Agent != "claude" || s.Project != "parser" ||
			s.ProjectPath != "/home/alice/src/parser" || s.Branch != "main" || s.Model != "model-x" ||
			s.Messages != 2 || s.Parent != "claude:0aaa00" || !s.Spawned || !s.Archived || s.Incompatible ||
			s.UpdatedAt.UnixMilli() != 1791803600002 || s.CreatedAt.UnixMilli() != 1791803599000 {
			t.Errorf("%s: session = %+v", messages, s)
		}
		for _, n := range l.Nodes {
			if n.Name == "desk" && (n.Incompatible || n.Protocol != 1 || n.Sessions != 1 || n.SyncedAt == nil) {
				t.Errorf("%s: the old node in the listing = %+v", messages, n)
			}
		}

		var r sessions.Reading
		if err := laptop.control("/v1/sessions/messages?node=desk&key=claude:0f3a9c", &r); err != nil {
			t.Fatalf("%s: reading a session of the old node: %v", messages, err)
		}
		if r.From != "peer" || len(r.Messages) != 2 || r.Next != 0 {
			t.Fatalf("%s: reading = %+v", messages, r)
		}
		first, second := r.Messages[0], r.Messages[1]
		if first.ID != 1 || first.Seq != 1 || first.Role != "user" || first.Time != 1791803500000 ||
			first.Text != "the parser drops the last token" || first.Truncated || first.Sidechain != "" {
			t.Errorf("%s: first message = %+v", messages, first)
		}
		if second.ID != 2 || second.Sidechain != "sc-1" || second.Role != "assistant" || !second.Truncated || second.Time != 0 {
			t.Errorf("%s: second message = %+v", messages, second)
		}
		// Reading again uses the copy: the old node is not asked a second time.
		asked := old.requests("/v1/sessions/messages")
		if err := laptop.control("/v1/sessions/messages?node=desk&key=claude:0f3a9c", &r); err != nil || r.From != "cache" ||
			old.requests("/v1/sessions/messages") != asked {
			t.Errorf("%s: second reading: from %q, %v", messages, r.From, err)
		}
		for len(laptop.logs) > 0 {
			if msg := <-laptop.logs; strings.Contains(msg, "protocol") {
				t.Errorf("%s: the node complained about the old node's protocol: %s", messages, msg)
			}
		}
	}
}

// holds reports where got lacks something that want has: a field, an
// element, or the same value. skip names fields whose value differs from
// node to node and is not compared.
func holds(got, want any, path string, skip map[string]bool) []string {
	switch want := want.(type) {
	case map[string]any:
		obj, ok := got.(map[string]any)
		if !ok {
			return []string{path + ": not an object any more"}
		}
		var missing []string
		for key, value := range want {
			at := path + "." + key
			if _, has := obj[key]; !has {
				missing = append(missing, at+": field is gone")
			} else if !skip[key] {
				missing = append(missing, holds(obj[key], value, at, skip)...)
			}
		}
		return missing
	case []any:
		list, ok := got.([]any)
		if !ok || len(list) < len(want) {
			return []string{path + ": not a list of at least that length any more"}
		}
		var missing []string
		for i, value := range want {
			missing = append(missing, holds(list[i], value, fmt.Sprintf("%s[%d]", path, i), skip)...)
		}
		return missing
	default:
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return []string{fmt.Sprintf("%s: %v, was %v", path, got, want)}
		}
		return nil
	}
}

// A node of build 3dc8c5a reads from this build: it sends the requests that
// build sent, and every field that build got back is still there with the
// same meaning. New fields may have joined them.
func TestProtocol1ReadersStillUnderstandThisBuild(t *testing.T) {
	c := newClock()
	desk, old := sessionNode(t, c, "desk"), sessionNode(t, c, "old")
	desk.trust(old)
	old.trust(desk)
	// The data the samples were recorded with.
	desk.wake.Put(waketest.Session{Key: "claude:0f3a9c", Agent: "claude", Title: "Fix the parser", Project: "parser",
		Path: "/home/alice/src/parser", Branch: "main", Model: "model-x", UpdatedAt: 1791803600000,
		Parent: "claude:0aaa00", Spawned: true, Archived: true},
		waketest.Message{Role: "user", Text: "the parser drops the last token", Time: 1791803500000},
		waketest.Message{Role: "assistant", Text: strings.Repeat("x", sessions.MaxText+1), Sidechain: "sc-1"})
	desk.wake.Put(waketest.Session{Key: "codex:71be", Agent: "codex", Title: "Gone", Project: "notes", UpdatedAt: 1791700000000})
	ctx := context.Background()
	desk.sessions.SyncLocal(ctx)
	desk.wake.Delete("codex:71be")
	desk.sessions.SyncLocal(ctx)

	// Values that differ from node to node, or that the sample shortened.
	skip := map[string]bool{"epoch": true, "id": true, "name": true, "port": true, "text": true}
	check := func(what string, got json.RawMessage, sampleName string) {
		t.Helper()
		var have, want any
		if err := json.Unmarshal(got, &have); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := json.Unmarshal(sample(t, sampleName), &want); err != nil {
			t.Fatal(err)
		}
		for _, problem := range holds(have, want, what, skip) {
			t.Error(problem)
		}
	}

	var hello json.RawMessage
	if _, err := old.client.Call(ctx, desk.addr, http.MethodPost, "/v1/hello",
		json.RawMessage(sample(t, "hello.request.json")), &hello); err != nil {
		t.Fatalf("the hello of an old node was refused: %v", err)
	}
	check("hello", hello, "hello.response.json")
	// The old node did not say what it speaks: it is a node of protocol 1.
	p := desk.peers()
	if len(p) != 1 || p[0].ID != "0123456789abcdef" || p[0].Name != "laptop" || !p[0].Online || p[0].Speaks() != 1 || p[0].Version != "" {
		t.Errorf("the old node as desk sees it = %+v", p)
	}

	feed, messages := sampleRequests(t)
	var got json.RawMessage
	if _, err := old.client.Get(ctx, desk.addr, feed, &got, sessions.MaxReply); err != nil {
		t.Fatalf("GET %s: %v", feed, err)
	}
	check("feed", got, "sessions.response.json")
	if _, err := old.client.Get(ctx, desk.addr, messages, &got, sessions.MaxReply); err != nil {
		t.Fatalf("GET %s: %v", messages, err)
	}
	check("messages", got, "messages.response.json")

	// That build needed these two to carry a value, whatever it is.
	var page struct{ Epoch string }
	if json.Unmarshal(got, &page); page.Epoch == "" {
		t.Error("messages: epoch is empty; build 3dc8c5a refuses such a page")
	}
}

// A peer that says it speaks another protocol is still a peer: found,
// online, listed. But no sessions are asked of it, the node says why, once,
// and every listing marks it.
func TestPeerOfAnotherProtocol(t *testing.T) {
	c := newClock()
	laptop := sessionNode(t, c, "laptop")
	hello := []byte(`{"id":"fedcba9876543210","name":"desk","port":4892,"protocol":2,"version":"v2.0.0"}`)
	future := recordedNode(t, laptop, hello, sample(t, "sessions.response.json"), sample(t, "messages.response.json"))
	laptop.static = []string{future.addr}
	ctx := context.Background()
	for range 3 {
		laptop.round()
		laptop.sessions.Round(ctx)
		c.advance(Interval)
	}

	p := laptop.peers()
	if len(p) != 1 || !p[0].Online || p[0].Protocol != 2 || p[0].Version != "v2.0.0" || p[0].Speaks() != 2 {
		t.Fatalf("peer = %+v", p)
	}
	if n := future.requests("/v1/sessions"); n != 0 {
		t.Errorf("sessions were asked of a node of another protocol, %d times", n)
	}
	said := 0
	for len(laptop.logs) > 0 {
		if msg := <-laptop.logs; strings.Contains(msg, "desk speaks protocol 2 and this node speaks 1") {
			said++
		}
	}
	if said != 1 {
		t.Errorf("the node said %d times that the peer speaks another protocol, want once", said)
	}
	l := laptop.listing("")
	var desk sessions.Node
	for _, n := range l.Nodes {
		if n.Name == "desk" {
			desk = n
		}
	}
	if !desk.Online || !desk.Incompatible || desk.Protocol != 2 || desk.Version != "v2.0.0" || desk.Sessions != 0 {
		t.Errorf("the node in the listing = %+v", desk)
	}
	if st := laptop.status(); st.Protocol != Protocol || st.Version == "" {
		t.Errorf("status = %+v", st)
	}
}

// The node tells its peers what it speaks, in fields an older build skips.
func TestHelloSaysProtocolAndVersion(t *testing.T) {
	a, b, _ := pair(t)
	a.static = []string{b.addr}
	a.round()
	b.round()
	for _, tc := range []struct{ self, other *testNode }{{a, b}, {b, a}} {
		p := tc.self.peer(tc.other)
		if p.Protocol != Protocol || p.Version == "" {
			t.Errorf("peer = %+v, want its protocol and version on record", p)
		}
	}
	raw, _ := json.Marshal(a.info())
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	for _, name := range []string{"id", "name", "port", "protocol", "version"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("hello lacks %q: %s", name, raw)
		}
	}
}

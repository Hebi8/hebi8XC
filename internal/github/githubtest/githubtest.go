// Package githubtest is an in-memory GitHub for tests: the device flow and
// the account and SSH signing key endpoints that package github calls.
package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Hebi8/hebi8XC/internal/github"
)

// UserCode is the code every device flow of the fake shows.
const UserCode = "WDJB-MJHT"

type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	accounts map[int64]*account
	tokens   map[string]int64
	nextKey  int64
	script   []string // pending answers of the token endpoint
	signedIn int64    // account the device flow ends in
	down     bool
	limited  bool
	redirect string
	requests map[string]int
	before   map[string][]func()
}

type account struct {
	login string
	keys  []github.SigningKey
}

func New(t *testing.T) *Server {
	s := &Server{
		accounts: make(map[int64]*account),
		tokens:   make(map[string]int64),
		requests: make(map[string]int),
		before:   make(map[string][]func()),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", s.deviceCode)
	mux.HandleFunc("POST /login/oauth/access_token", s.accessToken)
	mux.HandleFunc("GET /user", s.api(s.viewer))
	mux.HandleFunc("GET /user/{id}", s.api(s.accountByID))
	mux.HandleFunc("GET /users/{login}/ssh_signing_keys", s.api(s.publicKeys))
	mux.HandleFunc("GET /user/ssh_signing_keys", s.api(s.viewerKeys))
	mux.HandleFunc("POST /user/ssh_signing_keys", s.api(s.addKey))
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// Client returns a client that talks to the fake.
func (s *Server) Client() *github.Client {
	return &github.Client{API: s.srv.URL, Web: s.srv.URL, HTTP: s.srv.Client()}
}

func (s *Server) AddAccount(id int64, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[id] = &account{login: login}
}

// Rename changes an account's login; its id stays.
func (s *Server) Rename(id int64, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[id].login = login
}

func (s *Server) AddKey(id int64, title, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextKey++
	a := s.accounts[id]
	a.keys = append(a.keys, github.SigningKey{ID: s.nextKey, Title: title, Key: key})
}

func (s *Server) RemoveKey(id int64, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[id]
	a.keys = slices.DeleteFunc(a.keys, func(k github.SigningKey) bool { return k.Key == key })
}

// Keys returns the signing keys registered on an account.
func (s *Server) Keys(id int64) []github.SigningKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.accounts[id].keys)
}

// SignIn makes the device flow end in a token for the account, after the
// token endpoint has first given the scripted error answers in order.
func (s *Server) SignIn(id int64, errors ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signedIn, s.script = id, errors
}

// SetDown makes every API request fail, as when GitHub cannot be reached.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// SetRateLimited makes every API request fail as over the rate limit.
func (s *Server) SetRateLimited(limited bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limited = limited
}

// SetRedirect makes every API request answer with a redirect to the same
// path on another server.
func (s *Server) SetRedirect(to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirect = to
}

// Before runs fn once, just before the next request to the endpoint with
// this pattern is answered. Several calls queue up, one per request. It is
// how a test changes GitHub between two requests of one operation.
func (s *Server) Before(pattern string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before[pattern] = append(s.before[pattern], fn)
}

// Requests returns how often an endpoint was called, by its pattern, for
// example "GET /users/{login}/ssh_signing_keys".
func (s *Server) Requests(pattern string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[pattern]
}

// Tokens returns every token the fake has issued.
func (s *Server) Tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for t := range s.tokens {
		out = append(out, t)
	}
	return out
}

func (s *Server) deviceCode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[r.Pattern]++
	if r.FormValue("client_id") != github.ClientID {
		http.Error(w, "unknown client", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(github.DeviceCode{
		DeviceCode:      "device-code",
		UserCode:        UserCode,
		VerificationURI: "https://github.com/login/device",
		ExpiresIn:       900,
		Interval:        5,
	})
}

func (s *Server) accessToken(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[r.Pattern]++
	if r.FormValue("client_id") != github.ClientID || r.FormValue("device_code") != "device-code" ||
		r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		json.NewEncoder(w).Encode(map[string]any{"error": "incorrect_device_code"})
		return
	}
	if len(s.script) > 0 {
		answer := map[string]any{"error": s.script[0], "error_description": "scripted"}
		if s.script[0] == "slow_down" {
			answer["interval"] = 10
		}
		s.script = s.script[1:]
		json.NewEncoder(w).Encode(answer)
		return
	}
	token := fmt.Sprintf("ghu_fake%d", len(s.tokens)+1)
	s.tokens[token] = s.signedIn
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":  token,
		"token_type":    "bearer",
		"expires_in":    28800,
		"refresh_token": "ghr_fake" + strconv.Itoa(len(s.tokens)),
	})
}

// api wraps an API endpoint: it counts the request and applies the outage
// and rate limit switches. The handler runs with the lock held.
func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var hook func()
		if queue := s.before[r.Pattern]; len(queue) > 0 {
			hook, s.before[r.Pattern] = queue[0], queue[1:]
		}
		s.mu.Unlock()
		if hook != nil {
			hook()
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests[r.Pattern]++
		switch {
		case s.redirect != "":
			http.Redirect(w, r, s.redirect+r.URL.Path, http.StatusTemporaryRedirect)
		case s.down:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case s.limited:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1791730277")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"message": "API rate limit exceeded"})
		default:
			h(w, r)
		}
	}
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
}

// signedInAccount resolves the bearer token of a request.
func (s *Server) signedInAccount(w http.ResponseWriter, r *http.Request) (int64, *account) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, ok := s.tokens[token]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials"})
		return 0, nil
	}
	return id, s.accounts[id]
}

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	if id, a := s.signedInAccount(w, r); a != nil {
		json.NewEncoder(w).Encode(github.Account{ID: id, Login: a.login})
	}
}

func (s *Server) accountByID(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a := s.accounts[id]
	if a == nil {
		notFound(w)
		return
	}
	json.NewEncoder(w).Encode(github.Account{ID: id, Login: a.login})
}

func (s *Server) publicKeys(w http.ResponseWriter, r *http.Request) {
	for _, a := range s.accounts {
		if a.login == r.PathValue("login") {
			writePage(w, r, a.keys)
			return
		}
	}
	notFound(w)
}

func (s *Server) viewerKeys(w http.ResponseWriter, r *http.Request) {
	if _, a := s.signedInAccount(w, r); a != nil {
		writePage(w, r, a.keys)
	}
}

func writePage(w http.ResponseWriter, r *http.Request, keys []github.SigningKey) {
	perPage, _ := strconv.Atoi(r.FormValue("per_page"))
	page, _ := strconv.Atoi(r.FormValue("page"))
	if perPage <= 0 {
		perPage = 30
	}
	start := min(max(page-1, 0)*perPage, len(keys))
	end := min(start+perPage, len(keys))
	out := keys[start:end]
	if out == nil {
		out = []github.SigningKey{}
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) addKey(w http.ResponseWriter, r *http.Request) {
	_, a := s.signedInAccount(w, r)
	if a == nil {
		return
	}
	var in struct{ Title, Key string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, k := range a.keys {
		if k.Key == in.Key {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]string{"message": "key is already in use"})
			return
		}
	}
	s.nextKey++
	key := github.SigningKey{ID: s.nextKey, Title: in.Title, Key: in.Key}
	a.keys = append(a.keys, key)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(key)
}

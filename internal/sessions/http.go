package sessions

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// PeerRoutes adds what this node offers other nodes: its feed and the
// messages of its sessions. The mux must only be reachable by nodes in the
// roster.
func (s *Service) PeerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		pg, err := s.LocalChanges(r.Context(), after, limitParam(q.Get("limit"), MaxPage))
		answer(w, pg, err)
	})
	mux.HandleFunc("GET /v1/sessions/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		pg, err := s.LocalMessages(r.Context(), q.Get("key"), after, limitParam(q.Get("limit"), MaxPage))
		answer(w, pg, err)
	})
}

// ControlRoutes adds what the node's own tools ask it: the sessions of the
// whole network, and a session to read.
func (s *Service) ControlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		list, err := s.List(r.Context(), Query{
			Node: q.Get("node"), Agent: q.Get("agent"), Project: q.Get("project"),
			Since: since, Text: q["text"], Limit: limit,
		})
		answer(w, list, err)
	})
	mux.HandleFunc("GET /v1/sessions/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		reading, err := s.Read(r.Context(), q.Get("node"), q.Get("key"), after, limitParam(q.Get("limit"), 50))
		answer(w, reading, err)
	})
}

func answer(w http.ResponseWriter, v any, err error) {
	var notCached *NotCachedError
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoIndex):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.As(err, &notCached):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

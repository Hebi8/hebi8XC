package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PeerRoutes adds what this node offers other nodes: its feed and the
// messages of its sessions. The mux must only be reachable by nodes in the
// roster.
func (s *Service) PeerRoutes(mux *http.ServeMux) {
	s.searchRoute(mux, true)
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
	s.ReadRoutes(mux)
	mux.HandleFunc("GET /v1/sessions/export", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		options := ExportOptions{Epoch: q.Get("epoch"), After: after, Limit: limitParam(q.Get("limit"), 100)}
		for name, dst := range map[string]**int64{"ack": &options.Ack, "since": &options.Since} {
			if q.Has(name) {
				n, err := strconv.ParseInt(q.Get(name), 10, 64)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				*dst = &n
			}
		}
		pg, err := s.Export(r.Context(), options)
		answer(w, pg, err)
	})

}

// ReadRoutes exposes only listing and reading, also used by the web interface.
func (s *Service) ReadRoutes(mux *http.ServeMux) {
	s.searchRoute(mux, false)
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		list, err := s.List(r.Context(), Query{
			Node: q.Get("node"), Agent: q.Get("agent"), Project: q.Get("project"),
			Since: since, Text: q["text"], Limit: limit, After: q.Get("after"),
		})
		answer(w, list, err)
	})
	mux.HandleFunc("GET /v1/sessions/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		ctx := r.Context()
		var reading Reading
		var err error
		if q.Has("around") {
			var id int64
			var before, following int
			var rev *int64
			id, err = strconv.ParseInt(q.Get("around"), 10, 64)
			before, following = 8, 2
			if err == nil && q.Has("before") {
				before, err = strconv.Atoi(q.Get("before"))
			}
			if err == nil && q.Has("after") {
				following, err = strconv.Atoi(q.Get("after"))
			}
			if err == nil && q.Has("rev") {
				var n int64
				n, err = strconv.ParseInt(q.Get("rev"), 10, 64)
				rev = &n
			}
			if err != nil || id < 1 || before < 0 || following < 0 || before >= MaxPage || following > MaxPage-1-before {
				http.Error(w, "around requires a positive id and a window of at most 500 messages", http.StatusBadRequest)
				return
			}
			reading, err = s.Around(ctx, q.Get("node"), q.Get("key"), id, before, following, rev)
		} else {
			reading, err = s.Read(ctx, q.Get("node"), q.Get("key"), after, limitParam(q.Get("limit"), 50))
		}
		if code := Code(err); code != "" {
			w.Header().Set(ErrorHeader, code)
		}
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
	case strings.HasPrefix(err.Error(), "after is not"), strings.HasPrefix(err.Error(), "no node "),
		strings.HasPrefix(err.Error(), "several nodes"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Service) searchRoute(mux *http.ServeMux, local bool) {
	mux.HandleFunc("GET /v1/sessions/search", func(w http.ResponseWriter, r *http.Request) {
		q, err := ParseSearch(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if local {
			pg, err := s.LocalSearch(ctx, q)
			answer(w, pg, err)
		} else {
			pg, err := s.Search(ctx, q)
			answer(w, pg, err)
		}
	})
}

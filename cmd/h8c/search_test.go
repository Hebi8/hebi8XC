package main

import (
	"bytes"
	"encoding/json"
	"github.com/Hebi8/hebi8XC/internal/sessions/wake/waketest"
	"testing"
	"time"

	"github.com/Hebi8/hebi8XC/internal/sessions"
)

func TestSearchCommandAndAround(t *testing.T) {
	desk, laptop, fixture := twoMachines(t)
	var r sessions.SearchResult
	if e := json.Unmarshal([]byte(h8c(t, laptop, "search", "parser", "--node", "desk", "--json")), &r); e != nil {
		t.Fatal(e)
	}
	if len(r.Hits) != 1 || r.Hits[0].ID == 0 || r.Hits[0].Rev == 0 || r.Hits[0].Ref != "desk/claude:a1" || r.Nodes[0].Searched != "live" {
		t.Fatal(r)
	}
	var grouped sessions.SearchResult
	if e := json.Unmarshal([]byte(h8c(t, laptop, "search", "parser", "--node", "desk", "--by-session", "--json")), &grouped); e != nil {
		t.Fatal(e)
	}
	if len(grouped.Sessions) != 1 || grouped.Sessions[0].Hits != 1 {
		t.Fatal(grouped)
	}
	var reading sessions.Reading
	if e := json.Unmarshal([]byte(h8c(t, laptop, "show", r.Hits[0].Ref, "--around", "2", "--before", "1", "--after", "1", "--rev", "0", "--json")), &reading); e != nil {
		t.Fatal(e)
	}
	if len(reading.Messages) != 3 || !reading.Moved || reading.Rev == 0 {
		t.Fatal(reading)
	}
	fixture.Structure("claude:a1", 1, sessions.Message{})
	fixture.Exec("UPDATE message_extras SET kind='meta' WHERE session_key='claude:a1' AND seq=1")
	for _, tt := range []struct {
		args []string
		want int
	}{{[]string{"show", "desk/claude:a1", "--json"}, 2}, {[]string{"show", "desk/claude:a1", "--all", "--json"}, 3}} {
		var r sessions.Reading
		if e := json.Unmarshal([]byte(h8c(t, desk, tt.args...)), &r); e != nil || len(r.Messages) != tt.want {
			t.Fatal(len(r.Messages), e)
		}
	}

}

func TestSearchUntilIncludesWholeDayAndShowKey(t *testing.T) {
	desk, laptop, fixture := twoMachines(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	fixture.Put(waketest.Session{Key: "claude:a1", Agent: "claude", UpdatedAt: day.UnixMilli()}, waketest.Message{Text: "needle", Time: day.Add(23 * time.Hour).UnixMilli()}, waketest.Message{Text: "needle", Time: day.AddDate(0, 0, 1).UnixMilli()})
	var r sessions.SearchResult
	if e := json.Unmarshal([]byte(h8c(t, desk, "search", "needle", "--until", "2026-10-01", "--json")), &r); e != nil || len(r.Hits) != 1 {
		t.Fatal(r, e)
	}
	var read sessions.Reading
	if e := json.Unmarshal([]byte(h8c(t, laptop, "show", "claude:a1", "--json")), &read); e != nil || read.Session.Ref != "desk/claude:a1" {
		t.Fatal(read, e)
	}
	for _, args := range [][]string{{"show", "claude:a1", "--around", "0"}, {"show", "claude:a1", "--rev", "1"}} {
		var out bytes.Buffer
		if e := run(append([]string{"--data-dir", desk}, args...), &out); e == nil {
			t.Fatal(args)
		}
	}
}

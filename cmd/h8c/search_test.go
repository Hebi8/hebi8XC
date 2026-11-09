package main

import (
	"encoding/json"
	"testing"

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

// Package sessions makes the AI coding agent sessions of every machine
// visible from any of them. A node publishes the index of its own sessions
// to its peers, keeps a copy of theirs, and caches what it reads from them,
// so the sessions of a machine that is switched off can still be listed and,
// where they were read before, read.
package sessions

import (
	"context"
	"encoding/json"
	"errors"
)

// Session describes one agent session. It is what nodes tell each other
// about a session, and what the index of a machine has to provide.
type Session struct {
	Key         string `json:"key"` // unique on its machine, "<agent>:<id>"
	Agent       string `json:"agent"`
	Title       string `json:"title"`
	Project     string `json:"project"`
	ProjectPath string `json:"project_path"`
	Branch      string `json:"branch,omitempty"`
	Model       string `json:"model,omitempty"`
	CreatedAt   int64  `json:"created_at"` // Unix milliseconds
	UpdatedAt   int64  `json:"updated_at"`
	Messages    int64  `json:"messages"`
	Parent      string `json:"parent,omitempty"`  // key of the session this one belongs under
	Spawned     bool   `json:"spawned,omitempty"` // started by an agent, not by the user
	Archived    bool   `json:"archived,omitempty"`

	// Stamp changes whenever the content of the session may have changed.
	// It stays on the machine.
	Stamp string `json:"-"`
}

// Message is one message of a session.
type Message struct {
	// ID grows from one message of a session to the next. It is the cursor
	// for reading a session in pages and has no other meaning.
	ID        int64  `json:"id"`
	Seq       int64  `json:"seq"`                 // number of the message as the agent's own tools show it
	Sidechain string `json:"sidechain,omitempty"` // set for messages of a side thread, such as a subagent's
	Role      string `json:"role"`
	Time      int64  `json:"time,omitempty"` // Unix milliseconds, 0 if unknown
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"` // Text is only the beginning
}

// Source is the index of the sessions on this machine. The first one reads
// the index that Wake keeps; see package wake.
type Source interface {
	// Sessions returns every session of this machine.
	Sessions(ctx context.Context) ([]Session, error)
	// Messages returns the messages of a session whose ID is above after,
	// in order: at most limit of them, and no more than fit into budget
	// bytes by MessageSize, but always one. It must stop reading there, not
	// read on and discard. more says whether the session goes on. Text
	// longer than MaxText characters is cut and marked.
	Messages(ctx context.Context, key string, after int64, limit, budget int) (msgs []Message, more bool, err error)
}

// MessageSize is the size a message counts for: that of its JSON, which is
// about what it takes to send and to store.
func MessageSize(m Message) int {
	raw, _ := json.Marshal(m)
	return len(raw) + 1
}

// Limits on what one node hands another, and on what it accepts.
const (
	// MaxText is the longest message text that is passed on, in characters.
	MaxText = 200_000
	// MaxPage is the most sessions or messages in one answer.
	MaxPage = 500
	// PageBudget is the size an answer may reach before it is cut off, in
	// bytes of JSON. One message is always sent, however long.
	PageBudget = 1 << 20
	// MaxReply is the largest answer a node reads from a peer. A page stays
	// below it: PageBudget plus one message of MaxText characters.
	MaxReply = 8 << 20
	// maxField is the longest title, path or other descriptive field kept.
	maxField = 4096
	// maxSessionsPerNode bounds how many sessions of one peer are kept.
	maxSessionsPerNode = 200_000
)

// The copy of one session stops at maxCopyBytes, counted by MessageSize, or
// after maxCopyPages answers, whichever comes first. Variables so that
// tests can reach them.
var (
	maxCopyBytes = 256 << 20
	maxCopyPages = 2000
)

var (
	// ErrNotFound means the index has no such session.
	ErrNotFound = errors.New("no such session")
	// ErrNoIndex means this machine has no session index to read.
	ErrNoIndex = errors.New("this machine has no session index")
)

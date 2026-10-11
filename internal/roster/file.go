package roster

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

// Entry is one trusted key and the comment stored next to it.
type Entry struct {
	Key     ed25519.PublicKey
	Comment string
}

// File is a Roster backed by a local file with one "ssh-ed25519 ..." key per
// line. Blank lines and lines starting with # are ignored. The file belongs
// to a single user, so the user argument of Keys and Refresh is not used.
type File struct {
	path string

	mu      sync.RWMutex
	entries []Entry
}

// OpenFile loads the roster at path. A missing file is an empty roster.
func OpenFile(path string) (*File, error) {
	f := &File{path: path}
	if err := f.Refresh(context.Background(), ""); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *File) Keys(string) ([]ed25519.PublicKey, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	keys := make([]ed25519.PublicKey, len(f.entries))
	for i, e := range f.entries {
		keys[i] = e.Key
	}
	return keys, nil
}

// Refresh re-reads the file. On error the previously loaded keys stay in use.
func (f *File) Refresh(context.Context, string) error {
	lines, err := f.readLines()
	if err != nil {
		return err
	}
	var entries []Entry
	for i, line := range lines {
		if isBlank(line) {
			continue
		}
		key, comment, err := sshkey.Parse(line)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", f.path, i+1, err)
		}
		entries = append(entries, Entry{Key: key, Comment: comment})
	}
	f.mu.Lock()
	f.entries = entries
	f.mu.Unlock()
	return nil
}

func (f *File) Entries() []Entry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]Entry(nil), f.entries...)
}

// Add appends key to the file. It reports false if the key was already there.
func (f *File) Add(key ed25519.PublicKey, comment string) (bool, error) {
	for _, e := range f.Entries() {
		if e.Key.Equal(key) {
			return false, nil
		}
	}
	lines, err := f.readLines()
	if err != nil {
		return false, err
	}
	line := sshkey.Marshal(key)
	if comment != "" {
		line += " " + comment
	}
	return true, f.writeLines(append(lines, line))
}

// Remove deletes every entry that match identifies: a key line, a comment or
// a fingerprint. It returns how many entries were removed.
func (f *File) Remove(match string) (int, error) {
	matchKey, _, _ := sshkey.Parse(match)
	lines, err := f.readLines()
	if err != nil {
		return 0, err
	}
	var kept []string
	removed := 0
	for _, line := range lines {
		if !isBlank(line) {
			key, comment, err := sshkey.Parse(line)
			if err == nil && (key.Equal(matchKey) || comment == match || sshkey.Fingerprint(key) == match) {
				removed++
				continue
			}
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, f.writeLines(kept)
}

func isBlank(line string) bool {
	line = strings.TrimSpace(line)
	return line == "" || strings.HasPrefix(line, "#")
}

func (f *File) readLines() ([]string, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(raw), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

func (f *File) writeLines(lines []string) error {
	text := ""
	if len(lines) > 0 {
		text = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(f.path, []byte(text), 0o644); err != nil {
		return err
	}
	return f.Refresh(context.Background(), "")
}

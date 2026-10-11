// Package datadir looks after the node's data directory: who may enter it,
// the lock that allows one running node per directory, and file writes that
// leave either the old content or the new, never half of it.
package datadir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const lockFile = "lock"

// ErrLocked means a running node holds the directory's lock.
var ErrLocked = errors.New("a node is already running in this data directory")

// Ensure creates dir if it is missing and restricts it to the user, also
// when it already existed with wider permissions. Everything in the
// directory, the control socket included, relies on this.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Lock takes the exclusive lock of dir without waiting and returns the
// function that releases it. The lock dies with the process, so a crashed
// node never blocks the next one.
func Lock(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// WriteFile replaces the file at path with data in one step: it writes a
// temporary file next to it and renames that over path.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // fails once the rename has happened

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

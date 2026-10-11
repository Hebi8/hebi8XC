// Package roster answers one question: which public keys belong to a user.
// A node that proves it holds one of those keys is one of the user's machines.
package roster

import (
	"context"
	"crypto/ed25519"
	"errors"
)

// Roster is a per-user set of trusted public keys with a local cache.
//
// Keys is called during every handshake, so it must answer from the cache.
// Refresh is where an implementation talks to its source of truth: the file
// roster re-reads its file; the GitHub roster fetches the account's SSH
// signing keys and writes them to its cache.
type Roster interface {
	Keys(user string) ([]ed25519.PublicKey, error)
	Refresh(ctx context.Context, user string) error
}

// Trusts reports whether key is in r's cached set for user.
func Trusts(r Roster, user string, key ed25519.PublicKey) bool {
	keys, err := r.Keys(user)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if k.Equal(key) {
			return true
		}
	}
	return false
}

// Union returns a roster that trusts every key one of rs trusts. Its
// Refresh refreshes all of them and reports every failure.
func Union(rs ...Roster) Roster {
	return union(rs)
}

type union []Roster

func (u union) Keys(user string) ([]ed25519.PublicKey, error) {
	var all []ed25519.PublicKey
	for _, r := range u {
		keys, err := r.Keys(user)
		if err != nil {
			return nil, err
		}
		all = append(all, keys...)
	}
	return all, nil
}

func (u union) Refresh(ctx context.Context, user string) error {
	var errs []error
	for _, r := range u {
		errs = append(errs, r.Refresh(ctx, user))
	}
	return errors.Join(errs...)
}

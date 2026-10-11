package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Hebi8/hebi8XC/internal/datadir"
)

// LoadAccount reads the account a node is logged in to. ok is false when
// the node has never logged in.
func LoadAccount(path string) (a Account, ok bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return Account{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if a.ID <= 0 {
		return Account{}, false, fmt.Errorf("%s: missing account id", path)
	}
	return a, true, nil
}

// SaveAccount records the account a node is logged in to. Only the id and
// the login are stored, never a token.
func SaveAccount(path string, a Account) error {
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return datadir.WriteFile(path, append(raw, '\n'), 0o644)
}

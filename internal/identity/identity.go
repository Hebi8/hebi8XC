// Package identity holds what makes this node the same node across restarts
// and address changes: an id, a display name and an Ed25519 key pair.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	infoFile = "node.json"
	keyFile  = "node.key"
)

type Identity struct {
	ID   string
	Name string
	Key  ed25519.PrivateKey
}

func (i *Identity) Public() ed25519.PublicKey {
	return i.Key.Public().(ed25519.PublicKey)
}

type info struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Load reads the identity stored in dir. If dir holds no identity yet, it
// generates one, named after the host, and stores it.
func Load(dir string) (*Identity, error) {
	_, infoErr := os.Stat(filepath.Join(dir, infoFile))
	_, keyErr := os.Stat(filepath.Join(dir, keyFile))
	if errors.Is(infoErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist) {
		return create(dir)
	}
	return load(dir)
}

func load(dir string) (*Identity, error) {
	infoPath, keyPath := filepath.Join(dir, infoFile), filepath.Join(dir, keyFile)
	raw, err := os.ReadFile(infoPath)
	if err != nil {
		return nil, err
	}
	var in info
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%s: %w", infoPath, err)
	}
	if in.ID == "" {
		return nil, fmt.Errorf("%s: missing id", infoPath)
	}

	raw, err = os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM file", keyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 key", keyPath)
	}
	return &Identity{ID: in.ID, Name: in.Name, Key: key}, nil
}

// create never overwrites: the key file is created exclusively, and whoever
// loses that race loads the winner's identity instead of keeping its own.
func create(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	name, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ident := &Identity{ID: hex.EncodeToString(id), Name: name, Key: key}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	err = writeNew(filepath.Join(dir, keyFile), keyPEM, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return load(dir)
	}
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(info{ID: ident.ID, Name: ident.Name}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(dir, infoFile), append(raw, '\n'), 0o644); err != nil {
		return nil, err
	}
	return ident, nil
}

// writeNew writes a file that must not exist yet.
func writeNew(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

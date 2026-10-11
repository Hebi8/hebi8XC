// Package sshkey reads and writes Ed25519 public keys in the OpenSSH
// one-line format ("ssh-ed25519 AAAA... comment"), the same format GitHub
// uses for SSH signing keys.
package sshkey

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
)

const algo = "ssh-ed25519"

// wire returns the SSH wire encoding of pub: two length-prefixed strings,
// the algorithm name and the raw key.
func wire(pub ed25519.PublicKey) []byte {
	b := make([]byte, 0, 4+len(algo)+4+ed25519.PublicKeySize)
	b = binary.BigEndian.AppendUint32(b, uint32(len(algo)))
	b = append(b, algo...)
	b = binary.BigEndian.AppendUint32(b, ed25519.PublicKeySize)
	return append(b, pub...)
}

// Marshal returns "ssh-ed25519 <base64>" for pub, without a comment.
func Marshal(pub ed25519.PublicKey) string {
	return algo + " " + base64.StdEncoding.EncodeToString(wire(pub))
}

// Parse reads one "ssh-ed25519 <base64> [comment]" line.
func Parse(line string) (pub ed25519.PublicKey, comment string, err error) {
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != algo {
		return nil, "", errors.New("want \"ssh-ed25519 <base64> [comment]\"")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return nil, "", errors.New("key is not valid base64")
	}
	header := wire(nil)
	if len(blob) != len(header)+ed25519.PublicKeySize || !bytes.HasPrefix(blob, header) {
		return nil, "", errors.New("not an ssh-ed25519 key")
	}
	return ed25519.PublicKey(blob[len(header):]), strings.Join(f[2:], " "), nil
}

// Fingerprint returns the "SHA256:..." fingerprint that ssh-keygen -l prints.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(wire(pub))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

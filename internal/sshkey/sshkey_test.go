package sshkey

import (
	"crypto/ed25519"
	"testing"
)

// Produced by ssh-keygen -t ed25519; the fingerprint is from ssh-keygen -l.
const (
	vectorLine        = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFWeVDEg+tjCem68s7Pd5m2o9PAN+Hv6WuzNBnpaIUmr vector"
	vectorFingerprint = "SHA256:k8P4ZF+l1an6EWbNptbysCvw6nbiOSMK1/lbcwp3pF0"
)

func TestParseMatchesSSHKeygen(t *testing.T) {
	pub, comment, err := Parse(vectorLine)
	if err != nil {
		t.Fatal(err)
	}
	if comment != "vector" {
		t.Errorf("comment = %q", comment)
	}
	if got := Fingerprint(pub); got != vectorFingerprint {
		t.Errorf("fingerprint = %s, want %s", got, vectorFingerprint)
	}
	if got := Marshal(pub) + " vector"; got != vectorLine {
		t.Errorf("Marshal = %s", got)
	}
}

func TestRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	got, comment, err := Parse(Marshal(pub))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(pub) || comment != "" {
		t.Errorf("round trip changed the key or added a comment %q", comment)
	}
}

func TestParseRejects(t *testing.T) {
	for _, line := range []string{
		"",
		"ssh-ed25519",
		"ssh-ed25519 not-base64!",
		"ssh-ed25519 AAAA",
		"ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIFWeVDEg+tjCem68s7Pd5m2o9PAN+Hv6WuzNBnpaIUmr",
	} {
		if _, _, err := Parse(line); err == nil {
			t.Errorf("Parse(%q) succeeded", line)
		}
	}
}

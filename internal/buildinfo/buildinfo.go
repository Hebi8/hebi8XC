// Package buildinfo says which build of h8c this is, from what the Go
// toolchain records in every binary. Nothing is passed in at build time.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// read is debug.ReadBuildInfo, except in tests.
var read = debug.ReadBuildInfo

// Version is the module version the binary was built from, such as
// "v0.3.1" for an installed release or a pseudo-version for a build from a
// checkout. It is "devel" when the toolchain recorded none.
func Version() string {
	info, ok := read()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "devel"
	}
	return info.Main.Version
}

// Commit is the revision the binary was built from, with "-modified" if
// the checkout had changes. It is empty when the toolchain recorded none,
// as for a binary installed from the module proxy.
func Commit() string {
	info, ok := read()
	if !ok {
		return ""
	}
	var rev string
	modified := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		rev += "-modified"
	}
	return rev
}

// String names the build in one word, for telling builds apart: the
// version, and for a build without one the commit.
func String() string {
	v, c := Version(), Commit()
	if v == "devel" && c != "" {
		return "devel-" + strings.TrimSuffix(c, "-modified")
	}
	return v
}

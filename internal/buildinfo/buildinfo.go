// Package buildinfo says which build of h8c this is, from what the Go
// toolchain records in every binary. Nothing is passed in at build time.
package buildinfo

import (
	"regexp"
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

// pseudo matches the version the toolchain gives a build from a checkout
// that is not at a tag: a base version, a time and twelve characters of the
// commit, as in "v0.3.2-0.20261013104500-0123456789ab", with "+dirty" if
// the checkout had changes.
var pseudo = regexp.MustCompile(`^v\d+\.\d+\.\d+-(?:.*\.)?\d{14}-([0-9a-f]{12})(?:\+.*)?$`)

// Short names a build, this one's or a peer's, in a few characters, for a
// table or a page: a release by its version, and a build from a checkout,
// whose version is long and says little, by seven characters of its
// commit. Where there is room, the whole string says more.
func Short(build string) string {
	if m := pseudo.FindStringSubmatch(build); m != nil {
		return m[1][:7]
	}
	if commit, ok := strings.CutPrefix(build, "devel-"); ok && len(commit) > 7 {
		return commit[:7]
	}
	return build
}

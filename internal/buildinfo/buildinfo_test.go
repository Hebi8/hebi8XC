package buildinfo

import (
	"runtime/debug"
	"testing"
)

func with(t *testing.T, info *debug.BuildInfo) {
	old := read
	read = func() (*debug.BuildInfo, bool) { return info, info != nil }
	t.Cleanup(func() { read = old })
}

func TestBuildInfo(t *testing.T) {
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}
	for name, tc := range map[string]struct {
		info                    *debug.BuildInfo
		version, commit, string string
	}{
		"nothing recorded":  {nil, "devel", "", "devel"},
		"go test, go run":   {&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "devel", "", "devel"},
		"installed release": {&debug.BuildInfo{Main: debug.Module{Version: "v0.3.1"}}, "v0.3.1", "", "v0.3.1"},
		"built in a checkout": {&debug.BuildInfo{Main: debug.Module{Version: "v0.3.2-0.20261013104500-0123456789ab"},
			Settings: []debug.BuildSetting{rev}}, "v0.3.2-0.20261013104500-0123456789ab", "0123456789ab", "v0.3.2-0.20261013104500-0123456789ab"},
		"checkout without a version": {&debug.BuildInfo{Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{rev, dirty}}, "devel", "0123456789ab-modified", "devel-0123456789ab"},
	} {
		with(t, tc.info)
		if Version() != tc.version || Commit() != tc.commit || String() != tc.string {
			t.Errorf("%s: Version %q, Commit %q, String %q", name, Version(), Commit(), String())
		}
	}
}

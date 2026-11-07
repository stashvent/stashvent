package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	module := func(version string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: version}}
	}

	tests := []struct {
		name    string
		version string
		info    *debug.BuildInfo
		want    string
	}{
		{"set with -ldflags", "v0.2.0", module("v0.1.0"), "v0.2.0"},
		{"go install of a release", "dev", module("v0.1.0"), "v0.1.0"},
		{"go install of a release candidate", "dev", module("v0.1.0-rc.1"), "v0.1.0-rc.1"},
		{"go install of a commit after a tag", "dev", module("v0.1.1-0.20261101120000-d38bf9e1a2b3"), "dev"},
		{"go install of a commit before any tag", "dev", module("v0.0.0-20261101120000-d38bf9e1a2b3"), "dev"},
		{"go install of a commit after a pre-release", "dev", module("v0.1.0-rc.1.0.20261101120000-d38bf9e1a2b3"), "dev"},
		{"checkout with local changes", "dev", module("v0.1.0+dirty"), "dev"},
		{"checkout of a commit with local changes", "dev", module("v0.1.1-0.20261101120000-d38bf9e1a2b3+dirty"), "dev"},
		{"no module version", "dev", module("(devel)"), "dev"},
		{"no build info", "dev", nil, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVersion(tt.version, tt.info); got != tt.want {
				t.Errorf("buildVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

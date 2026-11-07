// Command stashvent is the StashVent CLI: push, pull, list, and delete. The
// commands live in the cmd package; see the stashvent-cli skill.
package main

import (
	"os"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/stashvent/stashvent/apps/stashvent/cmd"
)

// version and commit are set at build time:
// -ldflags "-X main.version=<version> -X main.commit=<commit>".
var (
	version = "dev"
	commit  = ""
)

func main() {
	info, _ := debug.ReadBuildInfo()
	os.Exit(cmd.Execute(buildVersion(version, info), commit, os.Args[1:]))
}

// pseudoVersion matches the end of the version Go records for an untagged
// commit: its time and hash.
var pseudoVersion = regexp.MustCompile(`[.-][0-9]{14}-[0-9a-f]{12}$`)

// buildVersion falls back to the module version when -ldflags didn't set
// one (go install …@v0.1.0), but only for a release: an untagged commit or
// uncommitted changes ("+dirty") stay "dev".
func buildVersion(version string, info *debug.BuildInfo) string {
	if version != "dev" || info == nil {
		return version
	}
	v := info.Main.Version
	if !strings.HasPrefix(v, "v") || strings.Contains(v, "+") || pseudoVersion.MatchString(v) {
		return version
	}
	return v
}

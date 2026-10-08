// Package version carries the build identity. The values are injected at
// build time with -ldflags -X, so no file in the repository has to be edited
// to cut a release.
package version

import (
	"fmt"
	"runtime/debug"
)

// Set by GoReleaser:
//
//	-X github.com/thyarles/lhc/internal/version.Version=1.2.3
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String is the one-line form printed by `lhc version`.
func String() string {
	commit, date := Commit, Date
	if commit == "" || date == "" {
		// A plain `go build` has no ldflags, but the toolchain still stamps
		// the VCS revision into the binary. Use it rather than print nothing.
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch {
				case s.Key == "vcs.revision" && commit == "":
					commit = s.Value
				case s.Key == "vcs.time" && date == "":
					date = s.Value
				}
			}
		}
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if commit == "" {
		commit = "unknown"
	}
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("lhc %s (commit %s, built %s)", Version, commit, date)
}

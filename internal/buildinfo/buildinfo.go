// Package buildinfo holds the version stamped into the binaries at build
// time with -ldflags (see Makefile and .goreleaser.yaml):
//
//	-X github.com/sebasBetancourt/relay/internal/buildinfo.Version=v0.1.0
//
// Unstamped builds (go run, go test) report "dev" and the VCS data Go
// embeds on its own, when available.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String is a one-line description, e.g. "v0.1.0 (abc1234, 2026-10-25T10:00:00Z)".
func String() string {
	commit, date := Commit, Date
	if commit == "" || date == "" {
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
	if len(commit) > 7 {
		commit = commit[:7]
	}
	if commit == "" {
		return Version
	}
	return fmt.Sprintf("%s (%s, %s)", Version, commit, date)
}

// Package version reports the version of this tele binary.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is set at release builds with
// -ldflags "-X github.com/ujzk/tele-agent/internal/version.Version=v1.2.3".
var Version string

// String returns Version, or for a development build the VCS revision the
// Go toolchain recorded ("devel+<rev>[-dirty]"), or "devel".
func String() string {
	if Version != "" {
		return Version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "devel"
	}
	var b strings.Builder
	b.WriteString("devel+")
	b.WriteString(rev[:min(12, len(rev))])
	if dirty {
		b.WriteString("-dirty")
	}
	return b.String()
}

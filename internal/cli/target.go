// Package cli parses the target argument of the tele command line,
// "<alias>[:<dir>]" (docs/cli.md section 1).
//
// The alias is split off at the first ':' in scp/rsync style, so an alias can
// never contain ':' while the directory may. The directory is a path on the
// remote host and is resolved against the remote home directory, which is
// only known once the session is up; hence parsing and resolution are
// separate steps.
package cli

import (
	"fmt"
	"path"
	"strings"

	"github.com/ujzk/tele-agent/internal/proto"
)

// Target is a parsed "<alias>[:<dir>]" argument.
type Target struct {
	Alias string
	// Dir is the directory as written, minus trailing slashes; "" means the
	// remote home directory. It is resolved with ResolveDir.
	Dir string
}

// reserved lists the subcommands that share the argument position with the
// alias; an alias equal to one of them could never be selected.
var reserved = map[string]struct{}{
	"host":    {},
	"doctor":  {},
	"server":  {},
	"help":    {},
	"version": {},
}

// IsReserved reports whether alias is a tele subcommand name and therefore
// cannot name a host. The comparison is exact, matching subcommand dispatch.
func IsReserved(alias string) bool {
	_, ok := reserved[alias]
	return ok
}

// CheckAlias validates a host alias: non-empty, [A-Za-z0-9][A-Za-z0-9._-]*,
// and not a reserved word.
func CheckAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("missing host alias; usage: tele [options] <alias>[:<dir>] [claude args...]")
	}
	for i := range len(alias) {
		c := alias[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '.' && c != '_' && c != '-') {
			return fmt.Errorf("invalid host alias %q: use letters, digits, '.', '_' and '-', starting with a letter or digit", alias)
		}
	}
	if IsReserved(alias) {
		return fmt.Errorf("%q is a tele command, not a host alias; run \"tele help\" for usage", alias)
	}
	return nil
}

// ParseTarget parses "<alias>[:<dir>]". Everything after the first ':' is
// the directory, which may itself contain ':'. Trailing slashes are removed
// from the directory, except for "/" itself.
func ParseTarget(s string) (Target, error) {
	alias, dir, _ := strings.Cut(s, ":")
	if err := CheckAlias(alias); err != nil {
		return Target{}, err
	}
	if strings.IndexByte(dir, 0) >= 0 {
		return Target{}, fmt.Errorf("directory %q contains a NUL byte", dir)
	}
	for len(dir) > 1 && dir[len(dir)-1] == '/' {
		dir = dir[:len(dir)-1]
	}
	return Target{Alias: alias, Dir: dir}, nil
}

// ResolveDir turns a Target.Dir into an absolute, clean path on the remote
// host: "" is home, and a relative dir is taken relative to home, the way
// scp resolves "host:path". home is the remote user's home directory and
// must be absolute. Whether the directory exists is checked by the caller
// on the remote side.
func ResolveDir(dir, home string) (string, error) {
	if !path.IsAbs(home) || strings.IndexByte(home, 0) >= 0 {
		return "", fmt.Errorf("remote home directory %q is not an absolute path", home)
	}
	if strings.IndexByte(dir, 0) >= 0 {
		return "", fmt.Errorf("directory %q contains a NUL byte", dir)
	}
	switch {
	case dir == "":
		dir = home
	case !path.IsAbs(dir):
		dir = path.Join(home, dir)
	}
	dir = path.Clean(dir)
	if err := proto.CheckPath(dir); err != nil {
		// Unreachable given the checks above; kept so that a future change
		// cannot hand the remote side a path it would reject.
		return "", fmt.Errorf("resolve directory: %w", err)
	}
	return dir, nil
}

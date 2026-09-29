package telefs

import (
	"fmt"
	"strings"

	"github.com/ujzk/tele-agent/internal/proto"
)

// LocalNames serves the entries of directory Dir of the view whose names
// start with Prefix from another file service, instead of the target
// host's: session main runs an fssvc.Service rooted at the local HOME in
// its own process and passes it as Opener. Claude writes ~/.claude.json
// through temporary files renamed over it and guards it with a lock
// directory, all named .claude.json* in HOME; a bind mount of the single
// file cannot take that, so the whole name family is local
// (docs/claude-code.md "~/.claude.json").
//
// The local names are the entries of the same names in the root of the
// Opener's file system. Dir becomes a synthetic directory like the
// ancestors of placeholders. Entries created there under other names stay
// remote, and a rename or link between a local and a remote name fails
// with EXDEV.
type LocalNames struct {
	Dir    string
	Prefix string
	Opener Opener
}

// localDir is the LocalNames rule of a synthetic directory.
type localDir struct {
	prefix string
	be     *backend
}

// owns reports whether entry name of the directory is local.
func (l *localDir) owns(name string) bool {
	return l != nil && strings.HasPrefix(name, l.prefix)
}

func checkLocalNames(ls []LocalNames) error {
	for _, l := range ls {
		if err := proto.CheckPath(l.Dir); err != nil {
			return fmt.Errorf("telefs: local names: invalid directory %q", l.Dir)
		}
		if proto.CheckName(l.Prefix) != nil {
			return fmt.Errorf("telefs: local names in %s: invalid prefix %q", l.Dir, l.Prefix)
		}
		if l.Opener == nil {
			return fmt.Errorf("telefs: local names in %s: no opener", l.Dir)
		}
	}
	return nil
}

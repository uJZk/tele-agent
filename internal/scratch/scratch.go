// Package scratch is session main's side of the scratch areas
// (docs/exec.md "scratch 路径改写与回传").
//
// Claude opens a few files locally that remote commands also use: the cwd
// file in CLAUDE_CODE_TMPDIR, shell snapshots, and CLAUDE_ENV_FILE
// (docs/claude-code.md "scratch 文件"). A Mapper rewrites their path prefixes
// in everything session main sends to the target, collects local changes
// to upload before a command, and applies the files the target reports as
// changed afterwards.
//
// A command's local side goes like this: Hold its output files, take
// Uploads with the budget of its ExecStart, start it with the Upload's
// files, Commit the Upload once it started (Rollback if it did not),
// Apply the files its exit status reports, and release the output files
// once all output was written.
//
// The shell-snapshots and session-env directories are in Claude's config
// directory, which every local Claude session shares. Of those, only this
// session's files are uploaded: the ones its target produced (recorded by
// Apply), and the entries its commands name (see Rewrite), such as its own
// session-env/<Claude session id> directory. Anything else there belongs
// to other sessions, possibly to other hosts, and may hold credentials a
// SessionStart hook wrote.
//
// The target is not trusted (docs/security.md "远端返回的数据"): Apply writes only
// below the local area directories, through os.Root, so neither ".."
// components nor symbolic links can make it touch any other local file.
package scratch

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/ujzk/tele-agent/internal/proto"
)

// Area is one scratch area as each party sees it.
type Area struct {
	ID proto.ScratchArea
	// ClaudePath is the area directory as Claude sees it: the prefix that
	// Rewrite replaces.
	ClaudePath string
	// LocalPath is the same directory in session main's view.
	LocalPath string
	// RemotePath is the area directory on the target host.
	RemotePath string
}

// Mapper maps the scratch areas of one session. Its methods may be called
// concurrently.
type Mapper struct {
	// Logger receives diagnostics about files that were not transferred.
	// Set it before the Mapper is used; nil discards them.
	Logger *slog.Logger

	areas []Area // by descending len(ClaudePath): Rewrite prefers the longest prefix
	byID  map[proto.ScratchArea]Area

	mu sync.Mutex
	// baseline is what the target is known to have for each local file:
	// the state at the last committed Upload or Apply that covered it.
	// For shared areas it also lists the files that are synced at all.
	baseline map[fileKey]entry // guarded by mu
	// applyGen counts Apply calls; see entry.gen.
	applyGen uint64 // guarded by mu
	// pending maps the files of every Upload not yet committed or rolled
	// back to its id.
	pending  map[fileKey]uint64 // guarded by mu
	uploadID uint64             // guarded by mu; the last Upload id
	// held counts Hold calls per file.
	held map[fileID]int // guarded by mu
	// claimed holds the entries of shared areas (their first path
	// component) that Rewrite saw in this session's commands.
	claimed map[fileKey]bool // guarded by mu
}

type fileKey struct {
	area proto.ScratchArea
	path string
}

type entry struct {
	state fileState
	// gone records a deletion made by Apply until an Uploads confirms it.
	gone bool
	// gen is the applyGen of the Apply that recorded the entry, 0 for
	// entries recorded by New or Commit. Uploads ignores entries newer
	// than its own scan: that scan may predate the Apply's writes, and
	// acting on it would upload a stale file or a spurious deletion.
	gen uint64
}

// New returns a Mapper for areas. The files already present in the local
// per-session areas form the baseline and are never uploaded unless they
// change.
func New(areas []Area) (*Mapper, error) {
	m := &Mapper{
		areas:    append([]Area(nil), areas...),
		byID:     make(map[proto.ScratchArea]Area, len(areas)),
		baseline: make(map[fileKey]entry),
		pending:  make(map[fileKey]uint64),
		held:     make(map[fileID]int),
		claimed:  make(map[fileKey]bool),
	}
	claude := make(map[string]bool, len(areas))
	for _, a := range areas {
		if err := checkArea(a); err != nil {
			return nil, err
		}
		if _, dup := m.byID[a.ID]; dup {
			return nil, fmt.Errorf("scratch: area %d listed twice", a.ID)
		}
		if claude[a.ClaudePath] {
			return nil, fmt.Errorf("scratch: prefix %q listed twice", a.ClaudePath)
		}
		m.byID[a.ID] = a
		claude[a.ClaudePath] = true
	}
	sort.SliceStable(m.areas, func(i, j int) bool {
		return len(m.areas[i].ClaudePath) > len(m.areas[j].ClaudePath)
	})
	cur, roots := m.scan(nil, nil)
	roots.close()
	for k, st := range cur.files {
		m.baseline[k] = entry{state: st}
	}
	return m, nil
}

// shared reports whether other local Claude sessions write the area's
// directory too: shell-snapshots and session-env are in Claude's config
// directory, while CLAUDE_CODE_TMPDIR is per session (docs/claude-code.md
// "注入的环境").
func (a Area) shared() bool {
	return a.ID != proto.ScratchTmp
}

func checkArea(a Area) error {
	if !a.ID.Valid() {
		return fmt.Errorf("scratch: invalid area %d", a.ID)
	}
	for _, p := range []string{a.ClaudePath, a.LocalPath, a.RemotePath} {
		if err := proto.CheckPath(p); err != nil {
			return fmt.Errorf("scratch: area %s: %w", a.ID.Dir(), err)
		}
	}
	if a.ClaudePath == "/" {
		return errors.New("scratch: the root directory cannot be a scratch prefix")
	}
	return nil
}

func (m *Mapper) logger() *slog.Logger {
	if m.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Logger
}

// Rewrite replaces every scratch prefix in s (a command string, argument
// or environment value) with the area's remote directory.
//
// An occurrence counts only where it starts a path and ends a path
// component: the byte before it must not continue a path ('/' or a
// segment byte), and the byte after it must be '/', the end of s, or a
// byte that cannot continue a path segment. So for the prefix
// "/.tele/abc/tmp", "'/.tele/abc/tmp/x'" and "DIR=/.tele/abc/tmp" are
// rewritten, but "/.tele/abc/tmpx" and "/mnt/.tele/abc/tmp" are not.
// Shell-level forms such as "-o/.tele/abc/tmp" are therefore left alone;
// Claude never generates them for scratch files.
//
// In a shared area, the entry an occurrence names (the path component
// after the prefix, such as the session id in
// "<config>/session-env/<id>/hook-1.sh") becomes this session's: Uploads
// considers it from then on.
func (m *Mapper) Rewrite(s string) string {
	var b strings.Builder
	last := 0 // s[last:i] has not been copied to b yet
	for i := 0; i < len(s); i++ {
		if s[i] != '/' || (i > 0 && continuesPath(s[i-1])) {
			continue
		}
		for _, a := range m.areas {
			end := i + len(a.ClaudePath)
			if !strings.HasPrefix(s[i:], a.ClaudePath) || (end < len(s) && isSegmentByte(s[end])) {
				continue
			}
			b.Grow(len(s) - last + len(a.RemotePath))
			b.WriteString(s[last:i])
			b.WriteString(a.RemotePath)
			last = end
			i = end - 1
			if a.shared() {
				m.claim(a.ID, s[end:])
			}
			break
		}
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// claim records the entry that rest, the text after a shared area's
// prefix, names.
func (m *Mapper) claim(id proto.ScratchArea, rest string) {
	if len(rest) < 2 || rest[0] != '/' {
		return
	}
	n := 1
	for n < len(rest) && isSegmentByte(rest[n]) {
		n++
	}
	name := rest[1:n]
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, tempPrefix) {
		return
	}
	m.mu.Lock()
	m.claimed[fileKey{id, name}] = true
	m.mu.Unlock()
}

// isSegmentByte reports whether c can continue a path segment in the sense
// of Rewrite. Anything else, including non-ASCII bytes, ends a prefix.
func isSegmentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

func continuesPath(c byte) bool {
	return c == '/' || isSegmentByte(c)
}

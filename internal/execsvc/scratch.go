package execsvc

import (
	"cmp"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// The file helpers below mirror internal/scratch on purpose: the server
// must not depend on session main's packages (docs/coding-standards.md
// "目录与包").

// tempPrefix names temporary files that writeFile renames into place.
// Scans skip them so that a concurrent command never reports a
// half-written upload.
const tempPrefix = ".tele-tmp-"

// maxDepth bounds directory recursion; scratch areas are shallow.
const maxDepth = 32

// Limits of the scratch files returned with ExecExit, besides
// proto.ScratchFileMax and proto.ScratchTotalMax, which the client checks.
const (
	// entryOverhead bounds the CBOR encoding of a proto.ScratchFile
	// besides the bytes of its path and data: map header, keys, area,
	// mode, deletion flag and string headers take at most 23 bytes.
	entryOverhead = 32
	// returnBudget bounds the encoded scratch files of one ExecExit, so
	// that the frame stays within proto.MaxDataFrame; the rest of the
	// frame takes a few dozen bytes.
	returnBudget = proto.MaxDataFrame - 4<<10
	// maxReturnEntries bounds the files of one ExecExit, so that a command
	// that creates or removes a huge number of scratch files cannot keep
	// the client busy applying them.
	maxReturnEntries = 1 << 14
)

type scratchKey struct {
	area proto.ScratchArea
	path string
}

func compareKeys(a, b scratchKey) int {
	if c := cmp.Compare(a.area, b.area); c != 0 {
		return c
	}
	return strings.Compare(a.path, b.path)
}

type fileState struct {
	size  int64
	mtime int64 // nanoseconds
	ino   uint64
	mode  fs.FileMode
}

func stateOf(fi fs.FileInfo) fileState {
	st := fileState{size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: fi.Mode()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino
	}
	return st
}

// snapshot is the state of the regular files in the scratch areas.
type snapshot struct {
	files map[scratchKey]fileState
	// unknown holds the directories (path "." for a whole area) and files
	// that could not be read. What is at or below them is neither new nor
	// deleted: reporting it as deleted would make the client remove its
	// copies (and upload the removal back).
	unknown map[scratchKey]struct{}
}

func newSnapshot() *snapshot {
	return &snapshot{files: make(map[scratchKey]fileState), unknown: make(map[scratchKey]struct{})}
}

// covers reports whether k is at or below an unknown entry.
func (s *snapshot) covers(k scratchKey) bool {
	if len(s.unknown) == 0 {
		return false
	}
	for p := k.path; ; p = path.Dir(p) {
		if _, ok := s.unknown[scratchKey{k.area, p}]; ok {
			return true
		}
		if p == "." {
			return false
		}
	}
}

// uploadRecord is what the service did to a scratch file for the client.
type uploadRecord struct {
	state   fileState // the file as written; unused for a removal
	deleted bool
	seq     uint64 // Service.upSeq when recorded
}

// areaRoots are the scratch areas opened for one operation; an area that
// could not be opened is missing.
type areaRoots map[proto.ScratchArea]*os.Root

func (r areaRoots) close() {
	for _, root := range r {
		_ = root.Close()
	}
}

// openAreas opens every scratch area, creating any that a command
// removed. They are resolved again for each operation because commands
// may remove and recreate them (rm -rf "$CLAUDE_CODE_TMPDIR" is rewritten
// to an area): a root kept open would still refer to the removed
// directory. Areas are opened through the scratch directory, so that one
// replaced by a symbolic link cannot lead a scan out of it.
func (s *Service) openAreas() areaRoots {
	roots := make(areaRoots, len(proto.ScratchAreas))
	if err := os.MkdirAll(s.scratchDir, 0o700); err != nil {
		s.logFileErr("create scratch dir", 0, s.scratchDir, err)
		return roots
	}
	parent, err := os.OpenRoot(s.scratchDir)
	if err != nil {
		s.logFileErr("open scratch dir", 0, s.scratchDir, err)
		return roots
	}
	defer func() { _ = parent.Close() }()
	for _, a := range proto.ScratchAreas {
		if err := parent.MkdirAll(a.Dir(), 0o700); err != nil {
			s.logFileErr("create scratch area", a, a.Dir(), err)
			continue
		}
		r, err := parent.OpenRoot(a.Dir())
		if err != nil {
			s.logFileErr("open scratch area", a, a.Dir(), err)
			continue
		}
		roots[a] = r
	}
	return roots
}

// logFileErr logs a failed file operation. Paths are logged only at debug
// level (docs/coding-standards.md "日志与输出").
func (s *Service) logFileErr(op string, area proto.ScratchArea, name string, err error) {
	attrs := []any{"op", op, "err", withoutPath(err)}
	if area != 0 {
		attrs = append(attrs, "area", area.Dir())
	}
	s.log.Warn("execsvc: scratch file operation failed", attrs...)
	s.log.Debug("execsvc: scratch file operation failed", "op", op, "path", name, "err", err)
}

// withoutPath returns err's message without the path a *fs.PathError or
// *os.LinkError carries.
func withoutPath(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op + ": " + le.Err.Error()
	}
	return err.Error()
}

// writeScratch applies files uploaded by the client (docs/exec.md
// "scratch 路径改写与回传") and records them for changedScratch. Paths were validated
// with proto.CheckScratch; os.Root keeps symlinks from leading out of an
// area.
//
// A file that cannot be written is logged and skipped, and the command
// runs without it: the client offers an upload again only after it failed
// to reach the server, so failing the command instead would fail every
// later command the same way.
func (s *Service) writeScratch(roots areaRoots, files []proto.ScratchFile) {
	if len(files) == 0 {
		return
	}
	done := make(map[scratchKey]uploadRecord, len(files))
	for _, f := range files {
		root := roots[f.Area]
		if root == nil {
			continue // logged by openAreas
		}
		k := scratchKey{f.Area, f.Path}
		if f.Dir {
			// Nothing to record: scans report files only.
			if err := root.MkdirAll(f.Path, 0o700); err != nil {
				s.logFileErr("create uploaded directory", f.Area, f.Path, err)
			}
			continue
		}
		if f.Deleted {
			if err := removeFile(root, f.Path); err != nil {
				s.logFileErr("remove uploaded file", f.Area, f.Path, err)
				continue
			}
			done[k] = uploadRecord{deleted: true}
			continue
		}
		st, err := writeFile(root, f.Path, f.Data, fileMode(f.Mode))
		if err != nil {
			s.logFileErr("write uploaded file", f.Area, f.Path, err)
			continue
		}
		done[k] = uploadRecord{state: st}
	}

	s.upMu.Lock()
	defer s.upMu.Unlock()
	s.upSeq++
	for k, r := range done {
		r.seq = s.upSeq
		s.uploads[k] = r
	}
}

// scan returns the state of every regular file in the open areas.
func scan(roots areaRoots) *snapshot {
	snap := newSnapshot()
	for _, a := range proto.ScratchAreas {
		root := roots[a]
		if root == nil {
			snap.unknown[scratchKey{a, "."}] = struct{}{}
			continue
		}
		walkDir(root, a, ".", 0, snap)
	}
	return snap
}

// changedScratch returns the files that commands changed since before:
// new and modified files with their contents, and removed files.
//
// Changes made by writeScratch are not reported, although they happen
// after before was taken when another command uploads while this one
// runs: the client would apply its own upload again, possibly over a
// newer local file.
//
// The result is bounded by proto.ScratchFileMax per file,
// proto.ScratchTotalMax in total, returnBudget encoded and
// maxReturnEntries files. Changed files are chosen smallest first, since
// the files Claude waits for (the cwd file, shell snapshots,
// CLAUDE_ENV_FILE, task markers) are small and must not be crowded out by
// a command that writes or removes many files; removals come next. What
// does not fit is logged and dropped.
func (s *Service) changedScratch(before *snapshot) []proto.ScratchFile {
	seq := s.uploadSeq()
	roots := s.openAreas()
	defer roots.close()
	after := scan(roots)
	changed, deleted := s.diff(before, after, seq)

	slices.SortFunc(changed, func(a, b scratchKey) int {
		if c := cmp.Compare(after.files[a].size, after.files[b].size); c != 0 {
			return c
		}
		return compareKeys(a, b)
	})
	var (
		b                sizeBudget
		written, removed []proto.ScratchFile
		dropped          int
	)
	for _, k := range changed {
		data, st, err := readFile(roots[k.area], k.path)
		switch {
		case errors.Is(err, errTooLarge):
			s.log.Warn("execsvc: scratch file too large to return", "area", k.area.Dir(), "size", st.size)
			s.log.Debug("execsvc: skipped scratch file", "area", k.area.Dir(), "path", k.path)
			continue
		case err != nil:
			continue // removed or replaced since the scan
		}
		if !b.take(len(k.path), len(data)) {
			dropped++
			continue
		}
		written = append(written, proto.ScratchFile{Area: k.area, Path: k.path, Mode: uint32(st.mode.Perm()), Data: data})
	}
	for _, k := range deleted {
		if !b.take(len(k.path), 0) {
			dropped++
			continue
		}
		removed = append(removed, proto.ScratchFile{Area: k.area, Path: k.path, Deleted: true})
	}
	if dropped > 0 {
		s.log.Warn("execsvc: scratch return limit reached, dropping changes", "dropped", dropped)
	}
	// Removals first: a file replaced by a directory of the same name is
	// removed before the directory's files are written.
	slices.SortFunc(written, compareFiles)
	return append(removed, written...)
}

func (s *Service) uploadSeq() uint64 {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	return s.upSeq
}

// diff compares two snapshots, leaving out what writeScratch did. after
// was taken once upSeq was seq. Both results are sorted.
func (s *Service) diff(before, after *snapshot, seq uint64) (changed, deleted []scratchKey) {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	s.pruneUploads(after, seq)
	for k, st := range after.files {
		if old, ok := before.files[k]; ok && old == st {
			continue
		}
		if r, ok := s.uploads[k]; ok && !r.deleted && r.state == st {
			continue
		}
		changed = append(changed, k)
	}
	for k := range before.files {
		if _, ok := after.files[k]; ok || after.covers(k) {
			continue
		}
		if r, ok := s.uploads[k]; ok && r.deleted {
			continue
		}
		deleted = append(deleted, k)
	}
	slices.SortFunc(changed, compareKeys)
	slices.SortFunc(deleted, compareKeys)
	return changed, deleted
}

// pruneUploads forgets the records that after shows to be outdated: the
// file changed since it was uploaded, or a removed one exists again. Only
// records older than after are judged, since after may miss later ones.
// Callers hold upMu.
func (s *Service) pruneUploads(after *snapshot, seq uint64) {
	for k, r := range s.uploads {
		if r.seq > seq || after.covers(k) {
			continue
		}
		st, exists := after.files[k]
		if r.deleted && !exists || !r.deleted && exists && st == r.state {
			continue
		}
		delete(s.uploads, k)
	}
}

// sizeBudget accounts for the scratch files of one message.
type sizeBudget struct {
	encoded, data, entries int
}

// take adds a file if it fits.
func (b *sizeBudget) take(pathLen, dataLen int) bool {
	size := pathLen + dataLen + entryOverhead
	if b.entries >= maxReturnEntries || b.encoded+size > returnBudget || b.data+dataLen > proto.ScratchTotalMax {
		return false
	}
	b.entries++
	b.encoded += size
	b.data += dataLen
	return true
}

func compareFiles(a, b proto.ScratchFile) int {
	return compareKeys(scratchKey{a.Area, a.Path}, scratchKey{b.Area, b.Path})
}

var (
	errTooLarge   = errors.New("execsvc: file too large")
	errNotRegular = errors.New("execsvc: not a regular file")
)

// readFile reads a regular file of at most proto.ScratchFileMax bytes.
func readFile(root *os.Root, rel string) ([]byte, fileState, error) {
	// O_NONBLOCK: a file replaced by a FIFO since the scan must not block
	// the open.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fileState{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fileState{}, err
	}
	st := stateOf(fi)
	if !fi.Mode().IsRegular() {
		return nil, st, errNotRegular
	}
	if fi.Size() > proto.ScratchFileMax {
		return nil, st, errTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, proto.ScratchFileMax+1))
	if err != nil {
		return nil, st, err
	}
	if len(data) > proto.ScratchFileMax {
		return nil, st, errTooLarge
	}
	return data, st, nil
}

// fileMode keeps only permission bits and defaults to private.
func fileMode(mode uint32) os.FileMode {
	if mode &= 0o777; mode == 0 {
		return 0o600
	}
	return os.FileMode(mode)
}

// writeFile replaces rel with data through a temporary file, so that a
// command reading it concurrently never sees a partial file and a symlink
// at rel is replaced rather than followed. It returns the state of the
// written file, taken before the rename so that a command changing the
// file right after cannot be taken for the upload.
func writeFile(root *os.Root, rel string, data []byte, mode os.FileMode) (fileState, error) {
	dir := path.Dir(rel)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return fileState{}, err
		}
	}
	tmp := path.Join(dir, tempPrefix+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fileState{}, err
	}
	_, werr := f.Write(data)
	cerr := f.Chmod(mode) // exact mode, independent of umask
	var st fileState
	fi, serr := f.Stat()
	if serr == nil {
		st = stateOf(fi)
	}
	if err := errors.Join(werr, cerr, serr, f.Close()); err != nil {
		_ = root.Remove(tmp)
		return fileState{}, err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return fileState{}, err
	}
	return st, nil
}

// removeFile removes rel unless it is a directory; a missing file is not
// an error.
func removeFile(root *os.Root, rel string) error {
	fi, err := root.Lstat(rel)
	switch {
	case gone(err):
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		return nil
	}
	if err := root.Remove(rel); err != nil && !gone(err) {
		return err
	}
	return nil
}

// gone reports whether err means that a path no longer exists, as opposed
// to a failure to find out.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR)
}

// walkDir adds every regular file at or below dir to snap without
// following symbolic links. Directories and files it cannot read are
// recorded as unknown.
func walkDir(root *os.Root, area proto.ScratchArea, dir string, depth int, snap *snapshot) {
	d, err := root.Open(dir)
	if err != nil {
		if !gone(err) {
			snap.unknown[scratchKey{area, dir}] = struct{}{}
		}
		return
	}
	ents, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		// The entries read so far are still valid.
		snap.unknown[scratchKey{area, dir}] = struct{}{}
	}
	for _, ent := range ents {
		name := ent.Name()
		if strings.HasPrefix(name, tempPrefix) {
			continue
		}
		rel := name
		if dir != "." {
			rel = dir + "/" + name
		}
		switch {
		case ent.IsDir():
			if depth < maxDepth {
				walkDir(root, area, rel, depth+1, snap)
			}
		case ent.Type().IsRegular():
			fi, err := root.Lstat(rel)
			switch {
			case err == nil && fi.Mode().IsRegular():
				snap.files[scratchKey{area, rel}] = stateOf(fi)
			case err != nil && !gone(err):
				snap.unknown[scratchKey{area, rel}] = struct{}{}
			}
		}
	}
}

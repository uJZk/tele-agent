package scratch

import (
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// tempPrefix names the temporary files Apply renames into place. Scans
// skip them so that a concurrent Uploads never sends a half-written file.
const tempPrefix = ".tele-tmp-"

// maxDepth bounds directory recursion. Scratch areas are shallow; the
// bound only matters if a directory is swapped for a symlink loop while
// it is being walked.
const maxDepth = 32

const (
	// entryOverhead bounds the CBOR encoding of a proto.ScratchFile
	// besides the bytes of its path and data: map header, keys, area,
	// mode, deletion flag and string headers take at most 23 bytes.
	entryOverhead = 32
	// maxUploadEntries bounds the files of one Upload, so that removing a
	// huge number of local files cannot keep the target busy.
	maxUploadEntries = 1 << 14
)

// fileState identifies a version of a file well enough to detect changes
// made by commands and by Claude.
type fileState struct {
	size  int64
	mtime int64 // nanoseconds
	dev   uint64
	ino   uint64
	mode  fs.FileMode
}

func stateOf(fi fs.FileInfo) fileState {
	st := fileState{size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: fi.Mode()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.dev, st.ino = sys.Dev, sys.Ino
	}
	return st
}

// fileID identifies a local file independent of its name.
type fileID struct{ dev, ino uint64 }

func (st fileState) id() fileID { return fileID{st.dev, st.ino} }

// Hold keeps the regular file open as f out of the sync until release is
// called: Uploads does not send it and Apply neither replaces nor removes
// it. It does nothing for other kinds of files.
//
// Session main holds the files a command writes its output to while it
// writes them. A background task's output file, which Claude opens
// locally and hands to the shim as stdout (docs/claude-code.md section
// 4), may lie in CLAUDE_CODE_TMPDIR: uploading the partial file before
// every command wastes the link, and an Apply renaming a copy over it
// would leave the output being written in an unlinked file.
func (m *Mapper) Hold(f *os.File) (release func(), err error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("scratch: hold: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return func() {}, nil
	}
	id := stateOf(fi).id()
	m.mu.Lock()
	m.held[id]++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.held[id]--; m.held[id] == 0 {
				delete(m.held, id)
			}
		})
	}, nil
}

func (m *Mapper) isHeld(st fileState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held[st.id()] > 0
}

// Upload is the set of local changes sent with one command. The target has
// them only once the command started, so the baseline changes only on
// Commit. Until Commit or Rollback, later Uploads leave its files alone,
// so that two versions of one file are never on their way to the target
// at once; after Rollback they are offered again. Exactly one of Commit
// and Rollback must be called.
type Upload struct {
	// Files go into the command's ExecStart (rexec.Command.Scratch).
	Files []proto.ScratchFile

	m     *Mapper
	id    uint64
	gen   uint64            // applyGen when the scan started
	items map[fileKey]entry // what Files carry: a state, or gone for a removal
	once  sync.Once
}

// Commit records that the target wrote the files: the command started
// (rexec.Process.Started is closed, or rexec.Result.StartErr is nil).
func (u *Upload) Commit() { u.finish(true) }

// Rollback releases the files of an Upload that did not reach the target,
// so that the next Uploads offers them again. It does nothing after
// Commit.
func (u *Upload) Rollback() { u.finish(false) }

func (u *Upload) finish(commit bool) {
	u.once.Do(func() {
		m := u.m
		m.mu.Lock()
		defer m.mu.Unlock()
		for k, it := range u.items {
			if m.pending[k] == u.id {
				delete(m.pending, k)
			}
			if !commit {
				continue
			}
			if e, ok := m.baseline[k]; ok && e.gen > u.gen {
				continue // an Apply since the scan brought a newer version
			}
			if it.gone {
				delete(m.baseline, k)
			} else {
				m.baseline[k] = entry{state: it.state}
			}
		}
	})
}

// Uploads collects the local files that changed since the target last saw
// them: new and modified files with their contents, and removed files as
// Deleted entries. Files of shared areas are considered only if they are
// this session's (see the package documentation), and held files not at
// all.
//
// budget bounds the encoded size of the files, so that the ExecStart that
// carries them stays within proto.MaxDataFrame (rexec.ScratchBudget).
// Files larger than proto.ScratchFileMax are skipped until they change
// again; files beyond the budget, proto.ScratchTotalMax or
// maxUploadEntries wait for the next call.
func (m *Mapper) Uploads(budget int) *Upload {
	m.mu.Lock()
	gen := m.applyGen
	tracked, claimed := m.sharedLocked()
	m.mu.Unlock()

	cur, roots := m.scan(tracked, claimed)
	defer roots.close()
	changed, deleted := m.diff(cur, gen)

	u := &Upload{m: m, gen: gen, items: make(map[fileKey]entry, len(changed)+len(deleted))}
	b := sizeBudget{limit: budget}
	tooLarge := make(map[fileKey]fileState)
	deferred := 0
	// Removals first: a file replaced by a directory of the same name is
	// removed before the directory's files are written.
	for _, k := range deleted {
		if !b.take(len(k.path), 0) {
			deferred++
			continue
		}
		u.Files = append(u.Files, proto.ScratchFile{Area: k.area, Path: k.path, Deleted: true})
		u.items[k] = entry{gone: true}
	}
	for _, k := range changed {
		data, st, err := readFile(roots[k.area], k.path)
		switch {
		case errors.Is(err, errTooLarge):
			m.logger().Warn("scratch: local file too large to upload", "area", k.area.Dir(), "size", st.size)
			m.logger().Debug("scratch: skipped file", "area", k.area.Dir(), "path", k.path)
			tooLarge[k] = st
			continue
		case err != nil:
			// Vanished or replaced since the scan; the next call sees it.
			continue
		}
		if !b.take(len(k.path), len(data)) {
			deferred++
			continue
		}
		u.Files = append(u.Files, proto.ScratchFile{Area: k.area, Path: k.path, Mode: uint32(st.mode.Perm()), Data: data})
		u.items[k] = entry{state: st}
	}
	if deferred > 0 {
		m.logger().Warn("scratch: upload limit reached, deferring files", "deferred", deferred)
	}
	m.register(u, tooLarge)
	return u
}

// register makes u pending and records the files that are too large.
func (m *Mapper) register(u *Upload, tooLarge map[fileKey]fileState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploadID++
	u.id = m.uploadID
	kept := u.Files[:0]
	for _, f := range u.Files {
		k := fileKey{f.Area, f.Path}
		if e, ok := m.baseline[k]; ok && e.gen > u.gen {
			// An Apply since the scan brought the target's version; the
			// local file read before it must not replace that.
			delete(u.items, k)
			continue
		}
		if _, busy := m.pending[k]; busy {
			// A concurrent Uploads registered it first.
			delete(u.items, k)
			continue
		}
		m.pending[k] = u.id
		kept = append(kept, f)
	}
	u.Files = kept
	for k, st := range tooLarge {
		if e, ok := m.baseline[k]; !ok || e.gen <= u.gen {
			m.baseline[k] = entry{state: st}
		}
	}
}

// sharedLocked returns what Uploads looks at in shared areas: the files
// the baseline lists, and the claimed entries. Callers hold mu.
func (m *Mapper) sharedLocked() (tracked, claimed []fileKey) {
	for k := range m.baseline {
		if m.byID[k.area].shared() {
			tracked = append(tracked, k)
		}
	}
	for k := range m.claimed {
		claimed = append(claimed, k)
	}
	return tracked, claimed
}

// diff compares a scan taken after applyGen was gen with the baseline.
func (m *Mapper) diff(cur *scanResult, gen uint64) (changed, deleted []fileKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, st := range cur.files {
		if _, busy := m.pending[k]; busy || m.held[st.id()] > 0 {
			continue
		}
		e, ok := m.baseline[k]
		if ok && (e.gen > gen || !e.gone && e.state == st) {
			continue
		}
		changed = append(changed, k)
	}
	for k, e := range m.baseline {
		if _, ok := cur.files[k]; ok || e.gen > gen || !cur.sees(k) {
			continue
		}
		if _, busy := m.pending[k]; busy {
			continue
		}
		if e.gone {
			delete(m.baseline, k)
			continue
		}
		deleted = append(deleted, k)
	}
	slices.SortFunc(changed, compareKeys)
	slices.SortFunc(deleted, compareKeys)
	return changed, deleted
}

func compareKeys(a, b fileKey) int {
	if c := cmp.Compare(a.area, b.area); c != 0 {
		return c
	}
	return strings.Compare(a.path, b.path)
}

// sizeBudget accounts for the files of one Upload.
type sizeBudget struct {
	limit                  int
	encoded, data, entries int
}

// take adds a file if it fits.
func (b *sizeBudget) take(pathLen, dataLen int) bool {
	size := pathLen + dataLen + entryOverhead
	if b.entries >= maxUploadEntries || b.encoded+size > b.limit || b.data+dataLen > proto.ScratchTotalMax {
		return false
	}
	b.entries++
	b.encoded += size
	b.data += dataLen
	return true
}

var (
	errTooLarge   = errors.New("scratch: file too large")
	errNotRegular = errors.New("scratch: not a regular file")
)

// readFile reads a regular file of at most proto.ScratchFileMax bytes.
func readFile(root *os.Root, rel string) ([]byte, fileState, error) {
	if root == nil {
		return nil, fileState{}, fs.ErrNotExist
	}
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

// Apply writes files reported by the target into the local areas and
// records them so that they are not uploaded back. Held files are left
// alone. Every file is attempted; the returned error joins the failures.
func (m *Mapper) Apply(files []proto.ScratchFile) error {
	if err := proto.CheckScratch(files); err != nil {
		return fmt.Errorf("scratch: apply: %w", err)
	}
	for _, f := range files {
		if _, ok := m.byID[f.Area]; !ok {
			return fmt.Errorf("scratch: apply: area %s is not mapped", f.Area.Dir())
		}
	}

	roots := make(areaRoots)
	defer roots.close()
	type result struct {
		key  fileKey
		st   fileState
		gone bool
	}
	var (
		results []result
		errs    []error
	)
	for _, f := range files {
		root, err := m.openLocalRoot(roots, f.Area)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if m.heldAt(root, f.Path) {
			m.logger().Debug("scratch: not applying to a held file", "area", f.Area.Dir(), "path", f.Path)
			continue
		}
		k := fileKey{f.Area, f.Path}
		if f.Deleted {
			err = removeFile(root, f.Path)
			if err == nil {
				results = append(results, result{key: k, gone: true})
			}
		} else {
			var st fileState
			st, err = writeFile(root, f.Path, f.Data, fileMode(f.Mode))
			if err == nil {
				results = append(results, result{key: k, st: st})
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("scratch: apply to %s: %w", f.Area.Dir(), err))
		}
	}

	m.mu.Lock()
	m.applyGen++
	for _, r := range results {
		m.baseline[r.key] = entry{state: r.st, gone: r.gone, gen: m.applyGen}
	}
	m.mu.Unlock()
	return errors.Join(errs...)
}

// heldAt reports whether rel is a held file.
func (m *Mapper) heldAt(root *os.Root, rel string) bool {
	fi, err := root.Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return m.isHeld(stateOf(fi))
}

// areaRoots are local area directories open for one operation.
type areaRoots map[proto.ScratchArea]*os.Root

func (r areaRoots) close() {
	for _, root := range r {
		_ = root.Close()
	}
}

func (m *Mapper) openLocalRoot(roots areaRoots, id proto.ScratchArea) (*os.Root, error) {
	if r, ok := roots[id]; ok {
		return r, nil
	}
	dir := m.byID[id].LocalPath
	// The snapshot and session-env directories may not exist yet in a
	// fresh Claude config directory.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("scratch: create area %s: %w", id.Dir(), err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("scratch: open area %s: %w", id.Dir(), err)
	}
	roots[id] = r
	return r, nil
}

// fileMode is the permission of a file written from a peer's ScratchFile:
// only permission bits, and private when the peer sent none.
func fileMode(mode uint32) os.FileMode {
	if mode &= 0o777; mode == 0 {
		return 0o600
	}
	return os.FileMode(mode)
}

// writeFile replaces rel with data. It writes a temporary file and renames
// it into place, so that readers never see a partial file and a symlink at
// rel is replaced rather than followed. The returned state is taken before
// the rename, so that a change made right after it is not mistaken for
// the written file.
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
	// Chmod on the open file sets the exact mode, independent of umask.
	cerr := f.Chmod(mode)
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

// removeFile removes rel unless it is a directory; a missing file is not an
// error. A symlink at rel is removed itself, never its target.
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

// gone reports whether err means that a path does not exist, as opposed
// to a failure to find out.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR)
}

// scanResult is the state of the local files a scan looked at.
type scanResult struct {
	files map[fileKey]fileState
	// seen holds what the scan covered completely: whole areas (path
	// "."), claimed directories, and single files, present or not.
	seen map[fileKey]struct{}
	// unknown holds the directories and files that could not be read.
	// What is at or below them is neither new nor removed: reporting it as
	// removed would delete the target's copies.
	unknown map[fileKey]struct{}
}

// sees reports whether the scan established the current state of k.
func (r *scanResult) sees(k fileKey) bool {
	seen := false
	for p := k.path; ; p = path.Dir(p) {
		if _, ok := r.unknown[fileKey{k.area, p}]; ok {
			return false
		}
		if _, ok := r.seen[fileKey{k.area, p}]; ok {
			seen = true
		}
		if p == "." {
			return seen
		}
	}
}

// scan lists the regular files of the per-session areas, and of shared
// areas those that are this session's: the tracked files and the claimed
// entries. It returns the area roots it opened, for reading the files; a
// missing area directory has no files.
func (m *Mapper) scan(tracked, claimed []fileKey) (*scanResult, areaRoots) {
	res := &scanResult{
		files:   make(map[fileKey]fileState),
		seen:    make(map[fileKey]struct{}),
		unknown: make(map[fileKey]struct{}),
	}
	roots := make(areaRoots, len(m.areas))
	for _, a := range m.areas {
		root, err := os.OpenRoot(a.LocalPath)
		switch {
		case gone(err):
			res.seen[fileKey{a.ID, "."}] = struct{}{}
			continue
		case err != nil:
			m.logger().Warn("scratch: open area", "area", a.ID.Dir(), "err", withoutPath(err))
			res.unknown[fileKey{a.ID, "."}] = struct{}{}
			continue
		}
		roots[a.ID] = root
		if !a.shared() {
			res.seen[fileKey{a.ID, "."}] = struct{}{}
			walkDir(root, a.ID, ".", 0, res)
		}
	}
	for _, k := range append(tracked, claimed...) {
		root := roots[k.area]
		if root == nil {
			continue // area missing (seen) or unknown
		}
		fi, err := root.Lstat(k.path)
		switch {
		case err != nil && !gone(err):
			res.unknown[k] = struct{}{}
			continue
		case err == nil && fi.Mode().IsRegular():
			res.files[k] = stateOf(fi)
		case err == nil && fi.IsDir():
			walkDir(root, k.area, k.path, 1, res)
		}
		res.seen[k] = struct{}{}
	}
	return res, roots
}

// walkDir adds every regular file at or below dir to res without
// following symbolic links. Directories and files it cannot read are
// recorded as unknown.
func walkDir(root *os.Root, area proto.ScratchArea, dir string, depth int, res *scanResult) {
	d, err := root.Open(dir)
	if err != nil {
		if !gone(err) {
			res.unknown[fileKey{area, dir}] = struct{}{}
		}
		return
	}
	ents, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		// The entries read so far are still valid.
		res.unknown[fileKey{area, dir}] = struct{}{}
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
				walkDir(root, area, rel, depth+1, res)
			}
		case ent.Type().IsRegular():
			fi, err := root.Lstat(rel)
			switch {
			case err == nil && fi.Mode().IsRegular():
				res.files[fileKey{area, rel}] = stateOf(fi)
			case err != nil && !gone(err):
				res.unknown[fileKey{area, rel}] = struct{}{}
			}
		}
	}
}

// withoutPath returns err's message without the path a *fs.PathError
// carries; paths are logged only at debug level
// (docs/coding-standards.md section 10).
func withoutPath(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

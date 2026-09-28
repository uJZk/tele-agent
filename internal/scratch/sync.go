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
	"syscall"

	"github.com/ujzk/tele-agent/internal/proto"
)

// tempPrefix names the temporary files Apply renames into place. Scans
// skip them so that a concurrent Uploads never sends a half-written file.
const tempPrefix = ".tele-tmp-"

// maxDepth bounds directory recursion. Scratch areas are shallow; the
// bound only matters if a directory is swapped for a symlink loop while
// it is being walked.
const maxDepth = 32

// fileState identifies a version of a file well enough to detect changes
// made by commands and by Claude.
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

// Uploads returns the local files that changed since the target last saw
// them: new and modified files with their contents, and removed files as
// Deleted entries. Files larger than proto.ScratchFileMax are skipped
// until they change again; once proto.ScratchTotalMax is reached the rest
// wait for the next call.
func (m *Mapper) Uploads() ([]proto.ScratchFile, error) {
	m.mu.Lock()
	gen := m.applyGen
	m.mu.Unlock()

	cur, err := m.scan()
	if err != nil {
		return nil, err
	}
	changed, deleted := m.diff(cur, gen)

	files := make([]proto.ScratchFile, 0, len(changed)+len(deleted))
	seen := make(map[fileKey]fileState, len(changed))
	total := 0
	for i, k := range changed {
		f, st, err := m.readLocal(k)
		switch {
		case errors.Is(err, errTooLarge):
			m.logger().Warn("scratch: local file too large to upload", "area", k.area.Dir(), "size", st.size)
			m.logger().Debug("scratch: skipped file", "area", k.area.Dir(), "path", k.path)
			seen[k] = st
			continue
		case err != nil:
			// Vanished or replaced since the scan; the next call sees it.
			continue
		}
		if total+len(f.Data) > proto.ScratchTotalMax {
			m.logger().Warn("scratch: upload limit reached, deferring files", "deferred", len(changed)-i)
			break
		}
		total += len(f.Data)
		files = append(files, f)
		seen[k] = st
	}
	for _, k := range deleted {
		files = append(files, proto.ScratchFile{Area: k.area, Path: k.path, Deleted: true})
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for k, st := range seen {
		if e, ok := m.baseline[k]; !ok || e.gen <= gen {
			m.baseline[k] = entry{state: st}
		}
	}
	for _, k := range deleted {
		if e, ok := m.baseline[k]; ok && e.gen <= gen {
			delete(m.baseline, k)
		}
	}
	return files, nil
}

// diff compares a scan taken after applyGen was gen with the baseline.
func (m *Mapper) diff(cur map[fileKey]fileState, gen uint64) (changed, deleted []fileKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, st := range cur {
		e, ok := m.baseline[k]
		if ok && (e.gen > gen || !e.gone && e.state == st) {
			continue
		}
		changed = append(changed, k)
	}
	for k, e := range m.baseline {
		if _, ok := cur[k]; ok || e.gen > gen {
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

var (
	errTooLarge   = errors.New("scratch: file too large")
	errNotRegular = errors.New("scratch: not a regular file")
)

func (m *Mapper) readLocal(k fileKey) (proto.ScratchFile, fileState, error) {
	root, err := os.OpenRoot(m.byID[k.area].LocalPath)
	if err != nil {
		return proto.ScratchFile{}, fileState{}, err
	}
	defer func() { _ = root.Close() }()
	data, st, err := readFile(root, k.path)
	if err != nil {
		return proto.ScratchFile{}, st, err
	}
	return proto.ScratchFile{Area: k.area, Path: k.path, Mode: uint32(st.mode.Perm()), Data: data}, st, nil
}

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

// Apply writes files reported by the target into the local areas and
// records them so that they are not uploaded back. Every file is
// attempted; the returned error joins the failures.
func (m *Mapper) Apply(files []proto.ScratchFile) error {
	if err := proto.CheckScratch(files); err != nil {
		return fmt.Errorf("scratch: apply: %w", err)
	}
	for _, f := range files {
		if _, ok := m.byID[f.Area]; !ok {
			return fmt.Errorf("scratch: apply: area %s is not mapped", f.Area.Dir())
		}
	}

	roots := make(map[proto.ScratchArea]*os.Root)
	defer func() {
		for _, r := range roots {
			_ = r.Close()
		}
	}()
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

func (m *Mapper) openLocalRoot(roots map[proto.ScratchArea]*os.Root, id proto.ScratchArea) (*os.Root, error) {
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
// rel is replaced rather than followed.
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
	if err := errors.Join(werr, cerr, f.Close()); err != nil {
		_ = root.Remove(tmp)
		return fileState{}, err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return fileState{}, err
	}
	fi, err := root.Lstat(rel)
	if err != nil {
		return fileState{}, err
	}
	return stateOf(fi), nil
}

// removeFile removes rel unless it is a directory; a missing file is not an
// error. A symlink at rel is removed itself, never its target.
func removeFile(root *os.Root, rel string) error {
	fi, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		return nil
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// scan returns the regular files of every local area. A missing area
// directory has no files.
func (m *Mapper) scan() (map[fileKey]fileState, error) {
	cur := make(map[fileKey]fileState)
	for _, a := range m.areas {
		root, err := os.OpenRoot(a.LocalPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("scratch: open area %s: %w", a.ID.Dir(), err)
		}
		err = walk(root, func(rel string, fi fs.FileInfo) {
			cur[fileKey{a.ID, rel}] = stateOf(fi)
		})
		_ = root.Close()
		if err != nil {
			return nil, fmt.Errorf("scratch: scan area %s: %w", a.ID.Dir(), err)
		}
	}
	return cur, nil
}

// walk calls fn for every regular file below root without following
// symbolic links. Unreadable subdirectories are skipped.
func walk(root *os.Root, fn func(rel string, fi fs.FileInfo)) error {
	return walkDir(root, ".", 0, fn)
}

func walkDir(root *os.Root, dir string, depth int, fn func(rel string, fi fs.FileInfo)) error {
	d, err := root.Open(dir)
	if err != nil {
		if dir == "." {
			return err
		}
		return nil
	}
	ents, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil && dir == "." {
		return err
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
				_ = walkDir(root, rel, depth+1, fn)
			}
		case ent.Type().IsRegular():
			if fi, err := root.Lstat(rel); err == nil && fi.Mode().IsRegular() {
				fn(rel, fi)
			}
		}
	}
	return nil
}

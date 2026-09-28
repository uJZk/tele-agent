package execsvc

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"

	"github.com/ujzk/tele-agent/internal/proto"
)

// The file helpers below mirror internal/scratch on purpose: the server
// must not depend on session main's packages (docs/coding-standards.md
// section 2).

// tempPrefix names temporary files that writeFile renames into place.
// Scans skip them so that a concurrent command never reports a
// half-written upload.
const tempPrefix = ".tele-tmp-"

// maxDepth bounds directory recursion; scratch areas are shallow.
const maxDepth = 32

type scratchKey struct {
	area proto.ScratchArea
	path string
}

type fileState struct {
	size  int64
	mtime int64 // nanoseconds
	ino   uint64
	mode  fs.FileMode
}

// snapshot is the state of every regular file in the scratch areas.
type snapshot map[scratchKey]fileState

func stateOf(fi fs.FileInfo) fileState {
	st := fileState{size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: fi.Mode()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino
	}
	return st
}

// writeScratch applies files uploaded by the client (docs/exec.md
// section 5). Paths were validated with proto.CheckScratch; os.Root keeps
// symlinks from leading out of an area.
func (s *Service) writeScratch(files []proto.ScratchFile) error {
	for _, f := range files {
		root := s.areas[f.Area]
		var err error
		if f.Deleted {
			err = removeFile(root, f.Path)
		} else {
			err = writeFile(root, f.Path, f.Data, fileMode(f.Mode))
		}
		if err != nil {
			return fmt.Errorf("area %s: %w", f.Area.Dir(), err)
		}
	}
	return nil
}

func (s *Service) snapshot() snapshot {
	snap := make(snapshot)
	for _, a := range proto.ScratchAreas {
		walk(s.areas[a], func(rel string, fi fs.FileInfo) {
			snap[scratchKey{a, rel}] = stateOf(fi)
		})
	}
	return snap
}

// changedScratch returns the files that are new or modified since before,
// with their contents, and those that were removed. Files larger than
// proto.ScratchFileMax are skipped, and files beyond proto.ScratchTotalMax
// are dropped; both are logged.
func (s *Service) changedScratch(before snapshot, log *slog.Logger) []proto.ScratchFile {
	after := s.snapshot()
	var changed, deleted []scratchKey
	for k, st := range after {
		if old, ok := before[k]; !ok || old != st {
			changed = append(changed, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			deleted = append(deleted, k)
		}
	}
	sortKeys(changed)
	sortKeys(deleted)

	files := make([]proto.ScratchFile, 0, len(changed)+len(deleted))
	total := 0
	for i, k := range changed {
		data, st, err := readFile(s.areas[k.area], k.path)
		switch {
		case errors.Is(err, errTooLarge):
			log.Warn("execsvc: scratch file too large to return", "area", k.area.Dir(), "size", st.size)
			log.Debug("execsvc: skipped scratch file", "area", k.area.Dir(), "path", k.path)
			continue
		case err != nil:
			continue // removed or replaced since the scan
		}
		if total+len(data) > proto.ScratchTotalMax {
			log.Warn("execsvc: scratch return limit reached, dropping files", "dropped", len(changed)-i)
			break
		}
		total += len(data)
		files = append(files, proto.ScratchFile{Area: k.area, Path: k.path, Mode: uint32(st.mode.Perm()), Data: data})
	}
	for _, k := range deleted {
		files = append(files, proto.ScratchFile{Area: k.area, Path: k.path, Deleted: true})
	}
	return files
}

func sortKeys(keys []scratchKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].area != keys[j].area {
			return keys[i].area < keys[j].area
		}
		return keys[i].path < keys[j].path
	})
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
// at rel is replaced rather than followed.
func writeFile(root *os.Root, rel string, data []byte, mode os.FileMode) error {
	dir := path.Dir(rel)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path.Join(dir, tempPrefix+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Chmod(mode) // exact mode, independent of umask
	if err := errors.Join(werr, cerr, f.Close()); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// removeFile removes rel unless it is a directory; a missing file is not
// an error.
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

// walk calls fn for every regular file below root without following
// symbolic links. Unreadable directories are skipped.
func walk(root *os.Root, fn func(rel string, fi fs.FileInfo)) {
	walkDir(root, ".", 0, fn)
}

func walkDir(root *os.Root, dir string, depth int, fn func(rel string, fi fs.FileInfo)) {
	d, err := root.Open(dir)
	if err != nil {
		return
	}
	ents, _ := d.ReadDir(-1)
	_ = d.Close()
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
				walkDir(root, rel, depth+1, fn)
			}
		case ent.Type().IsRegular():
			if fi, err := root.Lstat(rel); err == nil && fi.Mode().IsRegular() {
				fn(rel, fi)
			}
		}
	}
}

package fssvc

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// Readdir limits. The kernel asks for one page of entries at a time, so a
// few hundred entries per round trip is plenty; the byte budget keeps every
// response far below the frame limit whatever the name lengths.
const (
	maxReaddirEntries = 1024
	maxReaddirBytes   = 512 << 10
	// direntOverhead approximates the encoded size of a DirEntry without
	// its name.
	direntOverhead = 96
)

// dirent is one entry of getdents64.
type dirent struct {
	name string
	off  int64
	ino  uint64
	typ  uint8 // DT_*
}

// linux_dirent64 layout: d_ino u64, d_off s64, d_reclen u16, d_type u8,
// d_name (NUL-terminated, padded to d_reclen).
const direntNameOffset = 19

// parseDirents decodes a getdents64 buffer. The kernel writes it in native
// byte order. Malformed records end the parse.
func parseDirents(buf []byte) []dirent {
	var out []dirent
	for len(buf) >= direntNameOffset {
		reclen := int(binary.NativeEndian.Uint16(buf[16:18]))
		if reclen < direntNameOffset || reclen > len(buf) {
			break
		}
		ino := binary.NativeEndian.Uint64(buf[0:8])
		off := int64(binary.NativeEndian.Uint64(buf[8:16]))
		typ := buf[18]
		name := buf[direntNameOffset:reclen]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		if len(name) > 0 {
			out = append(out, dirent{name: string(name), off: off, ino: ino, typ: typ})
		}
		buf = buf[reclen:]
	}
	return out
}

// readdir serves FSReaddir: entries of directory handle h starting at
// cookie off, at most size of them, each with its lstat attributes.
func (s *Service) readdir(h *handle, off int64, size uint32) *proto.FSResponse {
	h.dmu.Lock()
	defer h.dmu.Unlock()

	if off != h.dirPos {
		if _, err := unix.Seek(h.fd, off, unix.SEEK_SET); err != nil {
			return &proto.FSResponse{Errno: errnoOf(err)}
		}
		h.dirPos = off
		h.pending = nil
	}
	limit := int(size)
	if limit <= 0 || limit > maxReaddirEntries {
		limit = maxReaddirEntries
	}
	if h.dirBuf == nil {
		h.dirBuf = make([]byte, 32<<10)
	}

	resp := &proto.FSResponse{}
	budget := maxReaddirBytes
	for len(resp.Entries) < limit && budget > 0 {
		if len(h.pending) == 0 {
			var n int
			err := ignoringEINTR(func() error {
				var err error
				n, err = unix.Getdents(h.fd, h.dirBuf)
				return err
			})
			if err != nil {
				if len(resp.Entries) == 0 {
					return &proto.FSResponse{Errno: errnoOf(err)}
				}
				return resp
			}
			if n == 0 {
				resp.EOF = true
				return resp
			}
			h.pending = parseDirents(h.dirBuf[:n])
			if len(h.pending) == 0 {
				continue
			}
		}
		e := h.pending[0]
		h.pending = h.pending[1:]
		h.dirPos = e.off
		de, ok := s.direntAttr(h, e)
		if !ok {
			continue
		}
		resp.Entries = append(resp.Entries, de)
		budget -= direntOverhead + len(e.name)
	}
	return resp
}

// direntAttr returns entry e of directory handle h with its attributes,
// and false if it vanished since getdents listed it.
func (s *Service) direntAttr(h *handle, e dirent) (proto.DirEntry, bool) {
	de := proto.DirEntry{Name: e.name, Offset: e.off}
	var st unix.Stat_t
	err := ignoringEINTR(func() error {
		return unix.Fstatat(h.fd, e.name, &st, unix.AT_SYMLINK_NOFOLLOW)
	})
	switch {
	case err == nil:
		de.Attr = *attrFromStat(&st)
	case errors.Is(err, unix.ENOENT):
		// Leave it out, as a later listing would.
		return de, false
	default:
		// The name is listable but the entry cannot be examined, for
		// example in a directory with read but no search permission, or
		// a dead mount point: list it with what getdents told, as
		// readdir(3) would.
		s.logDebug("fssvc: readdir fstatat", "path", h.path, "name", e.name, "err", err)
		de.Attr = proto.Attr{Dev: h.dev, Ino: e.ino, Mode: dtypeMode(e.typ)}
		de.TypeOnly = true
	}
	return de, true
}

// dtypeMode converts a getdents d_type to the S_IFMT bits of a mode; 0 for
// DT_UNKNOWN.
func dtypeMode(t uint8) uint32 {
	return uint32(t) << 12
}

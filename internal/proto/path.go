package proto

import (
	"fmt"
	"path"
	"strings"
)

// CheckPath validates an absolute path received from a peer: it must be
// absolute, already clean, and free of NUL bytes.
func CheckPath(p string) error {
	if p == "" || p[0] != '/' {
		return fmt.Errorf("proto: path %q is not absolute", p)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("proto: path %q contains NUL", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("proto: path %q is not clean", p)
	}
	return nil
}

// CheckName validates a single directory entry name received from a peer.
func CheckName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("proto: invalid entry name %q", name)
	}
	if strings.IndexByte(name, '/') >= 0 || strings.IndexByte(name, 0) >= 0 {
		return fmt.Errorf("proto: entry name %q contains '/' or NUL", name)
	}
	return nil
}

// CheckRelPath validates a relative slash-separated path received from a
// peer: non-empty, clean, not absolute, without ".." components or NUL.
func CheckRelPath(p string) error {
	if p == "" || p[0] == '/' {
		return fmt.Errorf("proto: relative path %q is empty or absolute", p)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("proto: relative path %q contains NUL", p)
	}
	if path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("proto: relative path %q is not clean or escapes its root", p)
	}
	return nil
}

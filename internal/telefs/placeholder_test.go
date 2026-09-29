package telefs

import (
	"errors"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestPlaceholders(t *testing.T) {
	h := newHarness(t, harnessOpts{placeholders: []Placeholder{
		{Path: "/home/bob/.claude", Dir: true},
		{Path: "/home/bob/.claude.json"},
		{Path: "/.tele/0123456789abcdef", Dir: true},
		{Path: "/shadowed", Dir: true},
	}})
	// The remote has part of the ancestors, and an entry the placeholder
	// shadows.
	mkdirAll(t, h.b("home/bob/project"))
	writeFile(t, h.b("home/bob/.bashrc"), "rc")
	mkdirAll(t, h.b("shadowed"))
	writeFile(t, h.b("shadowed/secret"), "remote")
	writeFile(t, h.b("top"), "t")
	h.sync()

	t.Run("types", func(t *testing.T) {
		for rel, want := range map[string]uint32{
			"home/bob/.claude":       unix.S_IFDIR,
			"home/bob/.claude.json":  unix.S_IFREG,
			".tele":                  unix.S_IFDIR,
			".tele/0123456789abcdef": unix.S_IFDIR,
			"shadowed":               unix.S_IFDIR,
			"home":                   unix.S_IFDIR,
		} {
			if got := lstat(t, h.m(rel)).Mode & unix.S_IFMT; got != want {
				t.Errorf("%s type %o, want %o", rel, got, want)
			}
		}
		if st := lstat(t, h.m("home/bob/.claude.json")); st.Size != 0 {
			t.Errorf("placeholder file size %d", st.Size)
		}
		if got := readFile(t, h.m("home/bob/.claude.json")); got != "" {
			t.Errorf("placeholder file content %q", got)
		}
	})

	t.Run("listings", func(t *testing.T) {
		if got := names(t, h.m("")); !slices.Equal(got, []string{".tele", "home", "shadowed", "top"}) {
			t.Errorf("root lists %q", got)
		}
		if got := names(t, h.m("home/bob")); !slices.Equal(got, []string{".bashrc", ".claude", ".claude.json", "project"}) {
			t.Errorf("home/bob lists %q", got)
		}
		if got := names(t, h.m("shadowed")); len(got) != 0 {
			t.Errorf("placeholder lists %q", got)
		}
		if got := names(t, h.m(".tele")); !slices.Equal(got, []string{"0123456789abcdef"}) {
			t.Errorf(".tele lists %q", got)
		}
		if exists(h.m("shadowed/secret")) {
			t.Error("remote entry visible below a placeholder")
		}
		if got := readFile(t, h.m("home/bob/.bashrc")); got != "rc" {
			t.Errorf("remote file below an ancestor = %q", got)
		}
		// Remote entries below an ancestor are writable as usual.
		writeFile(t, h.m("home/bob/new"), "n")
		if got := readFile(t, h.b("home/bob/new")); got != "n" {
			t.Errorf("backing = %q", got)
		}
	})

	t.Run("protected entries", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			op   func() error
			want error
		}{
			{"rmdir placeholder", func() error { return unix.Rmdir(h.m("shadowed")) }, unix.EBUSY},
			{"unlink placeholder file", func() error { return unix.Unlink(h.m("home/bob/.claude.json")) }, unix.EBUSY},
			{"rmdir ancestor", func() error { return unix.Rmdir(h.m("home")) }, unix.EBUSY},
			{"rename placeholder", func() error { return unix.Rename(h.m("shadowed"), h.m("moved")) }, unix.EBUSY},
			{"rename onto placeholder", func() error { return unix.Rename(h.m("top"), h.m("home/bob/.claude.json")) }, unix.EBUSY},
			{"create in placeholder", func() error { return os.WriteFile(h.m("shadowed/x"), nil, 0o644) }, unix.EPERM},
			{"write placeholder file", func() error { return os.WriteFile(h.m("home/bob/.claude.json"), []byte("x"), 0o644) }, unix.EPERM},
		} {
			if err := tc.op(); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
			}
		}
	})

	t.Run("stable inodes", func(t *testing.T) {
		before := map[string]uint64{}
		for _, rel := range []string{"home", "home/bob", "home/bob/.claude", ".tele"} {
			before[rel] = lstat(t, h.m(rel)).Ino
		}
		if err := os.RemoveAll(h.b("home")); err != nil {
			t.Fatal(err)
		}
		h.sync()
		mkdirAll(t, h.b("home/bob"))
		h.sync()
		for rel, ino := range before {
			if got := lstat(t, h.m(rel)).Ino; got != ino {
				t.Errorf("%s inode %d, was %d", rel, got, ino)
			}
		}
	})
}

// TestSymlinkedAncestor covers merged-/usr hosts, where /bin is a symlink
// to usr/bin but /bin/sh is a placeholder: /bin is shown as a directory
// with the entries of usr/bin, and changes there are pushed.
func TestSymlinkedAncestor(t *testing.T) {
	h := newHarness(t, harnessOpts{
		placeholders: []Placeholder{{Path: "/bin/sh"}},
		// Ancestors are resolved at mount time.
		prepare: func(backing string) {
			mkdirAll(t, backing+"/usr/bin")
			writeFile(t, backing+"/usr/bin/ls", "ls")
			writeFile(t, backing+"/usr/bin/sh", "remote sh")
			if err := os.Symlink("usr/bin", backing+"/bin"); err != nil {
				t.Fatal(err)
			}
		},
	})

	if got := lstat(t, h.m("bin")).Mode & unix.S_IFMT; got != unix.S_IFDIR {
		t.Fatalf("bin type %o", got)
	}
	if got := names(t, h.m("bin")); !slices.Equal(got, []string{"ls", "sh"}) {
		t.Fatalf("bin lists %q", got)
	}
	if got := readFile(t, h.m("bin/ls")); got != "ls" {
		t.Fatalf("bin/ls = %q", got)
	}
	if got := lstat(t, h.m("bin/sh")).Size; got != 0 {
		t.Fatalf("bin/sh is not the placeholder: size %d", got)
	}
	_ = lstat(t, h.m("bin/ls"))
	if exists(h.m("bin/new")) {
		t.Fatal("bin/new exists")
	}
	writeFile(t, h.b("usr/bin/new"), "n")
	if err := os.Chmod(h.b("usr/bin/ls"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if !exists(h.m("bin/new")) {
		t.Fatal("entry created in the symlink target not visible")
	}
	if got := lstat(t, h.m("bin/ls")).Mode & 0o777; got != 0o700 {
		t.Fatalf("bin/ls mode %o after chmod", got)
	}
}

func TestBuildTree(t *testing.T) {
	cases := []struct {
		name string
		ps   []Placeholder
		ok   bool
	}{
		{"nested", []Placeholder{{Path: "/a/b", Dir: true}, {Path: "/a/c"}}, true},
		{"placeholder dir above another", []Placeholder{{Path: "/a", Dir: true}, {Path: "/a/b"}}, true},
		{"root", []Placeholder{{Path: "/", Dir: true}}, false},
		{"relative", []Placeholder{{Path: "a"}}, false},
		{"unclean", []Placeholder{{Path: "/a/../b"}}, false},
		{"duplicate", []Placeholder{{Path: "/a"}, {Path: "/a"}}, false},
		{"below a file", []Placeholder{{Path: "/a"}, {Path: "/a/b"}}, false},
		{"file above placeholders", []Placeholder{{Path: "/a/b"}, {Path: "/a"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildTree(tc.ps, nil)
			if (err == nil) != tc.ok {
				t.Fatalf("buildTree = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	root, err := buildTree([]Placeholder{{Path: "/a", Dir: true}, {Path: "/a/b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := root.children["a"]; a.kind != kindPlaceholderDir || a.children["b"].kind != kindPlaceholderFile {
		t.Fatalf("kinds %v %v", a.kind, a.children["b"].kind)
	}
}

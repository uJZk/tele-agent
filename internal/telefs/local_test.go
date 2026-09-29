package telefs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// localHarness mounts telefs with the .claude.json names of /home/bob
// served from a local directory, as tele does for ~/.claude.json
// (docs/claude-code.md "~/.claude.json").
func localHarness(t *testing.T) (*harness, string) {
	t.Helper()
	local := t.TempDir()
	h := newHarness(t, harnessOpts{
		placeholders: []Placeholder{{Path: "/home/bob/.claude", Dir: true}},
		localNames:   []LocalNames{{Dir: "/home/bob", Prefix: ".claude.json", Local: local}},
		prepare: func(b string) {
			mkdirAll(t, filepath.Join(b, "home/bob"))
			writeFile(t, filepath.Join(b, "home/bob/.bashrc"), "remote rc")
			// The remote user's own Claude state stays hidden.
			writeFile(t, filepath.Join(b, "home/bob/.claude.json"), "remote")
		},
	})
	writeFile(t, filepath.Join(local, ".claude.json"), "local")
	writeFile(t, filepath.Join(local, "unrelated"), "not shown")
	return h, local
}

func TestLocalNamesAtomicWrite(t *testing.T) {
	h, local := localHarness(t)
	if got := readFile(t, h.m("home/bob/.claude.json")); got != "local" {
		t.Fatalf(".claude.json = %q, want the local file", got)
	}
	// Claude's write: lock directory, temporary file renamed over the
	// target, lock removed.
	lock := h.m("home/bob/.claude.json.lock")
	if err := os.Mkdir(lock, 0o777); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(local, ".claude.json.lock")); err != nil || !fi.IsDir() || !fi.ModTime().Equal(stale) {
		t.Fatalf("local lock directory %v, %v; want it with the set mtime", fi, err)
	}
	tmp := h.m("home/bob/.claude.json.tmp.1.abc")
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, h.m("home/bob/.claude.json")); err != nil {
		t.Fatalf("rename over .claude.json: %v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(local, ".claude.json")); got != "new" {
		t.Errorf("local .claude.json = %q, want new", got)
	}
	if got := readFile(t, h.m("home/bob/.claude.json")); got != "new" {
		t.Errorf(".claude.json through the mount = %q, want new", got)
	}
	if got := readFile(t, h.b("home/bob/.claude.json")); got != "remote" {
		t.Errorf("remote .claude.json = %q, want it untouched", got)
	}
	if _, err := os.Lstat(h.b("home/bob/.claude.json.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock directory reached the remote: %v", err)
	}
}

func TestLocalNamesListing(t *testing.T) {
	h, local := localHarness(t)
	writeFile(t, filepath.Join(local, ".claude.json.backup"), "b")
	got := names(t, h.m("home/bob"))
	for _, want := range []string{".bashrc", ".claude", ".claude.json", ".claude.json.backup"} {
		if !slices.Contains(got, want) {
			t.Errorf("listing %q lacks %s", got, want)
		}
	}
	if slices.Contains(got, "unrelated") {
		t.Errorf("listing %q shows a local name outside the prefix", got)
	}
	if n := len(slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s != ".claude.json" })); n != 1 {
		t.Errorf("listing %q shows .claude.json %d times", got, n)
	}
	// Other names in the directory stay remote.
	writeFile(t, h.m("home/bob/notes"), "n")
	if got := readFile(t, h.b("home/bob/notes")); got != "n" {
		t.Errorf("remote notes = %q", got)
	}
}

func TestLocalNamesCrossRename(t *testing.T) {
	h, _ := localHarness(t)
	for _, tc := range []struct{ from, to string }{
		{"home/bob/.claude.json", "home/bob/elsewhere"},
		{"home/bob/.bashrc", "home/bob/.claude.json.x"},
	} {
		err := os.Rename(h.m(tc.from), h.m(tc.to))
		if !errors.Is(err, unix.EXDEV) {
			t.Errorf("rename %s -> %s = %v, want EXDEV", tc.from, tc.to, err)
		}
	}
}

func TestLocalNamesSeeLocalChanges(t *testing.T) {
	// Another local claude writes ~/.claude.json directly; the mount
	// shows the change at once.
	h, local := localHarness(t)
	if got := readFile(t, h.m("home/bob/.claude.json")); got != "local" {
		t.Fatalf(".claude.json = %q", got)
	}
	writeFile(t, filepath.Join(local, ".claude.json.tmp"), "changed locally")
	if err := os.Rename(filepath.Join(local, ".claude.json.tmp"), filepath.Join(local, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.m("home/bob/.claude.json")); got != "changed locally" {
		t.Fatalf(".claude.json = %q, want the local change", got)
	}
}

func TestLocalNamesConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		ps   []Placeholder
		ls   []LocalNames
		ok   bool
	}{
		{"ok", []Placeholder{{Path: "/h/.claude", Dir: true}}, []LocalNames{{Dir: "/h", Prefix: ".claude.json", Local: "/l"}}, true},
		{"placeholder among local names", []Placeholder{{Path: "/h/.claude.json"}}, []LocalNames{{Dir: "/h", Prefix: ".claude.json", Local: "/l"}}, false},
		{"on a placeholder", []Placeholder{{Path: "/h", Dir: true}}, []LocalNames{{Dir: "/h", Prefix: ".x", Local: "/l"}}, false},
		{"directory twice", nil, []LocalNames{{Dir: "/h", Prefix: ".x", Local: "/l"}, {Dir: "/h", Prefix: ".y", Local: "/l"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildTree(tc.ps, tc.ls); (err == nil) != tc.ok {
				t.Fatalf("buildTree = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	for _, l := range []LocalNames{
		{Dir: "h", Prefix: ".x", Local: "/l"},
		{Dir: "/h", Prefix: "", Local: "/l"},
		{Dir: "/h", Prefix: "a/b", Local: "/l"},
		{Dir: "/h", Prefix: ".x", Local: "l"},
		{Dir: "/h", Prefix: ".x\x00", Local: "/l"},
	} {
		if err := checkLocalNames([]LocalNames{l}); err == nil {
			t.Errorf("checkLocalNames(%+v) accepted it", l)
		}
	}
}

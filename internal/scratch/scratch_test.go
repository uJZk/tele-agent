package scratch

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ujzk/tele-agent/internal/proto"
)

func testMapper(t *testing.T) (*Mapper, string) {
	t.Helper()
	local := t.TempDir()
	m, err := New([]Area{
		{ID: proto.ScratchTmp, ClaudePath: "/.tele/abc/tmp", LocalPath: filepath.Join(local, "tmp"), RemotePath: "/r/s/tmp"},
		{ID: proto.ScratchSnapshots, ClaudePath: "/home/u/.claude/shell-snapshots", LocalPath: filepath.Join(local, "snap"), RemotePath: "/r/s/shell-snapshots"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, local
}

func TestRewrite(t *testing.T) {
	m, err := New([]Area{
		{ID: proto.ScratchTmp, ClaudePath: "/.tele/abc/tmp", LocalPath: "/l/tmp", RemotePath: "/R/tmp"},
		{ID: proto.ScratchSnapshots, ClaudePath: "/.tele/abc/tmp/snap", LocalPath: "/l/snap", RemotePath: "/R/snap"},
		{ID: proto.ScratchSessionEnv, ClaudePath: "/h/.claude/session-env", LocalPath: "/l/env", RemotePath: "/R/env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ in, want string }{
		{"", ""},
		{"ls /", "ls /"},
		{"/.tele/abc/tmp", "/R/tmp"},
		{"/.tele/abc/tmp/", "/R/tmp/"},
		{"/.tele/abc/tmp/claude-1-cwd", "/R/tmp/claude-1-cwd"},
		{"pwd -P >| '/.tele/abc/tmp/x' && cat \"/.tele/abc/tmp/y\"", "pwd -P >| '/R/tmp/x' && cat \"/R/tmp/y\""},
		{"DIR=/.tele/abc/tmp", "DIR=/R/tmp"},
		{"a:/.tele/abc/tmp:b", "a:/R/tmp:b"},
		{"/.tele/abc/tmp;/.tele/abc/tmp", "/R/tmp;/R/tmp"},
		// Not a path component boundary on the right.
		{"/.tele/abc/tmpx", "/.tele/abc/tmpx"},
		{"/.tele/abc/tmp.bak", "/.tele/abc/tmp.bak"},
		{"/.tele/abc/tmp-1", "/.tele/abc/tmp-1"},
		{"/.tele/abc/tmp_1", "/.tele/abc/tmp_1"},
		// Inside a longer path on the left.
		{"/mnt/.tele/abc/tmp/x", "/mnt/.tele/abc/tmp/x"},
		{"x/.tele/abc/tmp", "x/.tele/abc/tmp"},
		// Longest prefix first.
		{"/.tele/abc/tmp/snap/s.sh", "/R/snap/s.sh"},
		{"/.tele/abc/tmp/snapx", "/R/tmp/snapx"},
		{"source /h/.claude/session-env/e1 && x", "source /R/env/e1 && x"},
		// Non-ASCII ends a prefix.
		{"/.tele/abc/tmp\u00e9", "/R/tmp\u00e9"},
		{"/.tele/abc/tm", "/.tele/abc/tm"},
	}
	for _, tt := range tests {
		if got := m.Rewrite(tt.in); got != tt.want {
			t.Errorf("Rewrite(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNewRejectsBadAreas(t *testing.T) {
	good := Area{ID: proto.ScratchTmp, ClaudePath: "/c", LocalPath: "/l", RemotePath: "/r"}
	tests := []struct {
		name  string
		areas []Area
	}{
		{"invalid id", []Area{{ID: 9, ClaudePath: "/c", LocalPath: "/l", RemotePath: "/r"}}},
		{"relative", []Area{{ID: proto.ScratchTmp, ClaudePath: "c", LocalPath: "/l", RemotePath: "/r"}}},
		{"unclean", []Area{{ID: proto.ScratchTmp, ClaudePath: "/c/", LocalPath: "/l", RemotePath: "/r"}}},
		{"root prefix", []Area{{ID: proto.ScratchTmp, ClaudePath: "/", LocalPath: "/l", RemotePath: "/r"}}},
		{"duplicate id", []Area{good, {ID: proto.ScratchTmp, ClaudePath: "/d", LocalPath: "/l2", RemotePath: "/r2"}}},
		{"duplicate prefix", []Area{good, {ID: proto.ScratchSnapshots, ClaudePath: "/c", LocalPath: "/l2", RemotePath: "/r2"}}},
	}
	for _, tt := range tests {
		if _, err := New(tt.areas); err == nil {
			t.Errorf("%s: New accepted %+v", tt.name, tt.areas)
		}
	}
}

func writeLocal(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// upload takes the pending local changes with an ample budget and commits
// them, as for a command that started.
func upload(m *Mapper) []proto.ScratchFile {
	u := m.Uploads(proto.MaxDataFrame)
	u.Commit()
	return u.Files
}

func byPath(files []proto.ScratchFile) map[string]proto.ScratchFile {
	m := make(map[string]proto.ScratchFile, len(files))
	for _, f := range files {
		m[f.Area.Dir()+":"+f.Path] = f
	}
	return m
}

func TestUploads(t *testing.T) {
	local := t.TempDir()
	tmp := filepath.Join(local, "tmp")
	writeLocal(t, filepath.Join(tmp, "old"), "pre-existing")
	writeLocal(t, filepath.Join(tmp, "keep"), "unchanged")
	m, err := New([]Area{{ID: proto.ScratchTmp, ClaudePath: "/c/tmp", LocalPath: tmp, RemotePath: "/r/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	if files := upload(m); len(files) != 0 {
		t.Fatalf("Uploads after New = %+v; want nothing", files)
	}

	writeLocal(t, filepath.Join(tmp, "new"), "fresh")
	writeLocal(t, filepath.Join(tmp, "sub/dir/f"), "nested")
	writeLocal(t, filepath.Join(tmp, "old"), "modified content")
	if err := os.Chmod(filepath.Join(tmp, "new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tmp, "keep")); err != nil {
		t.Fatal(err)
	}
	// Neither symlinks nor Apply's temporary files are uploaded.
	if err := os.Symlink("/etc/passwd", filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, filepath.Join(tmp, tempPrefix+"x"), "partial")

	got := byPath(upload(m))
	want := map[string]proto.ScratchFile{
		"tmp:new":       {Area: proto.ScratchTmp, Path: "new", Mode: 0o640, Data: []byte("fresh")},
		"tmp:sub/dir/f": {Area: proto.ScratchTmp, Path: "sub/dir/f", Mode: 0o600, Data: []byte("nested")},
		"tmp:old":       {Area: proto.ScratchTmp, Path: "old", Mode: 0o600, Data: []byte("modified content")},
		"tmp:keep":      {Area: proto.ScratchTmp, Path: "keep", Deleted: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Uploads =\n%+v\nwant\n%+v", got, want)
	}
	if files := upload(m); len(files) != 0 {
		t.Fatalf("second Uploads = %+v; want nothing", files)
	}
}

func TestUploadsLimits(t *testing.T) {
	local := t.TempDir()
	tmp := filepath.Join(local, "tmp")
	m, err := New([]Area{{ID: proto.ScratchTmp, ClaudePath: "/c/tmp", LocalPath: tmp, RemotePath: "/r/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	writeLocal(t, filepath.Join(tmp, "big"), strings.Repeat("b", proto.ScratchFileMax+1))
	// Nine files of 1 MiB exceed the total limit by one file.
	for i := range 9 {
		writeLocal(t, filepath.Join(tmp, "f", string(rune('a'+i))), strings.Repeat("x", proto.ScratchFileMax))
	}
	files := upload(m)
	total := 0
	for _, f := range files {
		if f.Path == "big" {
			t.Fatal("oversized file uploaded")
		}
		total += len(f.Data)
	}
	if len(files) != 8 || total > proto.ScratchTotalMax {
		t.Fatalf("uploaded %d files, %d bytes; want 8 within the total limit", len(files), total)
	}
	// The deferred file follows in the next call; the oversized one stays
	// skipped until it changes.
	files = upload(m)
	if len(files) != 1 || files[0].Path != "f/i" {
		t.Fatalf("second Uploads = %d files; want only f/i", len(files))
	}
	writeLocal(t, filepath.Join(tmp, "big"), "small now")
	files = upload(m)
	if len(files) != 1 || files[0].Path != "big" {
		t.Fatalf("Uploads after shrink = %+v", files)
	}
}

func TestApply(t *testing.T) {
	m, local := testMapper(t)
	writeLocal(t, filepath.Join(local, "tmp", "gone"), "to delete")
	writeLocal(t, filepath.Join(local, "tmp", "over"), "old")
	upload(m)

	err := m.Apply([]proto.ScratchFile{
		{Area: proto.ScratchTmp, Path: "cwd", Data: []byte("/home/u\n")},
		{Area: proto.ScratchTmp, Path: "a/b/c", Mode: 0o755 | 0o4000, Data: []byte("exec")},
		{Area: proto.ScratchTmp, Path: "over", Mode: 0o644, Data: []byte("new")},
		{Area: proto.ScratchTmp, Path: "gone", Deleted: true},
		{Area: proto.ScratchTmp, Path: "never-existed", Deleted: true},
		{Area: proto.ScratchSnapshots, Path: "snapshot-bash-1.sh", Mode: 0o600, Data: []byte("export X=1\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		rel  string
		data string
		mode fs.FileMode
	}{
		{"tmp/cwd", "/home/u\n", 0o600},
		{"tmp/a/b/c", "exec", 0o755},
		{"tmp/over", "new", 0o644},
		{"snap/snapshot-bash-1.sh", "export X=1\n", 0o600},
	}
	for _, c := range checks {
		name := filepath.Join(local, c.rel)
		data, err := os.ReadFile(name)
		if err != nil || string(data) != c.data {
			t.Errorf("%s = %q, %v; want %q", c.rel, data, err, c.data)
			continue
		}
		fi, err := os.Stat(name)
		if err != nil || fi.Mode() != c.mode {
			t.Errorf("%s mode = %v, %v; want %v", c.rel, fi.Mode(), err, c.mode)
		}
	}
	fi, err := os.Stat(filepath.Join(local, "tmp", "a"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("created parent mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	if _, err := os.Stat(filepath.Join(local, "tmp", "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("deleted file still exists: %v", err)
	}
	// Applied files are what the target has: nothing to upload back.
	if files := upload(m); len(files) != 0 {
		t.Fatalf("Uploads after Apply = %+v; want nothing", files)
	}
	// A later local change is uploaded again, also in a shared area for a
	// file the target produced.
	writeLocal(t, filepath.Join(local, "tmp", "cwd"), "/tmp\n")
	if err := os.Remove(filepath.Join(local, "snap", "snapshot-bash-1.sh")); err != nil {
		t.Fatal(err)
	}
	want := map[string]proto.ScratchFile{
		"tmp:cwd":                            {Area: proto.ScratchTmp, Path: "cwd", Mode: 0o600, Data: []byte("/tmp\n")},
		"shell-snapshots:snapshot-bash-1.sh": {Area: proto.ScratchSnapshots, Path: "snapshot-bash-1.sh", Deleted: true},
	}
	if got := byPath(upload(m)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Uploads after local change = %+v, want %+v", got, want)
	}
	// The snapshot is no longer tracked once its removal was committed.
	writeLocal(t, filepath.Join(local, "snap", "snapshot-bash-1.sh"), "recreated by another session")
	if files := upload(m); len(files) != 0 {
		t.Fatalf("Uploads after an untracked change = %+v; want nothing", files)
	}
}

func TestApplyRejects(t *testing.T) {
	m, local := testMapper(t)
	outside := t.TempDir()
	tmp := filepath.Join(local, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tmp, "abs")); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(tmp, outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(tmp, "rel")); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	writeLocal(t, victim, "keep me")
	if err := os.Symlink(victim, filepath.Join(tmp, "direct")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		file proto.ScratchFile
	}{
		{"dotdot", proto.ScratchFile{Area: proto.ScratchTmp, Path: "../x", Data: []byte("x")}},
		{"absolute", proto.ScratchFile{Area: proto.ScratchTmp, Path: "/etc/x", Data: []byte("x")}},
		{"unclean", proto.ScratchFile{Area: proto.ScratchTmp, Path: "a/../../x", Data: []byte("x")}},
		{"invalid area", proto.ScratchFile{Area: 77, Path: "x", Data: []byte("x")}},
		{"unmapped area", proto.ScratchFile{Area: proto.ScratchSessionEnv, Path: "x", Data: []byte("x")}},
		{"absolute symlink dir", proto.ScratchFile{Area: proto.ScratchTmp, Path: "abs/x", Data: []byte("x")}},
		{"relative symlink dir", proto.ScratchFile{Area: proto.ScratchTmp, Path: "rel/x", Data: []byte("x")}},
		{"delete through symlink dir", proto.ScratchFile{Area: proto.ScratchTmp, Path: "abs/victim", Deleted: true}},
	}
	for _, tt := range tests {
		if err := m.Apply([]proto.ScratchFile{tt.file}); err == nil {
			t.Errorf("%s: Apply accepted %q", tt.name, tt.file.Path)
		}
	}
	// A symlink as the final component is replaced, not followed.
	if err := m.Apply([]proto.ScratchFile{{Area: proto.ScratchTmp, Path: "direct", Data: []byte("new")}}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep me" {
		t.Fatalf("symlink target = %q, %v; want untouched", data, err)
	}
	if fi, err := os.Lstat(filepath.Join(tmp, "direct")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("direct = %v, %v; want a regular file", fi, err)
	}
	ents, err := os.ReadDir(outside)
	if err != nil || len(ents) != 1 {
		t.Fatalf("outside dir has %d entries, %v; want only the victim", len(ents), err)
	}
}

func TestConcurrentApplyAndUploads(t *testing.T) {
	m, _ := testMapper(t)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for j := range 20 {
				f := proto.ScratchFile{Area: proto.ScratchTmp, Path: "f" + string(rune('a'+i)), Data: []byte{byte(j)}}
				if err := m.Apply([]proto.ScratchFile{f}); err != nil {
					t.Error(err)
					return
				}
				upload(m)
			}
		})
	}
	wg.Wait()
	// Every file now matches what was applied last; none is reported as
	// deleted even though scans raced with the writes.
	for _, f := range upload(m) {
		if f.Deleted {
			t.Fatalf("spurious deletion of %q", f.Path)
		}
	}
}

func FuzzRewrite(f *testing.F) {
	for _, s := range []string{"", "/.tele/abc/tmp", "x /.tele/abc/tmp/y", "/.tele/abc/tmpx", "'/h/.claude/session-env'", "/mnt/.tele/abc/tmp"} {
		f.Add(s)
	}
	m, err := New([]Area{
		{ID: proto.ScratchTmp, ClaudePath: "/.tele/abc/tmp", LocalPath: "/l/tmp", RemotePath: "/\x01T"},
		{ID: proto.ScratchSessionEnv, ClaudePath: "/h/.claude/session-env", LocalPath: "/l/env", RemotePath: "/\x01E"},
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := m.Rewrite(s)
		if !strings.Contains(s, "/.tele/abc/tmp") && !strings.Contains(s, "/h/.claude/session-env") && got != s {
			t.Fatalf("Rewrite(%q) = %q without any prefix", s, got)
		}
		if strings.Contains(s, "\x01") {
			return
		}
		// The remote paths use a byte the input lacks, so mapping them back
		// must restore the input exactly: only whole prefixes were replaced.
		back := strings.NewReplacer("/\x01T", "/.tele/abc/tmp", "/\x01E", "/h/.claude/session-env").Replace(got)
		if back != s {
			t.Fatalf("Rewrite(%q) = %q, which does not map back", s, got)
		}
		// No prefix at a path boundary survives.
		for i := range len(got) {
			for _, p := range []string{"/.tele/abc/tmp", "/h/.claude/session-env"} {
				end := i + len(p)
				if strings.HasPrefix(got[i:], p) && (i == 0 || !continuesPath(got[i-1])) &&
					(end == len(got) || !isSegmentByte(got[end])) {
					t.Fatalf("Rewrite(%q) = %q leaves %q at %d", s, got, p, i)
				}
			}
		}
	})
}

func FuzzApplyPath(f *testing.F) {
	for _, s := range []string{"x", "a/b", "../x", "link/x", "abs/x", "link/../x", "./x", "a//b", "..", "dir/", "link"} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, rel string, deleted bool) {
		base := t.TempDir()
		tmp := filepath.Join(base, "tmp")
		outside := filepath.Join(base, "outside")
		for _, d := range []string{tmp, outside} {
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		victim := filepath.Join(outside, "victim")
		if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, target := range map[string]string{"link": "../outside", "abs": outside, "file": victim} {
			if err := os.Symlink(target, filepath.Join(tmp, name)); err != nil {
				t.Fatal(err)
			}
		}
		m, err := New([]Area{{ID: proto.ScratchTmp, ClaudePath: "/c", LocalPath: tmp, RemotePath: "/r"}})
		if err != nil {
			t.Fatal(err)
		}
		applyErr := m.Apply([]proto.ScratchFile{{Area: proto.ScratchTmp, Path: rel, Data: []byte("data"), Deleted: deleted}})

		ents, err := os.ReadDir(outside)
		if err != nil || len(ents) != 1 {
			t.Fatalf("Apply(%q) changed the outside dir: %d entries, %v", rel, len(ents), err)
		}
		if data, err := os.ReadFile(victim); err != nil || !bytes.Equal(data, []byte("v")) {
			t.Fatalf("Apply(%q) changed the victim: %q, %v", rel, data, err)
		}
		if applyErr == nil && !deleted {
			data, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(rel)))
			if err != nil || string(data) != "data" {
				t.Fatalf("Apply(%q) succeeded but the file reads %q, %v", rel, data, err)
			}
		}
	})
}

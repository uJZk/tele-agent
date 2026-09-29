package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAndRestore(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(filepath.Join(dir, "cfg", "install-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(dir, "created")
	replaced := filepath.Join(dir, "replaced")
	if err := os.WriteFile(replaced, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a second install keeps the first backup
		if err := m.WriteFile("a", "created a file", created, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteFile("b", "replaced a file", replaced, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Record(Entry{ID: "c", Undo: "true"}); err != nil {
		t.Fatal(err)
	}

	m, err = Load(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 3 || m.Entries[0].Backup != "" || m.Entries[1].Backup == "" {
		t.Fatalf("entries = %+v", m.Entries)
	}
	for i := len(m.Entries) - 1; i >= 0; i-- {
		if err := RestoreFile(m.Entries[i]); err != nil {
			t.Fatal(err)
		}
		if err := m.Remove(m.Entries[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Errorf("created file survives rollback: %v", err)
	}
	b, err := os.ReadFile(replaced)
	if err != nil || string(b) != "original" {
		t.Errorf("replaced file = %q, %v; want the original", b, err)
	}
	if fi, err := os.Stat(replaced); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("restored mode = %v, %v", fi.Mode().Perm(), err)
	}
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Errorf("empty manifest is not deleted: %v", err)
	}
}

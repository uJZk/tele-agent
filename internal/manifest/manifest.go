// Package manifest records what "tele server install" changed on the
// target host, so that "tele server uninstall" can roll it back entry by
// entry: ~/.config/tele/install-manifest.json (docs/cli.md "预检与修复策略").
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Entry is one change. Rolling it back restores Path, then runs Undo.
type Entry struct {
	// ID identifies the change; recording an ID twice keeps the first
	// entry, so that repeated installs keep the original backup.
	ID string `json:"id"`
	// Desc says what the change was, for uninstall's output.
	Desc string `json:"desc"`
	// Path is a file tele wrote. Rollback removes it, or restores Backup.
	Path string `json:"path,omitempty"`
	// Backup holds the file Path replaced; empty when tele created Path.
	Backup string `json:"backup,omitempty"`
	// Undo is a shell command that reverts the change.
	Undo string `json:"undo,omitempty"`
	// Consent marks an Undo that, like the change itself, needs the
	// user's consent: it acts on the whole system or asks for privileges.
	Consent bool `json:"consent,omitempty"`
}

// Manifest is the list of changes, oldest first.
type Manifest struct {
	path    string
	Entries []Entry `json:"entries"`
}

// Path returns the default manifest file.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "tele", "install-manifest.json"), nil
}

// Load reads the manifest at path; a missing file is an empty manifest.
func Load(path string) (*Manifest, error) {
	m := &Manifest{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// BackupDir is where replaced files are kept until rollback.
func (m *Manifest) BackupDir() string {
	return filepath.Join(filepath.Dir(m.path), "backup")
}

// Has reports whether a change with id is recorded.
func (m *Manifest) Has(id string) bool {
	return slices.ContainsFunc(m.Entries, func(e Entry) bool { return e.ID == id })
}

// Record appends e unless its ID is recorded already, and saves.
func (m *Manifest) Record(e Entry) error {
	if m.Has(e.ID) {
		return nil
	}
	m.Entries = append(m.Entries, e)
	return m.Save()
}

// Remove drops the entry with id and saves.
func (m *Manifest) Remove(id string) error {
	m.Entries = slices.DeleteFunc(m.Entries, func(e Entry) bool { return e.ID == id })
	return m.Save()
}

// Save writes the manifest atomically, or deletes the file when the
// manifest is empty.
func (m *Manifest) Save() error {
	if len(m.Entries) == 0 {
		err := os.Remove(m.path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(m.path, append(b, '\n'), 0o600)
}

// WriteFile writes data to path as change id, recording whether it created
// path or replaced a file, which it first copies to the backup directory.
func (m *Manifest) WriteFile(id, desc, path string, data []byte, perm fs.FileMode) error {
	if !m.Has(id) {
		e := Entry{ID: id, Desc: desc, Path: path}
		old, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := os.MkdirAll(m.BackupDir(), 0o700); err != nil {
				return err
			}
			e.Backup = filepath.Join(m.BackupDir(), id)
			fi, err := os.Stat(path)
			if err != nil {
				return err
			}
			if err := WriteAtomic(e.Backup, old, fi.Mode().Perm()); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if err := m.Record(e); err != nil {
			return err
		}
	}
	return WriteAtomic(path, data, perm)
}

// RestoreFile rolls back the file of e: it restores the backup, or removes
// the file tele created.
func RestoreFile(e Entry) error {
	if e.Path == "" {
		return nil
	}
	if e.Backup != "" {
		return os.Rename(e.Backup, e.Path)
	}
	err := os.Remove(e.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// WriteAtomic writes data to path through a temporary file and a rename,
// creating the directory with mode 0700.
func WriteAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

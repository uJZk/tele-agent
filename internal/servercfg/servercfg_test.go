package servercfg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/sstransport"
)

func TestSaveLoad(t *testing.T) {
	psk, err := sstransport.NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "tele", "server.json")
	if _, _, err := Load(p); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Load of a missing file = %v, want ErrNotConfigured", err)
	}
	if err := Save(p, &Config{Listen: ":8443", PSK: psk.Encode()}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if di, err := os.Stat(filepath.Dir(p)); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v; want 0700", di.Mode().Perm(), err)
	}
	c, got, err := Load(p)
	if err != nil || c.Listen != ":8443" || got != psk {
		t.Fatalf("Load = %+v, %v", c, err)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	psk, _ := sstransport.NewPSK()
	for name, tc := range map[string]struct {
		content string
		mode    os.FileMode
		want    string
	}{
		"open":      {`{"listen":":1","psk":"` + psk.Encode() + `"}`, 0o644, "chmod 600"},
		"badjson":   {`{`, 0o600, "parse"},
		"badlisten": {`{"listen":"nope","psk":"` + psk.Encode() + `"}`, 0o600, "listen address"},
		"badpsk":    {`{"listen":":1","psk":"secret-value"}`, 0o600, "invalid PSK"},
	} {
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, []byte(tc.content), tc.mode); err != nil {
			t.Fatal(err)
		}
		_, _, err := Load(p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret-value") {
			t.Errorf("%s: error leaks the PSK: %v", name, err)
		}
	}
}

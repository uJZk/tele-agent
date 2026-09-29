package hostcfg

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/sstransport"
)

func TestSaveLoadListRemove(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	psk, err := sstransport.NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := List(); err != nil || got != nil {
		t.Fatalf("List with no config = %v, %v", got, err)
	}
	for _, h := range []*Host{
		{Alias: "web", Endpoint: "tele.example.com:8443", PSK: psk.Encode()},
		{Alias: "dev", Endpoint: "unix:/run/tele.sock", Token: "s3cret"},
	} {
		if err := Save(h); err != nil {
			t.Fatal(err)
		}
	}
	dir, _ := Dir()
	fi, err := os.Stat(filepath.Join(dir, "web.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("host file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if got, err := List(); err != nil || !slices.Equal(got, []string{"dev", "web"}) {
		t.Fatalf("List = %v, %v", got, err)
	}

	h, err := Load("web")
	if err != nil {
		t.Fatal(err)
	}
	ep, token, err := h.Resolve("")
	if err != nil || !ep.Authenticates() || token != nil || ep.String() != "tele.example.com:8443" {
		t.Fatalf("Resolve(web) = %v, %q, %v", ep, token, err)
	}
	h, err = Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	if ep, token, err := h.Resolve(""); err != nil || ep.Authenticates() || string(token) != "s3cret" {
		t.Fatalf("Resolve(dev) = %v, %q, %v", ep, token, err)
	}
	if ep, _, err := h.Resolve("unix:/other.sock"); err != nil || ep.Address != "/other.sock" {
		t.Fatalf("Resolve(dev, override) = %v, %v", ep, err)
	}

	if err := Remove("web"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("web"); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("Load after Remove = %v, want ErrUnknownHost", err)
	}
	if err := Remove("web"); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("second Remove = %v, want ErrUnknownHost", err)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, _ := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(alias, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, alias+".json"), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("open", `{"endpoint":"unix:/s","token":"x"}`, 0o644)
	write("bad", `{`, 0o600)
	write("nopsk", `{"endpoint":"h:1"}`, 0o600)
	write("badpsk", `{"endpoint":"h:1","psk":"short"}`, 0o600)
	write("notoken", `{"endpoint":"unix:/s"}`, 0o600)

	for alias, want := range map[string]string{
		"missing": "unknown host",
		"open":    "chmod 600",
		"bad":     "parse",
		"nopsk":   "no PSK",
		"badpsk":  "invalid PSK",
		"notoken": "no token",
		"a:b":     "",
		"host":    "",
	} {
		h, err := Load(alias)
		if err == nil {
			_, _, err = h.Resolve("")
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want containing %q", alias, err, want)
		}
	}
	// A secret never appears in an error.
	write("leak", `{"endpoint":"h:1","psk":"not-a-valid-psk-value"}`, 0o600)
	h, _ := Load("leak")
	if _, _, err := h.Resolve(""); err == nil || strings.Contains(err.Error(), "not-a-valid-psk-value") {
		t.Errorf("Resolve error leaks the PSK: %v", err)
	}
}

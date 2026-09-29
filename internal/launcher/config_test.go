package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHost(t *testing.T, alias, content string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "tele", "hosts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, alias+".json"), []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestLoadHost(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeHost(t, "dev", `{"endpoint":"unix:/run/tele.sock","token":"s3cret"}`, 0o600)
	writeHost(t, "open", `{"endpoint":"unix:/run/tele.sock","token":"s3cret"}`, 0o644)
	writeHost(t, "notoken", `{"endpoint":"unix:/run/tele.sock"}`, 0o600)
	writeHost(t, "bad", `{`, 0o600)
	writeHost(t, "ss", `{"endpoint":"tele.example.com:8443","psk":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}`, 0o600)
	writeHost(t, "nopsk", `{"endpoint":"tele.example.com:8443"}`, 0o600)

	ep, tok, err := loadHost("dev", "")
	if err != nil || ep.Address != "/run/tele.sock" || string(tok) != "s3cret" {
		t.Fatalf("loadHost(dev) = %v, %q, %v", ep, tok, err)
	}
	ep, _, err = loadHost("dev", "unix:/other.sock")
	if err != nil || ep.Address != "/other.sock" {
		t.Fatalf("loadHost(dev, override) = %v, %v", ep, err)
	}
	ep, tok, err = loadHost("ss", "")
	if err != nil || !ep.Authenticates() || ep.Address != "tele.example.com:8443" || tok != nil {
		t.Fatalf("loadHost(ss) = %v, %q, %v", ep, tok, err)
	}
	for alias, wantErr := range map[string]string{
		"nopsk":   "no PSK",
		"missing": "unknown host",
		"open":    "chmod 600",
		"notoken": "no token",
		"bad":     "parse",
	} {
		if _, _, err := loadHost(alias, ""); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("loadHost(%s) err = %v, want containing %q", alias, err, wantErr)
		}
	}
}

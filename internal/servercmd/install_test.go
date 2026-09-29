package servercmd

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/pairing"
	"github.com/ujzk/tele-agent/internal/servercfg"
)

// fakeSystemctl answers like a systemd user manager and keeps the state of
// tele-server.service in $STATE.
const fakeSystemctl = `#!/bin/sh
echo "$*" >> "$STATE/calls"
case "$*" in
"--user show --property=Version") echo Version=255 ;;
"--user show --property=UnitFileState,ActiveState tele-server.service")
	echo "UnitFileState=$(cat "$STATE/enabled" 2>/dev/null || echo disabled)"
	echo "ActiveState=$(cat "$STATE/active" 2>/dev/null || echo inactive)" ;;
"--user enable tele-server.service") echo enabled > "$STATE/enabled" ;;
"--user restart tele-server.service") echo active > "$STATE/active" ;;
"--user disable --now tele-server.service") rm -f "$STATE/enabled" "$STATE/active" ;;
esac
`

// setupHost gives the test a home directory and a PATH with the fake
// systemctl, and returns the state directory.
func setupHost(t *testing.T) (home, state string) {
	home, state = t.TempDir(), t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(fakeSystemctl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("STATE", state)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return home, state
}

func calls(state string) string {
	b, _ := os.ReadFile(filepath.Join(state, "calls"))
	return string(b)
}

func TestInstallUninstall(t *testing.T) {
	home, state := setupHost(t)
	offer, err := pairing.NewOffer(9443)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := offer.Encode()
	pairFile := filepath.Join(t.TempDir(), "pairing.tmp")
	if err := os.WriteFile(pairFile, []byte(code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if rc := Main([]string{"install", "--pair-file", pairFile}, Streams{Out: &out, Err: &errb}); rc != 0 {
		t.Fatalf("install = %d\n%s%s", rc, out.String(), errb.String())
	}
	if _, err := os.Stat(pairFile); !os.IsNotExist(err) {
		t.Error("the pairing file survives install")
	}
	cfg, psk, err := servercfg.Load(filepath.Join(home, ".config", "tele", "server.json"))
	if err != nil || psk != offer.PSK || cfg.Listen != ":9443" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	unit, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", Service))
	if err != nil || !strings.Contains(string(unit), `ExecStart="`+filepath.Join(home, ".local", "bin", "tele")+`" server run`) {
		t.Fatalf("unit = %s, %v", unit, err)
	}
	if c := calls(state); !strings.Contains(c, "--user enable "+Service) || !strings.Contains(c, "--user restart "+Service) {
		t.Errorf("systemctl calls:\n%s", c)
	}
	m := regexp.MustCompile(`tele1r:\S+`).FindString(out.String())
	r, err := pairing.ParseReceipt(m)
	if err != nil {
		t.Fatalf("receipt %q: %v\n%s", m, err, out.String())
	}
	if err := r.Verify(offer.PSK, offer.Token); err != nil || r.Port != 9443 {
		t.Fatalf("receipt: port %d, %v", r.Port, err)
	}

	// Installing again changes nothing and restarts nothing.
	_ = os.Remove(filepath.Join(state, "calls"))
	out.Reset()
	if rc := Main([]string{"install"}, Streams{Out: &out, Err: &errb}); rc != 0 {
		t.Fatalf("second install = %d\n%s%s", rc, out.String(), errb.String())
	}
	if c := calls(state); strings.Contains(c, "restart") || strings.Contains(out.String(), "🔧") {
		t.Errorf("second install repaired something:\n%s\n%s", out.String(), c)
	}

	// A new key needs consent, which a run without a terminal cannot give.
	other, _ := pairing.NewOffer(9443)
	code2, _ := other.Encode()
	out.Reset()
	errb.Reset()
	if rc := Main([]string{"install", "--pair", code2}, Streams{Out: &out, Err: &errb}); rc != 1 || !strings.Contains(errb.String(), "--yes") {
		t.Fatalf("install with another key = %d\n%s%s", rc, out.String(), errb.String())
	}
	if _, psk, _ := servercfg.Load(filepath.Join(home, ".config", "tele", "server.json")); psk != offer.PSK {
		t.Fatal("the key was replaced without consent")
	}

	out.Reset()
	if rc := Main([]string{"uninstall"}, Streams{Out: &out, Err: &errb}); rc != 0 {
		t.Fatalf("uninstall = %d\n%s%s", rc, out.String(), errb.String())
	}
	for _, p := range []string{
		filepath.Join(home, ".config", "tele", "server.json"),
		filepath.Join(home, ".config", "tele", "install-manifest.json"),
		filepath.Join(home, ".config", "systemd", "user", Service),
		filepath.Join(home, ".local", "bin", "tele"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survives uninstall", p)
		}
	}
	c := calls(state)
	if i, j := strings.Index(c, "disable --now"), strings.LastIndex(c, "daemon-reload"); i < 0 || j < i {
		t.Errorf("uninstall did not disable, then reload:\n%s", c)
	}
}

func TestInstallNotPaired(t *testing.T) {
	setupHost(t)
	var out, errb bytes.Buffer
	if rc := Main([]string{"install"}, Streams{Out: &out, Err: &errb}); rc != 1 || !strings.Contains(errb.String(), "not paired") {
		t.Fatalf("install = %d\n%s%s", rc, out.String(), errb.String())
	}
	// --check needs no pairing and changes nothing.
	out.Reset()
	if rc := Main([]string{"install", "--check"}, Streams{Out: &out, Err: &errb}); rc != 1 || !strings.Contains(out.String(), "❌ configuration") {
		t.Fatalf("install --check = %d\n%s", rc, out.String())
	}
	if rc := Main([]string{"install", "--check", "--yes"}, Streams{Out: &out, Err: &errb}); rc != 2 {
		t.Errorf("--check --yes = %d, want a usage error", rc)
	}
}

func TestKernelVersion(t *testing.T) {
	for in, want := range map[string][2]int{"6.8.0-45-generic": {6, 8}, "3.15.0": {3, 15}, "5.4": {5, 4}, "4.19.0+": {4, 19}} {
		if got, ok := kernelVersion(in); !ok || got != want {
			t.Errorf("kernelVersion(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := kernelVersion("garbage"); ok {
		t.Error("parsed garbage")
	}
}

func TestSystemdQuote(t *testing.T) {
	if got := systemdQuote(`/home/a b/100%/$x"\`); got != `"/home/a b/100%%/$$x\"\\"` {
		t.Errorf("systemdQuote = %s", got)
	}
}

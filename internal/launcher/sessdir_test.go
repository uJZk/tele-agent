package launcher

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/cabundle"
	"github.com/ujzk/tele-agent/internal/shimsrv"
	"github.com/ujzk/tele-agent/internal/testutil/certtest"
)

func TestCAExtras(t *testing.T) {
	got := caExtras([]string{
		"SSL_CERT_FILE=/f.pem", "PATH=/bin", "SSL_CERT_DIR=/a:/b", "NODE_EXTRA_CA_CERTS=/n.pem",
	})
	want := []cabundle.Extra{
		{Var: "SSL_CERT_FILE", Path: "/f.pem"},
		{Var: "SSL_CERT_DIR", Path: "/a"},
		{Var: "SSL_CERT_DIR", Path: "/b"},
		{Var: "NODE_EXTRA_CA_CERTS", Path: "/n.pem"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("caExtras = %v, want %v", got, want)
	}
}

func TestPrepareSessionDir(t *testing.T) {
	tmp := t.TempDir()
	certtest.WriteCA(t, filepath.Join(tmp, "corp.pem"), "tele test corp CA")
	dir := filepath.Join(tmp, "0123456789abcdef")
	var warn bytes.Buffer
	sd, err := prepareSessionDir(sessDirSpec{
		Dir: dir,
		UserEnv: []string{
			"NODE_EXTRA_CA_CERTS=" + filepath.Join(tmp, "corp.pem"),
			"SSL_CERT_FILE=" + filepath.Join(tmp, "missing.pem"),
		},
		Token:        []byte("tok"),
		SystemPrompt: "# Target host (tele)\n",
		LogLevel:     slog.LevelInfo,
		Warn:         &warn,
	})
	if err != nil {
		t.Fatal(err)
	}
	sd.Log.Info("hello from session main")
	if err := sd.Close(); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"", binDir, libDir, tmpDir} {
		fi, err := os.Stat(filepath.Join(dir, d))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%q: %v, %v; want a 0700 directory", d, fi, err)
		}
	}
	for name, want := range map[string]string{
		shimsrv.TokenFile: "tok",
		systemPromptFile:  "# Target host (tele)\n",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	for _, name := range []string{shimsrv.TokenFile, shimsrv.LogFile, caBundleFile, systemPromptFile} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v; want mode 0600", name, fi, err)
		}
	}

	ca, err := os.ReadFile(filepath.Join(dir, caBundleFile))
	if err != nil {
		t.Fatal(err)
	}
	if names := certtest.Names(t, ca); !slices.Contains(names, "tele test corp CA") {
		t.Errorf("CA bundle lacks NODE_EXTRA_CA_CERTS: %v", names)
	}
	// The unreadable SSL_CERT_FILE only warns, naming the variable, both to
	// the user and in the session log.
	if !strings.Contains(warn.String(), "SSL_CERT_FILE") {
		t.Errorf("warning %q does not name SSL_CERT_FILE", warn.String())
	}
	log, err := os.ReadFile(filepath.Join(dir, shimsrv.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SSL_CERT_FILE", "hello from session main"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("session log lacks %q:\n%s", want, log)
		}
	}
}

func TestPrepareSessionDirExisting(t *testing.T) {
	dir := t.TempDir() // exists already
	if err := os.WriteFile(filepath.Join(dir, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSessionDir(sessDirSpec{Dir: dir}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	// A directory tele did not create is left alone.
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Fatal(err)
	}
}

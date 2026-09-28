package cabundle

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newCert returns a self-signed CA certificate, DER-encoded.
func newCert(t *testing.T, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func pemOf(ders ...[]byte) []byte {
	var b bytes.Buffer
	for _, d := range ders {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: d})
	}
	return b.Bytes()
}

func write(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// names parses a bundle and returns the common names in order, failing on
// anything that is not a parseable CERTIFICATE block.
func names(t *testing.T, bundle []byte) []string {
	t.Helper()
	var out []string
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			t.Fatalf("bundle holds a %q block with headers %v", block.Type, block.Headers)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("bundle holds an unparseable certificate: %v", err)
		}
		out = append(out, c.Subject.CommonName)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("bundle has trailing data %q", rest)
	}
	return out
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	a, b, c, d, e := newCert(t, "a"), newCert(t, "b"), newCert(t, "c"), newCert(t, "d"), newCert(t, "e")

	sys1 := write(t, filepath.Join(dir, "sys1.crt"), pemOf(a, b))
	sys2 := write(t, filepath.Join(dir, "sys2.crt"), pemOf(c))
	sysDir := filepath.Join(dir, "sysdir")
	write(t, filepath.Join(sysDir, "s.pem"), pemOf(d))
	missing := filepath.Join(dir, "missing")

	userFile := write(t, filepath.Join(dir, "user.pem"), append(pemOf(b), pemOf(e)...)) // b duplicates a system cert
	userDir := filepath.Join(dir, "userdir")
	write(t, filepath.Join(userDir, "1.crt"), pemOf(c))
	write(t, filepath.Join(userDir, "0123abcd.0"), pemOf(d))
	write(t, filepath.Join(userDir, "README"), pemOf(e))     // wrong name: ignored
	write(t, filepath.Join(userDir, "0123abcd.x"), pemOf(e)) // not a hash name
	if err := os.Symlink(missing, filepath.Join(userDir, "dangling.pem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(userDir, "sub.pem"), 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		sysFiles []string
		sysDirs  []string
		extra    []string
		want     []string
	}{
		{name: "first readable system file", sysFiles: []string{missing, sysDir, sys1, sys2}, sysDirs: []string{sysDir}, want: []string{"a", "b"}},
		{name: "system dirs when no file", sysFiles: []string{missing}, sysDirs: []string{missing, sysDir}, want: []string{"d"}},
		{name: "extra file dedups", sysFiles: []string{sys1}, extra: []string{userFile, userFile}, want: []string{"a", "b", "e"}},
		{name: "extra dir", sysFiles: []string{sys2}, extra: []string{"", userDir}, want: []string{"c", "d"}},
		{name: "extras only", extra: []string{userDir, userFile}, want: []string{"d", "c", "b", "e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := build(tt.sysFiles, tt.sysDirs, tt.extra)
			if err != nil {
				t.Fatal(err)
			}
			if n := names(t, got); !slices.Equal(n, tt.want) {
				t.Fatalf("bundle = %v, want %v", n, tt.want)
			}
		})
	}
}

func TestBuildGarbage(t *testing.T) {
	dir := t.TempDir()
	good := newCert(t, "good")
	var in bytes.Buffer
	in.WriteString("leading text\n")
	_ = pem.Encode(&in, &pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a key")})
	_ = pem.Encode(&in, &pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")})
	_ = pem.Encode(&in, &pem.Block{Type: "TRUSTED CERTIFICATE", Bytes: good})
	_ = pem.Encode(&in, &pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"X": "y"}, Bytes: good})
	in.WriteString("-----BEGIN CERTIFICATE-----\ntruncated")
	f := write(t, filepath.Join(dir, "mixed.pem"), in.Bytes())

	got, err := build(nil, nil, []string{f})
	if err != nil {
		t.Fatal(err)
	}
	if n := names(t, got); !slices.Equal(n, []string{"good"}) {
		t.Fatalf("bundle = %v, want [good]", n)
	}

	junk := write(t, filepath.Join(dir, "junk.pem"), []byte("\x00\xff no pem here"))
	if _, err := build([]string{junk}, nil, []string{junk}); !errors.Is(err, ErrNoCertificates) {
		t.Fatalf("build of garbage = %v, want ErrNoCertificates", err)
	}
	if _, err := build(nil, nil, nil); !errors.Is(err, ErrNoCertificates) {
		t.Fatalf("build of nothing = %v, want ErrNoCertificates", err)
	}
}

func TestBuildExtraErrors(t *testing.T) {
	dir := t.TempDir()
	sys := write(t, filepath.Join(dir, "sys.crt"), pemOf(newCert(t, "sys")))
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	big := write(t, filepath.Join(dir, "big.pem"), bytes.Repeat([]byte{'x'}, maxFileSize+1))

	for _, p := range []string{filepath.Join(dir, "missing.pem"), fifo, big} {
		_, err := build([]string{sys}, nil, []string{p})
		if err == nil {
			t.Errorf("build with extra %s succeeded", filepath.Base(p))
			continue
		}
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not name %s", err, p)
		}
	}
	// A FIFO or oversized file among the system candidates is skipped.
	got, err := build([]string{fifo, big, sys}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := names(t, got); !slices.Equal(n, []string{"sys"}) {
		t.Fatalf("bundle = %v, want [sys]", n)
	}
}

func TestIsCertFileName(t *testing.T) {
	for name, want := range map[string]bool{
		"a.pem": true, "b.crt": true, "0123abcd.0": true, "deadbeef.12": true,
		"README": false, "a.key": false, "0123abcd.": false, "0123ABCD.0": false,
		"0123abc.0": false, "0123abcd.r0": false, "0123abcd.0.bak": false,
	} {
		if got := isCertFileName(name); got != want {
			t.Errorf("isCertFileName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestBuildSystem checks the real system store where one exists; the unit
// tests must also pass on hosts without one.
func TestBuildSystem(t *testing.T) {
	found := false
	for _, p := range append(slices.Clone(systemFiles), systemDirs...) {
		if _, err := os.Stat(p); err == nil {
			found = true
		}
	}
	if !found {
		t.Skip("no system CA store on this host")
	}
	got, err := Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(names(t, got)) == 0 {
		t.Fatal("empty system bundle")
	}
}

func FuzzAddPEM(f *testing.F) {
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"))
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		b := bundle{seen: make(map[string]struct{})}
		b.addPEM(data)
		b.addPEM(data)
		n := names(t, b.out.Bytes())
		if len(n) != len(b.seen) {
			t.Fatalf("%d certificates written, %d seen", len(n), len(b.seen))
		}
	})
}

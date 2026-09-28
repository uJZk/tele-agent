// Package cabundle builds the PEM bundle of CA certificates that Claude
// trusts during a session.
//
// Claude's TLS connections leave through the local proxy, so they must
// trust what the local network requires (a corporate TLS-inspection CA, for
// instance), yet Claude sees the remote filesystem, whose /etc/ssl may be
// missing or stale. The bundle therefore merges the local system roots with
// the user's own extra CA files; session main writes it into the session
// directory and points SSL_CERT_FILE and NODE_EXTRA_CA_CERTS at it
// (docs/filesystem.md section 2).
package cabundle

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// systemFiles and systemDirs are the places crypto/x509 looks for system
// roots on Linux (certFiles and certDirectories in
// src/crypto/x509/root_linux.go). Build does not trust exactly what Go
// does: Go reads every file in the directories in addition to the first
// file, and SSL_CERT_FILE and SSL_CERT_DIR replace its lists, whereas Build
// reads the directories only when no file is readable (see Build).
var (
	systemFiles = []string{
		"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Gentoo, Arch
		"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora, RHEL 6
		"/etc/ssl/ca-bundle.pem",                            // openSUSE
		"/etc/pki/tls/cacert.pem",                           // OpenELEC
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS, RHEL 7
		"/etc/ssl/cert.pem",                                 // Alpine
	}
	systemDirs = []string{
		"/etc/ssl/certs",     // SLES
		"/etc/pki/tls/certs", // Fedora, RHEL
	}
)

// maxFileSize bounds each file read. System bundles are a few hundred KiB;
// the bound only protects against a misconfigured path.
const maxFileSize = 16 << 20

// ErrNoCertificates reports that no usable CA certificate was found.
var ErrNoCertificates = errors.New("cabundle: no CA certificates found; " +
	"install the system CA package (ca-certificates) or set SSL_CERT_FILE to a PEM bundle")

// Extra is a CA file or directory the user configured.
type Extra struct {
	// Var names the environment variable that set Path, such as
	// NODE_EXTRA_CA_CERTS, so that errors can tell the user what to fix.
	Var string
	// Path is a PEM file (SSL_CERT_FILE, NODE_EXTRA_CA_CERTS) or a
	// directory (an SSL_CERT_DIR entry); empty means unset.
	Path string
}

// Build returns a PEM bundle of the local system roots and the certificates
// in extra, deduplicated, in the order found.
//
// The system roots come from the first readable file in the list
// crypto/x509 uses; the per-certificate directories are read only when no
// such file exists, since on common distributions they duplicate the file.
// Certificates present only in a directory are therefore missed on hosts
// that also have a bundle file.
//
// From a directory in extra, files named *.pem, *.crt or OpenSSL's
// <hash>.<n> are read and unreadable entries skipped. Entries with an empty
// Path are ignored so that unset variables can be passed as they are; any
// other entry that cannot be read is an error naming its variable, because
// silently dropping a CA the user configured would surface later as an
// opaque TLS failure.
//
// Only CERTIFICATE blocks that parse as X.509 are kept; anything else in
// the input is ignored.
func Build(extra []Extra) ([]byte, error) {
	return build(systemFiles, systemDirs, extra)
}

func build(sysFiles, sysDirs []string, extra []Extra) ([]byte, error) {
	b := bundle{seen: make(map[string]struct{})}
	if !b.addFirstFile(sysFiles) {
		for _, d := range sysDirs {
			_ = b.addDir(d) // best effort, like crypto/x509
		}
	}
	for _, e := range extra {
		if e.Path == "" {
			continue
		}
		if err := b.addPath(e.Path); err != nil {
			return nil, fmt.Errorf("cabundle: read CA certificates named by %s: %w; fix or unset %s",
				e.Var, err, e.Var)
		}
	}
	if b.out.Len() == 0 {
		return nil, ErrNoCertificates
	}
	return b.out.Bytes(), nil
}

type bundle struct {
	out  bytes.Buffer
	seen map[string]struct{} // DER of every certificate in out
}

// addFirstFile adds the first readable file in paths and reports whether
// one was found.
func (b *bundle) addFirstFile(paths []string) bool {
	for _, p := range paths {
		data, isDir, err := readFile(p)
		if err != nil || isDir {
			continue
		}
		b.addPEM(data)
		return true
	}
	return false
}

// addPath adds a file, or the certificate files in a directory. Errors name
// the path.
func (b *bundle) addPath(p string) error {
	data, isDir, err := readFile(p)
	if err != nil {
		return err
	}
	if isDir {
		return b.addDir(p)
	}
	b.addPEM(data)
	return nil
}

func (b *bundle) addDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !isCertFileName(e.Name()) {
			continue
		}
		// Directories commonly hold dangling hash links; skip what cannot
		// be read instead of failing the whole directory.
		data, isDir, err := readFile(filepath.Join(dir, e.Name()))
		if err == nil && !isDir {
			b.addPEM(data)
		}
	}
	return nil
}

// addPEM appends every CERTIFICATE block of data that parses and has not
// been seen yet, re-encoded without PEM headers.
func (b *bundle) addPEM(data []byte) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			continue
		}
		if _, dup := b.seen[string(block.Bytes)]; dup {
			continue
		}
		b.seen[string(block.Bytes)] = struct{}{}
		// Writing to a bytes.Buffer cannot fail.
		_ = pem.Encode(&b.out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
	}
}

// isCertFileName reports whether a directory entry may hold certificates:
// *.pem, *.crt, or an OpenSSL c_rehash name, 8 hex digits "." digits.
func isCertFileName(name string) bool {
	if strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".crt") {
		return true
	}
	hash, n, ok := strings.Cut(name, ".")
	if !ok || len(hash) != 8 || n == "" {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// readFile reads a regular file (following symlinks) or reports that p is a
// directory. It opens with O_NONBLOCK so that a FIFO given by mistake is
// rejected instead of blocking forever in open(2).
func readFile(p string) (data []byte, isDir bool, err error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	switch {
	case fi.IsDir():
		return nil, true, nil
	case !fi.Mode().IsRegular():
		return nil, false, fmt.Errorf("%s: not a regular file or directory", p)
	}
	data, err = io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxFileSize {
		return nil, false, fmt.Errorf("%s: larger than %d MiB", p, maxFileSize>>20)
	}
	return data, false, nil
}

// Package certtest makes self-signed certificates for tests.
package certtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"
)

// SelfSigned returns a CA certificate for the given DNS names, valid now
// and signed by itself, and its PEM encoding. The first name is also the
// subject's common name.
func SelfSigned(t testing.TB, names ...string) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              names,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// WriteCA writes a self-signed CA certificate named cn to path as PEM.
func WriteCA(t testing.TB, path, cn string) {
	t.Helper()
	_, p := SelfSigned(t, cn)
	if err := os.WriteFile(path, p, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Names returns the subject common names of the certificates in a PEM
// bundle, in order.
func Names(t testing.TB, bundle []byte) []string {
	t.Helper()
	var names []string
	for {
		var b *pem.Block
		b, bundle = pem.Decode(bundle)
		if b == nil {
			return names
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, c.Subject.CommonName)
	}
}

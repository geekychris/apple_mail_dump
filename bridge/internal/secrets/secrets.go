// Package secrets creates and loads the bridge's TLS certificate and bearer
// token. Both are generated on the Mac at first run and stored with mode 0600
// in the bridge's Application Support directory.
package secrets

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Files names the secret material under dir.
type Files struct{ Dir string }

func (f Files) Cert() string  { return filepath.Join(f.Dir, "tls-cert.pem") }
func (f Files) Key() string   { return filepath.Join(f.Dir, "tls-key.pem") }
func (f Files) Token() string { return filepath.Join(f.Dir, "token") }

// Ensure creates the directory, certificate and token when missing. hosts are
// the DNS names / IPs put in the certificate's SANs (informational: clients
// pin the fingerprint rather than trusting names). It never overwrites.
func (f Files) Ensure(hosts []string) (created []string, err error) {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(f.Dir, 0o700)
	if !exists(f.Token()) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		tok := base64.RawURLEncoding.EncodeToString(b)
		if err := writeExclusive(f.Token(), []byte(tok+"\n")); err != nil {
			return nil, err
		}
		created = append(created, "token")
	}
	if !exists(f.Cert()) || !exists(f.Key()) {
		certPEM, keyPEM, err := selfSigned(hosts)
		if err != nil {
			return nil, err
		}
		_ = os.Remove(f.Cert())
		_ = os.Remove(f.Key())
		if err := writeExclusive(f.Key(), keyPEM); err != nil {
			return nil, err
		}
		if err := writeExclusive(f.Cert(), certPEM); err != nil {
			return nil, err
		}
		created = append(created, "certificate")
	}
	return created, nil
}

// LoadToken reads the token and refuses one readable by group or others.
func (f Files) LoadToken() (string, error) {
	st, err := os.Stat(f.Token())
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must be mode 0600 (is %o)", f.Token(), st.Mode().Perm())
	}
	b, err := os.ReadFile(f.Token())
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 32 {
		return "", errors.New("token file is too short")
	}
	return t, nil
}

// LoadCert returns the TLS keypair and its SHA-256 fingerprint.
func (f Files) LoadCert() (tls.Certificate, string, error) {
	c, err := tls.LoadX509KeyPair(f.Cert(), f.Key())
	if err != nil {
		return c, "", err
	}
	return c, Fingerprint(c.Certificate[0]), nil
}

// Fingerprint is the SHA-256 of the DER certificate as colon-separated hex,
// the same form `openssl x509 -fingerprint -sha256` prints.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// TokenEqual compares in constant time.
func TokenEqual(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1 && want != ""
}

func selfSigned(hosts []string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mailbridge", Organization: []string{"mailbridge self-signed"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func writeExclusive(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

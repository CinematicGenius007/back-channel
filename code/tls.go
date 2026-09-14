package main

// Public-server support.
//
// TLS model: the hub generates a self-signed ECDSA certificate once and prints its
// SHA-256 fingerprint. Clients pin that fingerprint (trust-on-first-use, like SSH)
// instead of relying on a CA — so a bare VPS IP with no domain works, and a
// man-in-the-middle can't swap certificates without every client noticing.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// loadOrCreateCert returns the hub's certificate and its fingerprint.
func loadOrCreateCert(dir string) (tls.Certificate, string, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certPath); err != nil {
		if err := generateCert(certPath, keyPath); err != nil {
			return tls.Certificate{}, "", err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, fingerprint(leaf), nil
}

func generateCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dropchan hub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
}

// fingerprint is the SHA-256 of the DER certificate, formatted like SSH/OpenSSL.
func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

func normFP(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
}

// errFingerprint is returned when the hub's certificate doesn't match the pin.
type errFingerprint struct{ want, got string }

func (e *errFingerprint) Error() string {
	return fmt.Sprintf("HUB CERTIFICATE CHANGED — possible man-in-the-middle.\n  pinned: %s\n  got:    %s\n  If the hub was legitimately reinstalled, reconnect with -fingerprint %q", e.want, e.got, e.got)
}

// clientTLSConfig verifies the hub by fingerprint only. If *pin is empty, the
// first certificate seen is accepted and written back into *pin (TOFU); the
// caller is responsible for persisting it and telling the user.
func clientTLSConfig(pin *string, onFirstUse func(fp string)) *tls.Config {
	var mu sync.Mutex
	return &tls.Config{
		InsecureSkipVerify: true, // we verify by pin below, not by CA
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("hub sent no certificate")
			}
			got := fingerprint(cs.PeerCertificates[0])
			mu.Lock()
			defer mu.Unlock()
			if *pin == "" {
				*pin = got
				if onFirstUse != nil {
					onFirstUse(got)
				}
				return nil
			}
			if normFP(*pin) != normFP(got) {
				return &errFingerprint{want: *pin, got: got}
			}
			return nil
		},
	}
}

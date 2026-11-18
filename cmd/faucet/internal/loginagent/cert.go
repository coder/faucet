package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/xerrors"
)

// writeCert writes a self-signed certificate for 127.0.0.1, ::1 and
// localhost to dir/cert.pem, and its key to dir/key.pem. The tests serve
// client ID metadata documents with it, and start faucet with SSL_CERT_FILE
// naming cert.pem, which Go reads in place of the system's roots, so the
// document client of faucet trusts it and nothing else.
func writeCert(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return xerrors.Errorf("make the key: %w", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "faucet end-to-end documents"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(7 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return xerrors.Errorf("make the certificate: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return xerrors.Errorf("encode the key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return xerrors.Errorf("make %s: %w", dir, err)
	}
	for name, block := range map[string]*pem.Block{
		"cert.pem": {Type: "CERTIFICATE", Bytes: der},
		"key.pem":  {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			return xerrors.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

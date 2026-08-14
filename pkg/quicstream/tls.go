package quicstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/messageloopio/messageloop/shared"
)

// loadTLSConfig builds the server TLS 1.3 config. QUIC requires TLS; when
// Insecure is set and no cert files are given, an ephemeral self-signed
// certificate is generated for local development.
func loadTLSConfig(opts Options) (*tls.Config, error) {
	var cert tls.Certificate
	var err error
	switch {
	case opts.TLSCertFile != "" && opts.TLSKeyFile != "":
		cert, err = tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load quic tls credentials: %w", err)
		}
	case opts.Insecure:
		cert, err = generateSelfSignedCert()
		if err != nil {
			return nil, fmt.Errorf("generate self-signed quic certificate: %w", err)
		}
	default:
		return nil, fmt.Errorf("quic tls cert_file and key_file are required (or set insecure: true)")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   shared.ALPNProtocols(),
	}, nil
}

// generateSelfSignedCert creates a short-lived ECDSA certificate valid for
// localhost and 127.0.0.1. It is intended for tests and development only.
func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"MessageLoop"}, CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}

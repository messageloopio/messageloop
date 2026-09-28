package messageloopgo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// This file exercises the DialGRPC TLS support end to end against the real
// server binary. The deployment form terminates TLS at the platform edge
// (Traefik) and forwards h2c to the backend (docker/dokploy/README.md §4.2),
// so the test starts its own TLS terminating front in front of the spawned
// server's plaintext gRPC listener: tls.Listen with a self-signed certificate
// plus transparent byte forwarding. The plaintext side then carries exactly
// the HTTP/2 frames a client sends to an h2c backend, which makes the front
// equivalent to the edge topology, and the SDK side walks the full
// credentials.NewTLS path.

// runE2EGRPCTLSScenarios exercises the SDK's gRPC TLS credentials against the
// in-test TLS terminating front. frontWithIP carries a certificate with an IP
// SAN (dialed directly, no ServerName override); frontWithDNS carries a
// certificate with only a DNS SAN, so succeeding against it proves the
// ServerName override is honored.
func runE2EGRPCTLSScenarios(t *testing.T, srv *e2eServerProcess) {
	t.Helper()

	ipCertificate, ipPool := e2eSelfSignedCertificate(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	frontWithIP := startE2ETLSTerminatingFront(t, srv.grpcAddr, ipCertificate)

	dnsCertificate, dnsPool := e2eSelfSignedCertificate(t, []string{"messageloop.test"}, nil)
	frontWithDNS := startE2ETLSTerminatingFront(t, srv.grpcAddr, dnsCertificate)

	t.Run("CustomCAPool", func(t *testing.T) {
		runE2EGRPCTLSRoundTrip(t, frontWithIP, e2eNamespace()+".grpctls",
			WithTLSConfig(&tls.Config{RootCAs: ipPool}))
	})
	t.Run("InsecureSkipVerify", func(t *testing.T) {
		runE2EGRPCTLSRoundTrip(t, frontWithIP, e2eNamespace()+".grpctls",
			WithTLS(), WithInsecureSkipVerify())
	})
	t.Run("ServerNameOverride", func(t *testing.T) {
		runE2EGRPCTLSRoundTrip(t, frontWithDNS, e2eNamespace()+".grpctls",
			WithTLSConfig(&tls.Config{RootCAs: dnsPool, ServerName: "messageloop.test"}))
	})
	t.Run("ServerNameMismatchFailsClosed", func(t *testing.T) {
		expectE2EGRPCTLSConnectFailure(t, frontWithDNS, "custom CA pool without ServerName (DNS-only certificate)", WithTLSConfig(&tls.Config{RootCAs: dnsPool}))
	})
	t.Run("ReconnectDialPathUsesTLS", func(t *testing.T) {
		// The reconnect path dials through client.dialTransport; a succeeded
		// dial (newGRPCTransport waits for the MessageLoop stream) proves the
		// TLS credentials are applied there too, so a re-dial cannot silently
		// fall back to plaintext.
		sdkClient, err := DialGRPC(frontWithIP, WithTLSConfig(&tls.Config{RootCAs: ipPool}))
		if err != nil {
			t.Fatalf("DialGRPC(%s) via TLS front failed: %v", frontWithIP, err)
		}
		t.Cleanup(func() { _ = sdkClient.Close() })

		c, ok := sdkClient.(*client)
		if !ok {
			t.Fatalf("client type = %T, want *client", sdkClient)
		}
		trans, err := c.dialTransport()
		if err != nil {
			t.Fatalf("reconnect dial over TLS front failed: %v", err)
		}
		_ = trans.Close()
	})
	t.Run("SystemRootsRejectSelfSigned", func(t *testing.T) {
		expectE2EGRPCTLSConnectFailure(t, frontWithIP, "WithTLS() against a self-signed certificate", WithTLS())
	})
	t.Run("PlaintextRejectedByTLSFront", func(t *testing.T) {
		expectE2EGRPCTLSConnectFailure(t, frontWithIP, "plaintext default")
	})
}

// runE2EGRPCTLSRoundTrip dials the TLS front, subscribes to channel, publishes
// one text message and waits for it to be delivered back: one publish /
// subscribe round trip over the full SDK TLS credential path.
func runE2EGRPCTLSRoundTrip(t *testing.T, tlsAddr, channel string, opts ...Option) {
	t.Helper()

	dialOpts := append([]Option{WithAutoSubscribe(channel)}, opts...)
	client, err := DialGRPC(tlsAddr, dialOpts...)
	if err != nil {
		t.Fatalf("DialGRPC(%s) via TLS front failed: %v", tlsAddr, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	received := make(chan *Message, 8)
	client.OnMessage(collectE2EChannel(received, channel))
	connectE2E(t, client)

	const payload = "grpc-tls-roundtrip"
	if err := client.Publish(channel, NewMessageWithData("e2e.tls", NewTextData(payload))); err != nil {
		t.Fatalf("publish over TLS front failed: %v", err)
	}
	msg := waitE2EMessage(t, received, "TLS-delivered message")
	if got := msg.Data.AsText(); got != payload {
		t.Fatalf("TLS message payload = %q, want %q", got, payload)
	}
}

// expectE2EGRPCTLSConnectFailure asserts that the dial attempt cannot complete
// a MessageLoop session against the TLS front and reports the observed error.
// It backs the negative controls: certificate verification stays on, and a
// plaintext client cannot talk to a TLS listener. The handshake failure
// usually surfaces while DialGRPC creates the MessageLoop stream (the stream
// waits for a ready transport), but a late failure at Connect is accepted too.
func expectE2EGRPCTLSConnectFailure(t *testing.T, tlsAddr, what string, opts ...Option) {
	t.Helper()

	client, err := DialGRPC(tlsAddr, opts...)
	if err != nil {
		t.Logf("%s rejected by TLS front at dial (expected): %v", what, err)
		return
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), e2eStepTimeout)
	defer cancel()
	err = client.Connect(ctx)
	if err == nil {
		t.Fatalf("%s connected to the TLS front, want a failure", what)
	}
	t.Logf("%s rejected by TLS front at connect (expected): %v", what, err)
}

// e2eSelfSignedCertificate generates a self-signed certificate for the given
// DNS names and IP addresses, returning the server certificate and a pool
// trusting it (the TestRootCAs fixture).
func e2eSelfSignedCertificate(t *testing.T, dnsNames []string, ipAddresses []net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate certificate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "messageloop-e2e-tls-front"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// startE2ETLSTerminatingFront starts a TLS listener on a free loopback port
// that forwards every decrypted connection to backendAddr byte for byte
// (backendAddr is the spawned server's plaintext gRPC listener). It returns
// the front address to dial.
func startE2ETLSTerminatingFront(t *testing.T, backendAddr string, cert tls.Certificate) string {
	t.Helper()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
	})
	if err != nil {
		t.Fatalf("listen TLS front: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			clientConn, err := listener.Accept()
			if err != nil {
				return
			}
			go proxyE2ETLSConn(clientConn, backendAddr)
		}
	}()

	return listener.Addr().String()
}

// proxyE2ETLSConn byte-forwards one TLS front connection to the plaintext
// backend. The TLS handshake happens on the first read; a plaintext client
// fails it and both sides are closed.
func proxyE2ETLSConn(clientConn net.Conn, backendAddr string) {
	defer func() { _ = clientConn.Close() }()

	backendConn, err := net.Dial("tcp", backendAddr)
	if err != nil {
		return
	}
	defer func() { _ = backendConn.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(backendConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, backendConn)
		done <- struct{}{}
	}()
	<-done
}

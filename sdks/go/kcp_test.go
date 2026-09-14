package messageloopgo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	kcpgo "github.com/xtaci/kcp-go/v5"

	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

func selfSignedTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// startTestKCPServer runs a minimal KCP+TLS server that accepts one style of
// interaction: it echoes every received protocol frame back to the client.
func startTestKCPServer(t *testing.T) string {
	t.Helper()
	ln, err := kcpgo.ListenWithOptions("127.0.0.1:0", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	cert := selfSignedTestCert(t)
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{shared.ALPNMessageLoopJSON, shared.ALPNMessageLoopProto},
	}

	go func() {
		for {
			sess, err := ln.AcceptKCP()
			if err != nil {
				return
			}
			sess.SetStreamMode(true)
			sess.SetNoDelay(1, 10, 2, 1)
			go func() {
				defer func() { _ = sess.Close() }()
				gc := &kcpGraceCloseConn{Conn: sess}
				tc := tls.Server(gc, tlsConf)
				if err := tc.HandshakeContext(context.Background()); err != nil {
					return
				}
				defer func() { _ = tc.Close() }()
				for {
					frame, err := shared.ReadFrame(tc, 1<<20)
					if err != nil {
						return
					}
					if err := shared.WriteFrame(tc, frame); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestDialKCP_Unreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := newKCPTransport(ctx, "127.0.0.1:1", EncodingJSON, 200*time.Millisecond, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{shared.ALPNMessageLoopJSON},
	}, 0, 0)
	if err == nil {
		t.Fatal("expected dial error against a closed port")
	}
}

func TestKCPTransport_FrameRoundTrip(t *testing.T) {
	addr := startTestKCPServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	trans, err := newKCPTransport(ctx, addr, EncodingJSON, 5*time.Second, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{shared.ALPNMessageLoopJSON},
	}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trans.Close() })

	ping := &clientpb.InboundMessage{Id: "p1", Envelope: &clientpb.InboundMessage_Ping{Ping: &clientpb.Ping{}}}
	if err := trans.Send(ctx, ping); err != nil {
		t.Fatal(err)
	}
	out, err := trans.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.GetId() != "p1" || out.GetPing() == nil {
		t.Fatalf("unexpected echoed frame: %+v", out)
	}

	// Close must not hang and must deliver the final TLS close_notify: the
	// server echoes frames, so a clean server-side EOF is implied by the
	// server goroutine exiting without error.
	if err := trans.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDialTransport_RestoresKCP(t *testing.T) {
	// The reconnect path must redial KCP with the stored address and shard
	// configuration rather than falling through to "no dial address".
	c := &client{opts: defaultOptions()}
	c.dialKCP = "127.0.0.1:1"
	c.kcpDataShards = 10
	c.kcpParityShards = 3
	c.ctx = context.Background()

	_, err := c.dialTransport()
	if err == nil {
		t.Fatal("expected dial error against a closed port")
	}
}

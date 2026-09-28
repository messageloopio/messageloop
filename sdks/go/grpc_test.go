package messageloopgo

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
)

// TestGRPCTLSConfig_ClonesUserConfig pins the gRPC clone contract, mirroring
// TestQUICTLSConfig_ClonesUserConfig: ServerName is carried into the clone and
// InsecureSkipVerify is applied on the clone, never on the caller's
// configuration.
func TestGRPCTLSConfig_ClonesUserConfig(t *testing.T) {
	orig := &tls.Config{ServerName: "example.test"}
	opts := &Options{TLSConfig: orig, InsecureSkipVerify: true}
	cfg := grpcTLSConfig(opts)
	if cfg.ServerName != "example.test" {
		t.Fatalf("ServerName = %q, want %q", cfg.ServerName, "example.test")
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify to be applied on the clone")
	}
	if orig.InsecureSkipVerify {
		t.Fatal("original TLSConfig must not be mutated")
	}
}

// TestGRPCTransportCredentials_SecurityProtocol pins the single enable
// condition: without a TLS configuration the dial keeps the plaintext
// insecure credentials (local path zero regression); a TLS configuration
// (WithTLS or WithTLSConfig) switches it to TLS. InsecureSkipVerify alone is
// a modifier and must not flip the default.
func TestGRPCTransportCredentials_SecurityProtocol(t *testing.T) {
	cases := []struct {
		name string
		opts *Options
		want string
	}{
		{name: "nil options", opts: nil, want: "insecure"},
		{name: "no TLS config", opts: &Options{}, want: "insecure"},
		{name: "insecure skip verify alone", opts: &Options{InsecureSkipVerify: true}, want: "insecure"},
		{name: "TLS config", opts: &Options{TLSConfig: &tls.Config{}}, want: "tls"},
		{name: "TLS config with skip verify", opts: &Options{TLSConfig: &tls.Config{}, InsecureSkipVerify: true}, want: "tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grpcTransportCredentials(tc.opts).Info().SecurityProtocol
			if got != tc.want {
				t.Fatalf("SecurityProtocol = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewGRPCTransport_RequiresCredentials pins the fail-closed contract:
// the constructor takes the credentials from its caller, and a call without
// them fails at dial-option validation instead of silently connecting in
// plaintext (which is how a future dial path could otherwise regress the TLS
// default).
func TestNewGRPCTransport_RequiresCredentials(t *testing.T) {
	_, err := newGRPCTransport(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Fatal("newGRPCTransport without credentials succeeded, want a fail-closed error")
	}
	if !strings.Contains(err.Error(), "no transport security") {
		t.Fatalf("error = %v, want the gRPC no-transport-security failure", err)
	}
}

// TestWithTLS_EnablesTLS verifies the system-roots convenience option sets the
// empty TLS configuration that enables TLS on DialGRPC.
func TestWithTLS_EnablesTLS(t *testing.T) {
	opts := defaultOptions()
	WithTLS()(opts)
	if opts.TLSConfig == nil {
		t.Fatal("WithTLS must set a non-nil TLSConfig to enable TLS")
	}
}

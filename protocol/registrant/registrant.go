// Package registrant provides a minimal HTTP client for registering a proxy
// backend with the MessageLoop registry.
//
// A MessageLoop server with registry enabled exposes the service directory
// on its admin HTTP listener (server.http.addr) under /registry/v1/ and
// stores the directory in Redis with a 30s TTL. A backend registers once,
// keeps its entry alive with heartbeats and deregisters on shutdown:
//
//	reg, err := registrant.Register(ctx, registrant.Options{
//		ServerURL: "http://messageloop-admin:8080",
//		Service:   "acme-battle",          // must equal proxy[].service
//		ID:        os.Getenv("POD_NAME"),  // stable and unique per process
//		Address:   "10.0.0.7:9001",        // the gRPC address MessageLoop dials
//		Token:     adminToken,             // server.http.auth_token, when set
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer reg.Close()
//
// The package is stdlib-only on purpose: backend services take a dependency
// on the protocol module without pulling in the server's storage stack, and
// non-Go backends can speak the same four HTTP calls by hand.
package registrant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultHeartbeatInterval is the heartbeat period used when
// Options.HeartbeatInterval is unset. The server-side entry TTL is 30s, so
// a custom interval must stay well below that or the entry expires between
// beats.
const DefaultHeartbeatInterval = 10 * time.Second

// heartbeatFailLimit is the number of consecutive heartbeat failures after
// which Healthy reports the last error instead of nil (mirroring the lynx
// registrar's readiness semantics).
const heartbeatFailLimit = 3

// closeWaitTimeout bounds how long Close waits for the heartbeat loop to
// exit before sending the deregister request — waiting prevents an in-flight
// re-register from recreating the entry after the delete.
const closeWaitTimeout = 6 * time.Second

// deregisterTimeout bounds the deregister request Close sends.
const deregisterTimeout = 3 * time.Second

var (
	// ErrNotFound is reported by the heartbeat path when the server says
	// the instance entry is gone (TTL expired). Unless
	// Options.DisableReregister is set, the loop answers it by
	// re-registering under the same ID.
	ErrNotFound = errors.New("registrant: instance not found in registry")

	// ErrClosed is reported by Healthy after Close.
	ErrClosed = errors.New("registrant: closed")
)

// Options configures Register. ServerURL, Service, ID and Address are
// required; everything else has a usable zero value.
type Options struct {
	// ServerURL is the MessageLoop admin base URL, for example
	// "http://messageloop-admin:8080". Scheme must be http or https.
	ServerURL string

	// Service is the registry service name. It must equal the server's
	// proxy[].service configuration exactly. A single DNS label of 1-63
	// characters is the recommended convention, not enforced.
	Service string

	// ID identifies this process within the service. It must be stable for
	// the process lifetime and unique in the cluster (pod name, container
	// ID, or a UUID generated at startup): it feeds the server-side
	// consistent-hash placement, so changing it mid-run hands every routing
	// key this instance owns to another instance.
	ID string

	// Address is the gRPC endpoint MessageLoop dials for this instance:
	// host:port with a non-empty host. A bare ":port" is rejected.
	Address string

	// Version, Tags and Meta are free-form instance attributes surfaced in
	// the service listing; routing ignores them today.
	Version string
	Tags    []string
	Meta    map[string]string

	// Token is the admin listener bearer token (server.http.auth_token).
	// Empty means the listener requires none.
	Token string

	// HeartbeatInterval is how often the entry is renewed. Zero selects
	// DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration

	// DisableReregister turns off the automatic re-register when a
	// heartbeat finds the entry expired (404). With it set, a gone entry
	// stays gone and Healthy keeps reporting ErrNotFound until Close.
	DisableReregister bool

	// HTTPClient replaces the default client (5s per-request timeout).
	HTTPClient *http.Client

	// Logger receives lifecycle lines (registered, re-registered,
	// heartbeat failing). nil selects slog.Default().
	Logger *slog.Logger
}

// Registration is a live registry entry. Create one with Register and end
// it with Close. Methods are safe for concurrent use.
type Registration struct {
	opts   Options
	client *http.Client
	base   string
	logger *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	failures int
	lastErr  error
	closed   bool

	closeOnce sync.Once
	closeErr  error
}

// Register validates the options, registers the instance synchronously and
// starts the heartbeat loop. It returns fail-fast: a failed initial
// registration is an error, not a background retry — the caller decides the
// process fate.
func Register(ctx context.Context, opts Options) (*Registration, error) {
	base, err := normalizeServerURL(opts.ServerURL)
	if err != nil {
		return nil, err
	}
	if opts.Service == "" {
		return nil, errors.New("registrant: Service is required")
	}
	if opts.ID == "" {
		return nil, errors.New("registrant: ID is required")
	}
	if err := validateAddress(opts.Address); err != nil {
		return nil, err
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	reg := &Registration{
		opts:   opts,
		client: client,
		base:   base,
		logger: logger,
	}
	if err := reg.registerOnce(ctx); err != nil {
		return nil, err
	}
	reg.logger.Info("registrant: registered",
		"service", opts.Service, "id", opts.ID, "address", opts.Address)
	reg.startLoop()
	return reg, nil
}

// Healthy returns nil while heartbeats keep succeeding (transient failures
// below heartbeatFailLimit are tolerated), the last heartbeat error once
// heartbeatFailLimit consecutive beats failed, and ErrClosed after Close.
func (r *Registration) Healthy() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.failures >= heartbeatFailLimit {
		return fmt.Errorf("registrant: heartbeat failing: %w", r.lastErr)
	}
	return nil
}

// Close stops the heartbeat loop and deregisters the instance. It is
// idempotent: the first call returns the deregister result, later calls
// return nil.
func (r *Registration) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(closeWaitTimeout):
			r.logger.Warn("registrant: heartbeat loop did not stop in time, deregistering anyway")
		}
		ctx, cancel := context.WithTimeout(context.Background(), deregisterTimeout)
		defer cancel()
		err := r.deregisterOnce(ctx)
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.closeErr = err
	})
	return r.closeErr
}

// registerRequest is the wire shape of POST .../instances. The server side
// (internal/registryhttp) mirrors it; the closed-loop test there pins both
// sides to the same JSON.
type registerRequest struct {
	ID      string            `json:"id"`
	Address string            `json:"address"`
	Version string            `json:"version,omitempty"`
	Tags    []string          `json:"tags,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
}

func (r *Registration) registerOnce(ctx context.Context) error {
	body, err := json.Marshal(registerRequest{
		ID:      r.opts.ID,
		Address: r.opts.Address,
		Version: r.opts.Version,
		Tags:    r.opts.Tags,
		Meta:    r.opts.Meta,
	})
	if err != nil {
		return fmt.Errorf("registrant: encode register body: %w", err)
	}
	req, err := r.newRequest(ctx, http.MethodPost, r.servicePath("instances"), body)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("registrant: register: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("registrant: register: unexpected status %s: %s", resp.Status, errorSnippet(resp.Body))
	}
	return nil
}

func (r *Registration) heartbeatOnce(ctx context.Context) error {
	req, err := r.newRequest(ctx, http.MethodPut, r.instancePath("heartbeat"), nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("registrant: heartbeat: %w", err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("registrant: heartbeat: unexpected status %s: %s", resp.Status, errorSnippet(resp.Body))
	}
}

func (r *Registration) deregisterOnce(ctx context.Context) error {
	req, err := r.newRequest(ctx, http.MethodDelete, r.instancePath(""), nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("registrant: deregister: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("registrant: deregister: unexpected status %s: %s", resp.Status, errorSnippet(resp.Body))
	}
	return nil
}

// startLoop launches the heartbeat goroutine. The context it derives is
// cancelled by Close.
func (r *Registration) startLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.opts.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			err := r.heartbeatOnce(ctx)
			if errors.Is(err, ErrNotFound) && !r.opts.DisableReregister {
				// The entry expired (this process was paused or the
				// server lost it). Re-registering under the same ID is
				// safe — the server upserts — and keeping the ID stable
				// avoids reshuffling hash ownership.
				r.logger.Warn("registrant: entry expired, re-registering",
					"service", r.opts.Service, "id", r.opts.ID)
				err = r.registerOnce(ctx)
			}
			r.record(err)
		}
	}()
}

// record folds one heartbeat outcome into the health state.
func (r *Registration) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		if r.failures >= heartbeatFailLimit {
			r.logger.Info("registrant: heartbeat recovered",
				"service", r.opts.Service, "id", r.opts.ID)
		}
		r.failures = 0
		r.lastErr = nil
		return
	}
	r.failures++
	r.lastErr = err
	if r.failures == heartbeatFailLimit {
		r.logger.Error("registrant: heartbeat failing",
			"service", r.opts.Service, "id", r.opts.ID, "error", err)
	}
}

func (r *Registration) servicePath(suffix string) string {
	p := "/registry/v1/services/" + url.PathEscape(r.opts.Service)
	if suffix != "" {
		p += "/" + suffix
	}
	return p
}

// instancePath addresses this registration's own instance; suffix is
// "heartbeat" or empty for the instance itself.
func (r *Registration) instancePath(suffix string) string {
	p := r.servicePath("instances") + "/" + url.PathEscape(r.opts.ID)
	if suffix != "" {
		p += "/" + suffix
	}
	return p
}

func (r *Registration) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("registrant: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.opts.Token)
	}
	return req, nil
}

func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return "", errors.New("registrant: ServerURL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("registrant: invalid ServerURL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("registrant: ServerURL must be an http(s) URL, got %q", raw)
	}
	return raw, nil
}

func validateAddress(addr string) error {
	if addr == "" {
		return errors.New("registrant: Address is required")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("registrant: Address must be host:port: %w", err)
	}
	if host == "" {
		return fmt.Errorf("registrant: Address must include a host, got %q", addr)
	}
	return nil
}

// drain closes the response body, draining a bounded amount first so the
// connection is reusable.
func drain(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
}

// errorSnippet renders up to 256 bytes of an error response body for error
// messages.
func errorSnippet(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

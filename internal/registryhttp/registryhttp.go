// Package registryhttp serves the service-directory HTTP API on the admin
// listener under /registry/v1/. It is the registrant-facing half of the
// registry: proxy backends (Go ones via protocol/registrant, everything
// else via the same four calls) register, heartbeat, deregister and list
// here, and every write goes through the same Registry implementation whose
// directory the Resolver consumes — so a register or deregister is visible
// to routing immediately via the backend's Pub/Sub events.
//
// The handler is mounted only when registry.enabled is true. It inherits
// the admin listener's guard: with server.http.auth_token set every call
// needs the Bearer token, and a non-loopback admin bind without a token is
// rejected at config validation (G5), so the mutation endpoints are never
// silently exposed.
package registryhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/lynx-go/lynx/contrib/registry"
)

// maxBodyBytes bounds a register request body. Instances carry small
// attributes; 64KB is generous.
const maxBodyBytes = 64 << 10

// New returns a handler serving the /registry/v1 API. reg and disc are
// normally the same backend (the registry-redis client implements both);
// they are two parameters so tests can fake the read and write sides
// separately.
func New(reg registry.Registry, disc registry.Discovery) http.Handler {
	h := &handler{reg: reg, disc: disc}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /registry/v1/services/{service}/instances", h.register)
	mux.HandleFunc("PUT /registry/v1/services/{service}/instances/{id}/heartbeat", h.heartbeat)
	mux.HandleFunc("DELETE /registry/v1/services/{service}/instances/{id}", h.deregister)
	mux.HandleFunc("GET /registry/v1/services/{service}", h.list)
	return mux
}

type handler struct {
	reg  registry.Registry
	disc registry.Discovery
}

// registerRequest is the wire shape of POST .../instances. protocol/registrant
// mirrors it; the closed-loop test in this package pins both sides to the
// same JSON.
type registerRequest struct {
	ID      string            `json:"id"`
	Address string            `json:"address"`
	Version string            `json:"version,omitempty"`
	Tags    []string          `json:"tags,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// instanceView is one entry of the service listing.
type instanceView struct {
	ID      string            `json:"id"`
	Address string            `json:"address"`
	Version string            `json:"version,omitempty"`
	Tags    []string          `json:"tags,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := validateAddress(req.Address); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inst := registry.Instance{
		Name:      r.PathValue("service"),
		ID:        req.ID,
		Version:   req.Version,
		Status:    registry.StatusPassing,
		Endpoints: []registry.Endpoint{{Protocol: registry.ProtocolGRPC, Address: req.Address}},
		Tags:      req.Tags,
		Meta:      req.Meta,
	}
	if err := h.reg.Register(r.Context(), inst); err != nil {
		writeRegistryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	err := h.reg.Heartbeat(r.Context(), r.PathValue("service"), r.PathValue("id"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, registry.ErrNotRegistered):
		// The entry expired between heartbeats; registrants re-register
		// on this signal.
		writeError(w, http.StatusNotFound, "instance not registered")
	default:
		writeRegistryError(w, err)
	}
}

func (h *handler) deregister(w http.ResponseWriter, r *http.Request) {
	if err := h.reg.Deregister(r.Context(), r.PathValue("service"), r.PathValue("id")); err != nil {
		writeRegistryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	instances, err := h.disc.GetService(r.Context(), r.PathValue("service"), registry.Filter{Protocol: registry.ProtocolGRPC})
	if err != nil && !errors.Is(err, registry.ErrNoInstance) {
		writeRegistryError(w, err)
		return
	}
	views := make([]instanceView, 0, len(instances))
	for _, inst := range instances {
		address := ""
		if ep, err := registry.EndpointOf(inst, registry.ProtocolGRPC); err == nil {
			address = ep.Address
		}
		views = append(views, instanceView{
			ID:      inst.ID,
			Address: address,
			Version: inst.Version,
			Tags:    inst.Tags,
			Meta:    inst.Meta,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]instanceView{"instances": views})
}

// writeRegistryError maps backend errors: a bad name is the caller's fault,
// everything else is a directory failure.
func writeRegistryError(w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrBadName) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// validateAddress enforces the directory's endpoint contract: dialable
// host:port, never a bare ":port".
func validateAddress(addr string) error {
	if addr == "" {
		return errors.New("address is required")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address must be host:port: %w", err)
	}
	if host == "" {
		return fmt.Errorf("address must include a host, got %q", addr)
	}
	return nil
}

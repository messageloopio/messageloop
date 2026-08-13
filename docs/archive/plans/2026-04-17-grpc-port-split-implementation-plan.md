# gRPC Port Split Implementation Plan

## Status

This document is the implementation plan for separating client gRPC streaming traffic from admin gRPC API traffic.

It is based on the approved design in:

- `docs/superpowers/specs/2026-04-17-grpc-port-split-design.md`

This is a forward-looking execution plan, not a statement of current runtime behavior.

## Inputs

- Design spec: `docs/superpowers/specs/2026-04-17-grpc-port-split-design.md`
- Current bootstrap wiring: `cmd/server/main.go`
- Current combined gRPC server: `pkg/grpcstream/server.go`
- Current config surface: `config/config.go` and `config-example.yaml`
- Constraint: backward compatibility with the old single-port gRPC shape is not required.

## Strategy

Implement the split in three layers, in order:

1. Bootstrap seam and gRPC component split
2. Configuration and gRPC preflight contract
3. Runtime cutover, documentation, and regression coverage

The critical implementation rule is that client/admin gRPC preflight binding must happen before `node.Run(...)`, so bind-time failures cannot happen after `Node` side effects have started.

## Phase 1: Bootstrap Seam And gRPC Component Split

### Objective

Create the structural seams needed to test and implement the split safely before changing startup behavior.

### Tasks

1. Extract a testable bootstrap path from `cmd/server/main.go` so startup ordering can be asserted without relying only on black-box `main` execution.
2. Replace the combined gRPC server shape with explicit client/admin server component types.
3. Introduce a prepared-listener ownership model that can accept pre-bound listeners during startup.
4. Move RawCodec registration behind a process-global one-time guard.
5. Keep handler-layer responsibilities unchanged while separating component wiring.

### Likely Files

- `cmd/server/main.go`
- `pkg/grpcstream/server.go`
- `pkg/grpcstream/client_server.go`
- `pkg/grpcstream/admin_server.go`
- `pkg/grpcstream/server_common.go`
- `pkg/grpcstream/handler.go`
- `pkg/grpcstream/api_handler.go`
- `node.go` only if startup test seams require it

### Deliverables

- Startup is no longer trapped entirely inside the `main` closure.
- Two independent gRPC server component types exist.
- Pre-bound listener handoff is possible without constructor-time binding.

### Tests

1. Service registration tests for client/admin server types.
2. Bootstrap seam tests that can observe whether `node.Run(...)` was called.
3. RawCodec one-time registration regression coverage where practical.

## Phase 2: Config Contract And gRPC Preflight

### Objective

Introduce the new config contract and make gRPC preflight binding happen before `node.Run(...)`.

### Tasks

1. Add `server.grpc_admin` config fields.
2. Keep `transport.grpc` as the client-streaming surface only.
3. Add validation for:
   - missing client/admin gRPC addresses
   - partial TLS configuration
   - cross-surface TLS non-inheritance assumptions
4. Add an explicit gRPC preflight helper that:
   - resolves config for both gRPC surfaces
   - loads and validates TLS credentials for each surface before `node.Run(...)`
   - attempts real listener binding for both
   - closes any already-bound listener if a later preflight or later bootstrap step fails
5. Reorder bootstrap so gRPC preflight completes before `node.Run(...)`.

### Likely Files

- `config/config.go`
- `config-example.yaml`
- `configs/test.yaml`
- `cmd/server/main.go`
- new bootstrap or helper code under `pkg/grpcstream/` or the root package

### Deliverables

- New config shape exists and is validated.
- gRPC bind failure happens before `Node` startup side effects.
- No leaked pre-bound listeners on aborted startup.

### Tests

1. Config parsing and validation tests.
2. Partial TLS config rejection tests.
3. Bad TLS cert/key path tests proving credential-load failures happen during preflight.
4. Mixed TLS-mode tests proving one gRPC surface does not inherit TLS settings from the other.
5. Preflight cleanup tests.
6. Bootstrap-level test proving gRPC bind failure prevents `node.Run(...)` side effects.

## Phase 3: Main Wiring And Runtime Cutover

### Objective

Cut over the application bootstrap so successful startup serves on two gRPC ports with the correct service boundaries.

### Tasks

1. Wire both gRPC components into application startup.
2. Ensure successful preflight is followed by `node.Run(...)`, then component registration/start.
3. Ensure any startup abort after preflight still closes both pre-bound gRPC listeners.
4. Keep WebSocket and admin HTTP behavior unchanged except for config/documentation references.
5. Update logs to distinguish `grpc-client-server` and `grpc-admin-server`.
6. Add a positive client-streaming gRPC integration path proving bidi MessageLoop traffic still works on the client port.

### Likely Files

- `cmd/server/main.go`
- `pkg/grpcstream/client_server.go`
- `pkg/grpcstream/admin_server.go`
- `pkg/grpcstream/integration_test.go`
- `node.go` only if startup test hooks are required

### Deliverables

- Client gRPC port serves only streaming traffic.
- Admin gRPC port serves only admin RPCs.
- Startup ordering matches the design contract.
- Existing client-side gRPC streaming behavior still works.

### Tests

1. Real bootstrap integration test proving successful dual-port startup.
2. Positive gRPC streaming test proving MessageLoop bidi traffic succeeds on the client port.
3. Positive admin-port bootstrap test proving `APIService` calls succeed on the admin port and affect the shared `Node` runtime.
4. Wrong-port negative tests:
   - Admin API on client port returns unimplemented.
   - Streaming RPC on admin port returns unimplemented.
5. Collision test using a deterministic, platform-safe bind-equivalent address strategy, or mark it platform-conditional when socket rules differ by OS.

## Phase 4: Documentation And Operator Cutover

### Objective

Update all operator-facing and contributor-facing docs to reflect the split-port runtime model.

### Tasks

1. Update `README.md` endpoint and Admin API sections.
2. Update `config-example.yaml` to show separate ports.
3. Update `configs/test.yaml` to the new required config shape.
4. Optionally update local `config.yaml` for manual verification only; do not treat it as a versioned merge-gating deliverable.
5. Update internal contributor docs that mention gRPC listener behavior.
6. Call out that `server.grpc_admin.addr` and `server.http.addr` are private operational surfaces by default.
7. Remove any remaining wording that suggests Admin API shares the client gRPC port.
8. Update example ports where needed so the new admin gRPC default does not collide with existing proxy or timeout examples.

### Likely Files

- `README.md`
- `config-example.yaml`
- `configs/test.yaml`
- `RPC_TIMEOUT.md`
- `AGENTS.md`
- `CLAUDE.md`
- `.github/copilot-instructions.md`

### Deliverables

- Docs match the new config contract and endpoint model.
- Operators understand the breaking change and new default exposure guidance.

### Tests

1. Repository-wide grep for stale shared-port terminology.
2. Repository-wide example-port sweep so the new admin gRPC default does not conflict with proxy or timeout examples.
3. Basic doc/config sanity pass.

## Phase 5: Hardening And Regression Sweep

### Objective

Stabilize the split-port change with focused failure-path and migration regression coverage.

### Tasks

1. Add failure tests for aborted startup after preflight.
2. Confirm no pre-bound listener leaks on all early-return paths.
3. Add mixed-mode TLS and non-inheritance regression tests.
4. Run full Go test suite.
5. Review logs and metrics naming for the new gRPC surfaces.

### Likely Files

- `pkg/grpcstream/integration_test.go`
- `cmd/server/main.go`
- any new bootstrap test files

### Deliverables

- Startup and rollback behavior is defensible.
- Migration risks are covered by regression tests.

### Tests

1. Full `go test ./...`
2. Focused startup failure-path tests.
3. Real dual-port bootstrap regression tests.
4. TLS independence regression tests.

## Critical Path

1. Freeze the new config contract.
2. Create the bootstrap seam and split the combined gRPC server into client/admin components.
3. Implement gRPC preflight binding before `node.Run(...)`.
4. Prove both success-path and failure-path bootstrap behavior with real startup tests.
5. Update configs and docs to reflect the new port model.

## Dependency Ordering

- Phase 1 must land first, because the split components and bootstrap seam are prerequisites for safe preflight wiring.
- Phase 2 depends on Phase 1, because preflight needs the new component ownership model.
- Phase 3 depends on Phases 1 and 2.
- Phase 4 can begin after the config contract is stable, but should be finalized only once Phase 3 is complete.
- Phase 5 runs after all earlier phases and should gate merge readiness.
# Distributed Deployment Implementation Plan

## Status

This document is a historical implementation plan captured on 2026-04-17.

It describes the intended rollout order at planning time, not a live checklist of what is still missing today.

For current behavior and configuration, use:

- `README.md`
- `config-example.yaml`
- `CLAUDE.md`
- current code and tests in the repository

## Inputs

- Design baseline: `docs/superpowers/specs/2026-04-17-distributed-deployment-design.md`
- Goal at planning time: implement true distributed deployment support for MessageLoop.
- Constraint: backward compatibility is not required.

## Strategy

Keep `Hub` as a local in-memory runtime structure and add distributed control-plane capabilities around `Node` through explicit ports and Redis-backed adapters.

This minimizes architecture churn while replacing the then-current node-local assumptions in session ownership, presence, admin routing, and cluster-wide queries.

## Phase 1: Cluster Skeleton And Wiring

### Objective

Introduce cluster configuration, lifecycle wiring, and control-plane interfaces without changing runtime behavior yet.

### Tasks

1. Add cluster configuration fields.
2. Add `node_id` and generated `incarnation_id` handling.
3. Introduce cluster ports and command/result model types.
4. Wire cluster lifecycle into `Node.Run` and `Node.Shutdown`.
5. Keep cluster disabled by default with in-memory or noop implementations.

### Likely Files

- `config/config.go`
- `config-example.yaml`
- `cmd/server/main.go`
- `node.go`
- new files in root package for cluster ports and types

### Deliverables

- Cluster config parsed and validated.
- `Node` has explicit control-plane dependencies.
- Cluster mode can be enabled without changing data-plane semantics yet.

### Tests

1. Config parsing tests.
2. `Node` startup and shutdown tests.
3. Regression tests with cluster disabled.

## Phase 2: Shared Ownership State And Distributed Presence

### Objective

Make liveness and session ownership durable and cluster-visible. Replace local-only presence assumptions with distributed presence.

### Tasks

1. Implement node lease storage and renewal.
2. Implement session lease storage with compare-and-set ownership guards.
3. Implement session snapshot persistence including `auth_context`, subscriptions, offsets, and broker epoch.
4. Rework Redis presence to support per-member TTL by `(channel, session_id)`.
5. Route connect, disconnect, subscribe, unsubscribe, and ping through shared-state synchronization paths.

### Likely Files

- `node.go`
- `client.go`
- `presence.go`
- `pkg/redisbroker/options.go`
- `pkg/redisbroker/redis.go`
- `pkg/redisbroker/presence_redis.go`
- new Redis-backed cluster adapter files in `pkg/redisbroker/`

### Deliverables

- Live node incarnations tracked in shared storage.
- Session ownership durable and fenced.
- Presence independently expires per member.

### Tests

1. Redis adapter contract tests.
2. Lease version CAS tests.
3. Stale owner write rejection tests.
4. Per-member TTL expiration tests.
5. Crash cleanup via TTL tests.

## Phase 3: Command Bus, Dedupe, And Cluster-Aware Admin

### Objective

Make session-targeted control operations work from any node.

### Tasks

1. Implement Redis-backed command bus using node-incarnation-specific channels.
2. Add command dedupe records with `pending`, `succeeded`, and `failed` states.
3. Add reply channel handling and per-command result aggregation.
4. Refactor Admin API handlers to resolve owner and dispatch locally or remotely.
5. Upgrade server API result models from `map<string,bool>` to structured per-target results.
6. Implement missing cluster-aware Survey RPC using node broadcast and reply aggregation.

### Likely Files

- `pkg/grpcstream/api_handler.go`
- `pkg/grpcstream/server.go`
- `protocol/server/v1/api.proto`
- generated server protobuf files
- `node.go`
- `survey.go`
- new command bus files under `pkg/redisbroker/`

### Deliverables

- Cluster-wide `disconnect`, `subscribe`, `unsubscribe`, and session-targeted publish.
- Idempotent command execution and partial success reporting.
- Cluster-scoped Survey.

### Tests

1. Two-node admin command integration tests.
2. Duplicate command delivery tests.
3. `STALE_OWNER` single retry tests.
4. Batch partial success tests.
5. Survey node-level timeout and partial failure tests.

## Phase 4: Resume, Takeover, And Recovery Semantics

### Objective

Replace local-only session takeover with cluster-safe ownership transfer and recovery.

### Tasks

1. Remove direct dependence on local `hub.LookupSession` for resume.
2. Add ownership state machine handling: `active`, `transferring`, `closed`.
3. Implement takeover command flow and ownership CAS.
4. Recreate local runtime subscriptions from snapshot after ownership is established.
5. Recover message history from stored offsets when broker epoch matches.
6. Surface `recovery_gap` explicitly when epoch or retained history is insufficient.

### Likely Files

- `client.go`
- `node.go`
- `hub.go`
- `protocol/client/v1/service.proto`
- generated client protobuf files
- `sdks/go/client.go`
- `sdks/ts/src/client/client.ts`
- related converter/generated TS proto files

### Deliverables

- Cross-node session resume.
- Ownership transfer guarded by node lease and session lease fencing.
- Explicit recovery-gap behavior.

### Tests

1. Graceful two-node takeover tests.
2. Forced takeover after owner lease expiry tests.
3. Owner timeout without forced takeover tests.
4. Epoch mismatch recovery-gap tests.
5. Old owner stale write rejection tests.

## Phase 5: Shared Query Projections And Read Path Cutover

### Objective

Make cluster-wide query APIs read from shared projections instead of local hub state.

### Tasks

1. Define projection schema for literal subscription keys.
2. Add projection writes on connect, disconnect, subscribe, and unsubscribe.
3. Add incremental updates and periodic full projection republish.
4. Move `GetChannels` to `ClusterQueryStore`.
5. Ensure `GetPresence` automatically uses Redis presence in cluster mode.
6. Keep `Hub` local-only for delivery and repair support.

### Likely Files

- `node.go`
- `hub.go`
- `cmd/server/main.go`
- `pkg/grpcstream/api_handler.go`
- new projection adapter files in `pkg/redisbroker/`

### Deliverables

- Cluster-wide `GetChannels`.
- Shared projection repair and TTL cleanup.
- No node-local source of truth for cluster query APIs.

### Tests

1. Projection TTL cleanup tests.
2. Node restart repair tests.
3. Wildcard key preservation tests.
4. Eventual consistency window tests for query APIs.

## Phase 6: Hardening, Integration, And Deployment Readiness

### Objective

Stabilize the distributed path, expand multi-node tests, and document the operational model.

### Tasks

1. Expand multi-node Redis integration harness.
2. Add failure tests for TTL, fencing, dedupe, command timeout, and repair lag.
3. Add structured metrics and logs for command timeout, stale owner, dedupe hit, recovery gap, and projection repair.
4. Document cluster config, Redis namespace strategy, and non-mixed rollout expectations.

### Likely Files

- `pkg/grpcstream/integration_test.go`
- `pkg/websocket/integration_test.go`
- `pkg/redisbroker/*.go`
- `README.md`
- `config-example.yaml`

### Deliverables

- Multi-node test coverage.
- Operational visibility for distributed failure modes.
- Deployment documentation.

### Tests

1. Two-node and three-node end-to-end suites.
2. Lease expiry race tests.
3. Command timeout and unknown-final-state tests.
4. Projection repair lag tests.

## Critical Path

1. Freeze cluster config and port model.
2. Implement node lease, session lease, and snapshot CAS.
3. Implement command bus and dedupe.
4. Make Admin session-targeted APIs cluster-aware.
5. Implement resume/takeover with fencing.

`GetChannels` projection cutover can proceed in parallel once projection schema and write path are available.

## Dependency Ordering

1. Phase 1 must complete before any Redis-backed control-plane work.
2. Phase 2 must complete before cluster-aware Admin or resume.
3. Phase 3 must complete before replacing local-only admin behavior.
4. Phase 4 depends on ownership fencing and command bus.
5. Phase 5 depends on projection schema plus stable mutation events.
6. Phase 6 spans all phases but should intensify after Phase 3 and Phase 4 land.

## Major Risks

### Split-Brain And Stale Writes

- Risk: old owners continue writing after takeover.
- Mitigation: route all ownership-sensitive writes through Redis CAS or Lua-guarded adapter logic.

### Ambiguous Command Results

- Risk: owner crashes after side effect but before terminal dedupe result write.
- Mitigation: embrace at-most-once semantics with explicit `COMMAND_IN_PROGRESS` and `UNKNOWN_FINAL_STATE` outcomes.

### Presence And Projection Write Amplification

- Risk: high-cardinality channels cause Redis pressure.
- Mitigation: per-member TTL for presence, literal-key-only projections, incremental plus periodic repair.

### TTL Tuning Sensitivity

- Risk: aggressive lease TTL causes false takeover under transient network issues.
- Mitigation: set node and session lease TTLs to at least 3x heartbeat cadence and validate with failure tests.

### Test Fragility

- Risk: race-heavy multi-node tests become flaky.
- Mitigation: build a reusable cluster harness and avoid ad-hoc sleeps in individual tests.

## Rollout Guidance

1. Do not mix old and new distributed semantics in one cluster.
2. Use a fresh Redis namespace or DB for the new control plane.
3. Validate on a two-node environment before moving to larger clusters.
4. Enable cluster mode only after command bus, dedupe, and fencing are in place.
5. Treat recovery-gap observability as a release gate, not a follow-up item.

## Suggested First Milestone

The shortest path to an initial usable distributed build is:

1. Phase 1
2. Phase 2
3. Phase 3
4. Phase 4

That sequence delivers cluster-safe ownership, distributed presence, cluster-aware session control, and cross-node resume, which together form the minimum viable distributed deployment capability.
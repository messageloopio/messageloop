# Distributed Deployment Design

## Status

This document is a historical design baseline written on 2026-04-17 before the cluster work was implemented end-to-end.

Use the following as the current source of truth for runtime behavior and operator-facing configuration:

- `README.md`
- `config-example.yaml`
- `CLAUDE.md`
- current code under the root package and `pkg/redisbroker/`

## Historical Context

At design time, MessageLoop already supported distributed message fan-out through the Redis broker, but distributed deployment was not end-to-end complete.

Gaps at design time:

- Presence is still node-local by default.
- Admin operations are node-local even when message delivery is distributed.
- Session resume depends on local in-memory session ownership.
- Global queries such as active channels and presence are not backed by a shared cluster view.

This design proposed how MessageLoop could support true multi-node deployment across both the data plane and control plane.

## Goals

- Support horizontal deployment of stateless MessageLoop nodes.
- Make session-targeted control operations work from any node in the cluster.
- Make presence and channel queries cluster-wide rather than node-local.
- Support session recovery and resume across nodes.
- Keep the implementation extensible through interfaces, with Redis as the default backend.

## Non-Goals

- Backward compatibility is not required.
- Extracting a separate control-plane service is not required in this phase.
- Strong consistency for read-only cluster queries is not required.

## Selected Approach

Use unified cluster ports with Redis as the default implementation.

The runtime remains a single MessageLoop server binary, but internally it is split into:

- Data plane: client connections, subscriptions, publish, RPC, local delivery.
- Control plane: session ownership, resumable session state, cluster commands, global query indexes.

This approach provides the fastest path to distributed deployment while preserving clear boundaries for future backends such as etcd, NATS, or Kafka.

## Architecture

### Cluster Ports

Add the following abstractions:

- `SessionDirectory`
  - Maps `session_id -> owner (node_id, incarnation_id)`.
  - Stores lease metadata with TTL.
  - Stores resumable session snapshot.
- `ClusterCommandBus`
  - Delivers cluster commands to a target node incarnation or set of node incarnations.
  - Supports idempotent command execution.
- `ClusterQueryStore`
  - Stores cluster-visible query projections such as active channels, subscriber counts, node liveness, and optional session summaries.
- `PresenceStore`
  - Remains a dedicated port.
  - Redis becomes the default implementation in distributed mode.
  - Must support per-member TTL rather than only per-channel TTL.

### Node Responsibilities

Each node:

- Owns only its local in-memory connection objects.
- Periodically renews its node lease.
- Subscribes to an incarnation-specific command channel.
- Writes session lease, session snapshot, and query projections into shared storage.

### Configuration

Add a cluster section:

```yaml
cluster:
  enabled: true
  node_id: node-a
  backend: redis
```

Each process start also generates an internal `incarnation_id`.

- `node_id` identifies the logical node identity configured by operators.
- `incarnation_id` identifies the current process lifetime.

All distributed ownership and command routing must use the pair `(node_id, incarnation_id)` rather than `node_id` alone.

When `cluster.enabled=false`, MessageLoop may still run in single-node mode with in-memory implementations.

## Runtime Data Model

### Node Lease

Contains:

- `node_id`
- `incarnation_id`
- `started_at`
- `expires_at`

`Node Lease` is the source of truth for process liveness.

### Session Lease

Contains:

- `session_id`
- `node_id`
- `incarnation_id`
- `user_id`
- `client_id`
- `lease_version`
- `expires_at`

`Session Lease` is the source of truth for which live node currently owns a session.

### Session Snapshot

Contains resumable session state:

- `session_id`
- `user_id`
- `client_id`
- `authenticated`
- `subscriptions`
- `channel_offsets`
- `broker_epoch`
- `auth_context`
- `updated_at`

`Session Snapshot` is recovery data only. It is never the source of truth for ownership.

### Query Projections

Contains cluster-visible summaries:

- Active channels and subscriber counts.
- Optional node-level connection counts.

Presence membership itself remains the responsibility of `PresenceStore`. `ClusterQueryStore` may store presence-derived counters, but it must not become a second source of truth for per-member presence.

Projection records must also carry enough metadata for repair and deduplication:

- `owner node_id`
- `owner incarnation_id`
- `projection_version` or monotonic sequence
- `expires_at`

### Ownership Priority

Ownership decisions must follow this order:

1. `Node Lease` decides whether a process incarnation is alive.
2. `Session Lease` decides which live incarnation owns the session.
3. `Session Snapshot` is only used to reconstruct state after ownership is established.

If a `Session Lease` points to an incarnation whose `Node Lease` is expired, that ownership is stale even if the session record itself has not yet expired.

### Local Ownership State Machine

Each owner node manages local session runtime state with these states:

- `active`
  - Accepts client traffic and owner-targeted admin mutations.
  - Renews the session lease.
- `transferring`
  - Entered only after a valid takeover command matches the current ownership tuple.
  - Rejects new mutating client and admin operations.
  - Flushes final snapshot, stops lease renewal, and closes the current transport.
- `closed`
  - Local runtime state has been released.
  - Any later write attempt from this incarnation must fail as stale.

Transition rules:

- `active -> transferring` when takeover is accepted.
- `transferring -> closed` after final snapshot flush and local cleanup.
- There is no `transferring -> active` rollback path.

If a requesting owner loses the compare-and-set race during takeover, it must discard the attempt, reload ownership, and follow the owner-drift rule. The old owner remains `closed`; safety is preferred over restoring the previous connection.

## Data Flow

### Connect

1. Node authenticates the incoming connection.
2. Node creates the local client/session object.
3. Node writes the session lease and initial session snapshot.
4. Node starts renewing the lease.

### Resume / Reconnect

1. Incoming node authenticates the new connection before attempting resume.
2. Incoming node validates that the authenticated principal is compatible with the stored `auth_context`.
3. If authentication fails or the principal does not match the stored `auth_context`, resume is rejected.
4. Incoming node reads the session lease and snapshot.
5. Incoming node reads the owner node lease for the `(node_id, incarnation_id)` referenced by the session lease.
6. If the owner node lease is valid, the new node sends a `takeover` command through the command bus, including the current ownership tuple and the desired next `lease_version`.
7. The old owner validates that it still owns the current tuple, flushes the latest snapshot, stops renewing local ownership, closes the old transport, and acknowledges readiness to transfer.
8. The new owner performs an atomic compare-and-set on the session lease from the old tuple to the new `(node_id, incarnation_id, lease_version+1)` tuple.
9. Only after compare-and-set succeeds may the new owner install local runtime state and begin renewing ownership.
10. If the owner node lease is expired, the new node may perform a forced compare-and-set takeover without waiting for an acknowledgement.
11. If the owner node lease is still valid but the takeover command times out, the request fails with `OWNER_UNREACHABLE`; the system must not force takeover while the previous owner is still considered alive.

After ownership is established, resume recovery proceeds as follows:

1. The new owner recreates local subscriptions from the stored session snapshot.
2. The new owner restores per-subscription offsets and the stored broker epoch.
3. If the stored broker epoch matches the current broker epoch, recovery fetches history from `offset + 1` for each recoverable subscription before normal live delivery resumes.
4. If the broker epoch differs or the required history range is no longer available, that subscription is marked `recovery_gap`.
5. `recovery_gap` must be surfaced explicitly to the caller or session state; the system must not silently pretend that gap-free recovery succeeded.
6. After recovery completes, the new owner resumes normal live fan-out.

### Subscribe / Unsubscribe

For each state change, the owner node updates:

- Local hub state.
- Session snapshot.
- Presence store.
- Cluster query projections.

Write order for mutating subscription state:

1. Apply to local owner state.
2. Persist session snapshot guarded by the current ownership tuple.
3. Update presence store.
4. Update cluster query projection.

If any shared-state write fails, the owner node must retry or return failure to the caller. Silent success with stale shared state is not allowed.

Mutation failure policy:

- Local subscription mutation is provisional until snapshot, presence, and projection writes succeed.
- If shared-state synchronization fails, the owner must roll back the local mutation.
- If rollback cannot be completed safely, the owner must close the local session and force a clean reconnect rather than leaving ambiguous state behind.

### Presence Refresh

Ping/heartbeat refresh updates:

- Local activity timestamp.
- Presence TTL.
- Session lease TTL.

Presence TTL semantics:

- Presence expiration must be tracked per `(channel, session_id)` membership.
- Redis implementations must not rely on a single channel-level TTL for all members.
- A stale member must be removable independently even if other members in the same channel remain active.

### Disconnect

On clean disconnect, the owner node removes local state and deletes or expires:

- Session lease.
- Presence entries.
- Query projection entries.

On crash, TTL expiry performs delayed cleanup automatically.

### Admin Commands

#### Channel-targeted publish

- Continues to go through the distributed broker.

#### Session-targeted commands

Examples:

- `disconnect(session_id)`
- `subscribe(session_id, channel)`
- `unsubscribe(session_id, channel)`
- `publish(session_id, payload)`

Execution flow:

1. Entry node resolves the owner through `SessionDirectory`.
2. Entry node emits a command to the owner node.
3. Owner node executes against the local in-memory session.
4. Owner node updates shared state if required.
5. Entry node aggregates and returns results.

#### Survey

Survey is a cluster-scoped control operation.

Execution flow:

1. Entry node emits a survey command to all live node incarnations.
2. Each target node evaluates the survey against its local subscribers using its local matcher and subscription state.
3. Each target node sends survey requests only to matching local sessions.
4. Each target node collects local survey responses.
5. Each target node publishes a `survey_result` message back to the origin `(node_id, incarnation_id)` using a reply channel that includes `survey_id`, `origin node_id`, and `origin incarnation_id`.
6. The origin node aggregates per-node results until the survey deadline expires.
7. Node-level timeout or delivery failure is recorded as a partial survey failure, not as a silent drop.
8. Entry node returns aggregated per-session results together with node-level timeout/error information when applicable.

Survey fan-out is intentionally node-broadcast rather than projection-routed so that wildcard matching semantics remain local and correct.

### Batch Command Aggregation

For commands targeting multiple sessions:

1. Entry node resolves owner tuples per session.
2. Entry node groups work by owner node incarnation.
3. Entry node dispatches grouped commands in parallel.
4. Entry node returns per-target results, including:
  - `session_id`
  - `owner node_id`
  - `owner incarnation_id`
  - `command_id`
  - `status`
  - `error code`, if any

Batch responses must preserve partial success. A global request must not collapse into one opaque error.

### Owner Drift Handling

If ownership changes after routing but before execution:

- The target node returns `STALE_OWNER` together with the observed current ownership tuple if available.
- The entry node re-resolves ownership once and retries once.
- If ownership changes again during the retry, the command fails with final `STALE_OWNER`.

This makes ownership drift explicit while avoiding unbounded retries.

### Global Queries

- `GetPresence` reads `PresenceStore`.
- `GetChannels` reads `ClusterQueryStore`.
- Node-local hub state must no longer be the source of truth for cluster-wide APIs.

`GetChannels` semantics are defined as follows:

- It returns subscription keys that currently exist in the cluster.
- Exact channels and wildcard patterns are returned exactly as subscribed.
- It does not expand wildcard patterns into concrete runtime channel instances.

If a future API is needed for concrete active channel instances, that must be a separate query contract.

### Projection Key Model

`GetChannels` is backed by projection entries keyed by the literal subscription key.

Rules:

- Exact channels use the subscribed channel string as the key.
- Wildcard subscriptions use the wildcard pattern string as the key.
- The count is the number of live session subscriptions currently referencing that exact key.
- Keys with zero count must be deleted immediately on successful mutation and also expire via TTL as a fallback.

## Command Model

Each cluster command must contain:

- `command_id`
- `command_type`
- `target_node_id`
- `target_incarnation_id`
- `issued_at`
- `lease_version` or fencing token if the command mutates session ownership
- command-specific payload

Command handlers must be idempotent by `command_id`.

Each command backend must also persist a deduplication record for a bounded TTL window containing:

- `command_id`
- terminal execution status
- result payload or error payload

If the same command is redelivered, the owner node must return the stored result instead of executing it again.

Deduplication scope is cluster-global by `command_id`, so command IDs must be globally unique.

Execution order:

1. Owner creates a dedupe record in `pending` state using `SETNX` or equivalent compare-and-set.
2. If the record already exists in terminal `succeeded` or `failed`, the stored result is returned.
3. If the record already exists in `pending`, return `COMMAND_IN_PROGRESS`.
4. After `pending` is established, execute the side effect.
5. Transition the dedupe record to terminal `succeeded` or `failed` with the final result payload.

For externally visible side effects such as session-targeted publish, the platform guarantees at-most-once execution with possible `COMMAND_IN_PROGRESS` or `UNKNOWN_FINAL_STATE` responses after owner crashes. It does not guarantee exactly-once delivery.

## Failure Semantics

### Control Commands

- Unknown session: return `SESSION_NOT_FOUND`.
- Owner lookup succeeded but command delivery/execution timed out: return `OWNER_UNREACHABLE` or `COMMAND_TIMEOUT`.
- Batch commands return per-target result details instead of one opaque error.

### Split-Brain Prevention

Use `lease_version` or fencing tokens for takeover and ownership-sensitive writes.

Rules:

- Only the current lease holder may renew ownership.
- Older lease versions must not overwrite newer state.
- Stale nodes that reconnect after isolation must fail their state update attempts.
- Snapshot and projection writes must be rejected if their ownership tuple is stale.

### Read Consistency

- Query APIs are eventually consistent.
- Command APIs must be explicitly successful or explicitly failed.
- Silent drops are not acceptable.

### Projection Repair

`ClusterQueryStore` is a materialized view, not the ownership source of truth.

Repair rules:

- Each owner node publishes incremental projection updates for local state changes.
- Each owner node also periodically republishes a full projection snapshot for the sessions and channels it currently owns.
- Projection keys expire via TTL to bound stale data after crashes.
- On startup, a node rebuilds projections for any local state it recovered.

This ensures the system can recover from dropped projection updates without manual repair.

## Interface Impact

The following existing behavior must change:

- Admin APIs must become cluster-aware.
- Session resume must stop depending on local hub lookup only.
- Presence must default to a distributed store when cluster mode is enabled.
- Active channel queries must read shared indexes rather than local hub state.

Backward compatibility is intentionally not a constraint, so protocol and internal interfaces may be simplified or reshaped for cluster-first behavior.

## Testing Strategy

### Contract Tests

Define port-level test suites and run them against:

- In-memory implementations.
- Redis implementations.

### Integration Tests

Run at least 2-node and 3-node scenarios covering:

- Cross-node publish delivery.
- Cross-node presence visibility.
- Cross-node session resume.
- Cross-node admin disconnect.
- Cross-node admin subscribe/unsubscribe.
- Cluster-wide channel queries.

### Failure Tests

Cover:

- Owner node crash.
- Lease expiry and cleanup.
- Duplicate command delivery.
- Command timeout.
- Owner takeover race.
- Partial success in batch commands.
- TTL-based cleanup of stale presence and query state.

## Rollout Plan

Suggested implementation order:

1. Introduce cluster ports and single-node noop/in-memory implementations.
2. Add cluster config, stable `node_id`, generated `incarnation_id`, and node lease renewal.
3. Wire Redis presence automatically in cluster mode.
4. Add session lease and session snapshot storage with compare-and-set ownership guards.
5. Add command bus, command deduplication, and owner-node command handling.
6. Implement cluster-aware session resume and takeover using ownership fencing.
7. Introduce projection schema and projection write adapters for shared channel/query visibility.
8. Make Admin APIs cluster-aware for session-targeted commands.
9. Move cluster queries fully to shared projections and implement projection repair.
10. Add distributed integration and failure tests, with failure scenarios introduced as soon as ownership fencing and command deduplication are available rather than only at the end.

## Recommendation

Implement distributed deployment in one codebase with explicit ports/adapters and Redis as the default backend.

This keeps the system practical today while preserving a clean path to a future dedicated control plane or alternate coordination backends.
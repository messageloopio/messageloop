package runtime

import (
	"context"
	"errors"
)

// This file holds the thin export seam for the deterministic fencing
// simulator (internal/cluster/sim, PR-KA-C1). The simulator drives two real
// *Node instances through the production fencing paths, so its scenario tests
// (cluster_sim_test.go, package messageloop_test — an external test package
// because sim itself imports this package) need these unexported entry
// points. Production code must not call them; nothing here changes the
// fencing algorithm, it only re-exposes it.

// SimSyncClusterSessionState exposes the lease/snapshot sync used by AddClient
// and the throttled ping/pong refresh, so simulator scenarios can trigger the
// "B stole the fencing, A must not write back" check directly.
func SimSyncClusterSessionState(n *Node, ctx context.Context, s *Session) error {
	return n.syncClusterSessionState(ctx, s)
}

// SimResumeRemoteSession exposes the cross-node resume (CAS claim + takeover
// command), so simulator scenarios can script a Bind on another node.
func SimResumeRemoteSession(n *Node, ctx context.Context, s *Session, sessionID string) (*ClusterSessionSnapshot, bool, error) {
	return n.resumeRemoteSession(ctx, s, sessionID)
}

// SimMembershipOnce runs one membership beat of the cluster repairer, so
// simulator scenarios can drive OnLeave deterministically instead of waiting
// for the SCAN ticker.
func SimMembershipOnce(repairer ClusterRepairer, ctx context.Context) error {
	r, ok := repairer.(*clusterRepairer)
	if !ok {
		return errors.New("sim membership beat: repairer does not run membership scans")
	}
	return r.membershipOnce(ctx)
}

package messageloop

import (
	"context"
	"sort"

	"github.com/lynx-go/x/log"
)

// ExpandUserSessions resolves userID to the deduplicated, sorted set of
// session IDs that should receive user-targeted admin operations
// (Publish/Disconnect/Subscribe/Unsubscribe). Local hub sessions are trusted
// directly (Client.UserID is authoritative); cluster index entries are only
// accepted when the session lease still carries the requested user — the
// index is a hint, never the source of truth. An empty userID returns nil
// without scanning anything. No full-cluster SCAN is ever performed on an
// index miss: stale index entries converge via the periodic repair.
func (n *Node) ExpandUserSessions(ctx context.Context, userID string) []string {
	if userID == "" {
		return nil
	}

	seen := make(map[string]struct{})
	for _, c := range n.hub.SessionsByUser(userID) {
		if c.UserID() == userID {
			seen[c.SessionID()] = struct{}{}
		}
	}

	if n.ClusterEnabled() {
		directory := n.clusterSessionDirectory()
		ids, err := directory.ListUserSessions(ctx, userID)
		if err != nil {
			log.WarnContext(ctx, "failed to list user sessions from cluster index", err, "user_id", userID)
		}
		for _, sid := range ids {
			lease, err := directory.GetSessionLease(ctx, sid)
			if err != nil || lease == nil || lease.UserID != userID {
				continue
			}
			seen[sid] = struct{}{}
		}
	}

	result := make([]string, 0, len(seen))
	for sid := range seen {
		result = append(result, sid)
	}
	sort.Strings(result)
	return result
}

// ObserveAdminUserFanout records the fan-out size (number of sessions) of one
// user-targeted admin operation (op: publish|disconnect|subscribe|unsubscribe).
func (n *Node) ObserveAdminUserFanout(op string, sessions int) {
	if n.metrics != nil {
		n.metrics.AdminUserFanout.WithLabelValues(op).Observe(float64(sessions))
	}
}

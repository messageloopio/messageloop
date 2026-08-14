package messageloop

import (
	"context"
	"sort"
	"time"

	"github.com/lynx-go/x/log"
)

// SyncUserIndex reconciles the user→sessions index with a session lease
// write (Put / successful CAS / Delete) so every index mutation path is
// covered by one helper:
//
//   - newLease == nil (Delete): remove the old lease's membership.
//   - Put/CAS success: add (or refresh the TTL of) the new lease's
//     membership; when the user changed, remove the old membership first.
//   - Empty UserID: only ever removes — anonymous sessions never enter the
//     index (an empty user ID is not an addressable key).
//
// The index is never authoritative: expansions re-check GetSessionLease.
func SyncUserIndex(ctx context.Context, directory SessionDirectory, oldLease, newLease *ClusterSessionLease, ttl time.Duration) error {
	if directory == nil {
		return nil
	}

	var sessionID string
	if newLease != nil {
		sessionID = newLease.SessionID
	} else if oldLease != nil {
		sessionID = oldLease.SessionID
	}
	if sessionID == "" {
		return nil
	}

	// Delete path: remove the old lease's membership.
	if newLease == nil {
		if oldLease == nil || oldLease.UserID == "" {
			return nil
		}
		return directory.RemoveUserSession(ctx, oldLease.UserID, sessionID)
	}

	// The lease changed user: move the membership before adding the new one.
	if oldLease != nil && oldLease.UserID != "" && oldLease.UserID != newLease.UserID {
		if err := directory.RemoveUserSession(ctx, oldLease.UserID, sessionID); err != nil {
			return err
		}
	}

	// Anonymous sessions never enter the index.
	if newLease.UserID == "" {
		return nil
	}
	return directory.AddUserSession(ctx, newLease.UserID, sessionID, ttl)
}

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

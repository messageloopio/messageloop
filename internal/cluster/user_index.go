package cluster

import (
	"context"
	"time"
)

// SyncUserIndex reconciles the user→sessions index with a session lease
// write (Put / successful CAS / Delete) so every index mutation path is
// covered by one helper:
//
//   - newLease == nil (Delete): remove the old lease's membership.
//   - Put/CAS success: add (or refresh the TTL of) the new lease's
//     membership; when the (namespace, user) identity changed, remove the old
//     membership first.
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
		return directory.RemoveUserSession(ctx, oldLease.Namespace, oldLease.UserID, sessionID)
	}

	// The lease changed identity: move the membership before adding the new one.
	if oldLease != nil && oldLease.UserID != "" &&
		(oldLease.UserID != newLease.UserID || oldLease.Namespace != newLease.Namespace) {
		if err := directory.RemoveUserSession(ctx, oldLease.Namespace, oldLease.UserID, sessionID); err != nil {
			return err
		}
	}

	// Anonymous sessions never enter the index.
	if newLease.UserID == "" {
		return nil
	}
	return directory.AddUserSession(ctx, newLease.Namespace, newLease.UserID, sessionID, ttl)
}

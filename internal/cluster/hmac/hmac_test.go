package hmac

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

func testCommand() *messageloop.ClusterCommand {
	return &messageloop.ClusterCommand{
		CommandID:           "cmd-1",
		Type:                messageloop.ClusterCommandDisconnect,
		TargetNodeID:        "node-b",
		TargetIncarnationID: "inc-b",
		SessionID:           "sess-1",
		Channel:             "news",
		LeaseVersion:        7,
		IssuedAt:            time.Unix(1_700_000_000, 123456789),
		IssuedBy:            "node-a",
		Payload:             []byte("payload-bytes"),
		Metadata:            map[string]string{"k": "v"},
	}
}

func rejectReason(t *testing.T, err error) RejectReason {
	t.Helper()
	require.Error(t, err)
	var verifyErr *VerifyError
	require.True(t, errors.As(err, &verifyErr), "expected a *VerifyError, got %T", err)
	return verifyErr.Reason
}

func TestSignCommand_Deterministic(t *testing.T) {
	first := testCommand()
	second := testCommand()
	require.NoError(t, SignCommand(testKey, first))
	require.NoError(t, SignCommand(testKey, second))
	assert.Equal(t, first.Signature, second.Signature, "same input must sign to the same hex")
	assert.Len(t, first.Signature, 64, "hex(HMAC-SHA256) is 64 chars")
}

func TestSignCommand_NilAndEmptyPayloadDigestIdentical(t *testing.T) {
	withNil := testCommand()
	withNil.Payload = nil
	withEmpty := testCommand()
	withEmpty.Payload = []byte{}
	require.NoError(t, SignCommand(testKey, withNil))
	require.NoError(t, SignCommand(testKey, withEmpty))
	assert.Equal(t, withNil.Signature, withEmpty.Signature,
		"nil and empty payloads must hash to the same digest")
}

func TestVerifyCommand_RoundTrip(t *testing.T) {
	cmd := testCommand()
	now := cmd.IssuedAt.UTC()
	require.NoError(t, SignCommand(testKey, cmd))
	require.NoError(t, VerifyCommand(testKey, cmd, now))
}

func TestVerifyCommand_FieldTamperingFails(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	cases := map[string]func(cmd *messageloop.ClusterCommand){
		"type":         func(cmd *messageloop.ClusterCommand) { cmd.Type = messageloop.ClusterCommandTakeover },
		"session_id":   func(cmd *messageloop.ClusterCommand) { cmd.SessionID = "sess-other" },
		"lease_version": func(cmd *messageloop.ClusterCommand) { cmd.LeaseVersion++ },
		"payload":      func(cmd *messageloop.ClusterCommand) { cmd.Payload = []byte("forged") },
		"command_id":   func(cmd *messageloop.ClusterCommand) { cmd.CommandID = "cmd-other" },
		"issued_at":    func(cmd *messageloop.ClusterCommand) { cmd.IssuedAt = cmd.IssuedAt.Add(time.Second) },
		"target_incarnation": func(cmd *messageloop.ClusterCommand) { cmd.TargetIncarnationID = "inc-other" },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := testCommand()
			require.NoError(t, SignCommand(testKey, cmd))
			tamper(cmd)
			assert.Equal(t, RejectBad, rejectReason(t, VerifyCommand(testKey, cmd, now)))
		})
	}
}

// IssuedBy is audit-only and deliberately outside the canonical bytes:
// rewriting it must NOT invalidate the signature (and conversely it proves
// IssuedBy cannot act as a security boundary).
func TestVerifyCommand_IssuedByNotCovered(t *testing.T) {
	cmd := testCommand()
	require.NoError(t, SignCommand(testKey, cmd))
	cmd.IssuedBy = "forged-node"
	require.NoError(t, VerifyCommand(testKey, cmd, cmd.IssuedAt.UTC()))
}

func TestVerifyCommand_MissingSignature(t *testing.T) {
	cmd := testCommand()
	assert.Equal(t, RejectMissing, rejectReason(t, VerifyCommand(testKey, cmd, cmd.IssuedAt.UTC())))
}

func TestVerifyCommand_BadHexAndWrongKey(t *testing.T) {
	cmd := testCommand()
	require.NoError(t, SignCommand(testKey, cmd))

	cmd.Signature = "zz-not-hex"
	assert.Equal(t, RejectBad, rejectReason(t, VerifyCommand(testKey, cmd, cmd.IssuedAt.UTC())))

	cmd = testCommand()
	require.NoError(t, SignCommand([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), cmd))
	assert.Equal(t, RejectBad, rejectReason(t, VerifyCommand(testKey, cmd, cmd.IssuedAt.UTC())),
		"a MAC made with a different key must not verify")
}

func TestVerifyCommand_ClockSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	for _, tc := range []struct {
		name     string
		issuedAt time.Time
		reason   RejectReason
		ok       bool
	}{
		{"future_31s", now.Add(31 * time.Second), RejectSkew, false},
		{"past_31s", now.Add(-31 * time.Second), RejectSkew, false},
		{"future_29s", now.Add(29 * time.Second), "", true},
		{"past_29s", now.Add(-29 * time.Second), "", true},
		{"zero", time.Time{}, RejectSkew, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := testCommand()
			cmd.IssuedAt = tc.issuedAt
			require.NoError(t, SignCommand(testKey, cmd))
			err := VerifyCommand(testKey, cmd, now)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			assert.Equal(t, tc.reason, rejectReason(t, err))
		})
	}
}

// An empty CommandID is rejected even when the MAC is correctly computed over
// the empty id (the signer is willing, the envelope is still invalid).
func TestVerifyCommand_EmptyCommandIDRejected(t *testing.T) {
	cmd := testCommand()
	cmd.CommandID = ""
	require.NoError(t, SignCommand(testKey, cmd))
	assert.Equal(t, RejectID, rejectReason(t, VerifyCommand(testKey, cmd, cmd.IssuedAt.UTC())))
}

func TestSignAndVerifyResult_RoundTrip(t *testing.T) {
	res := &messageloop.ClusterCommandResult{
		CommandID:     "cmd-1",
		SessionID:     "sess-1",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		Status:        messageloop.ClusterCommandStatusSucceeded,
		ErrorCode:     "",
		IssuedAt:      time.Unix(1_700_000_000, 0).UTC(),
	}
	require.NoError(t, SignResult(testKey, res))
	require.NoError(t, VerifyResult(testKey, res, res.IssuedAt))

	tampered := *res
	tampered.Status = messageloop.ClusterCommandStatusFailed
	assert.Equal(t, RejectBad, rejectReason(t, VerifyResult(testKey, &tampered, res.IssuedAt)))

	unsigned := *res
	unsigned.Signature = ""
	assert.Equal(t, RejectMissing, rejectReason(t, VerifyResult(testKey, &unsigned, res.IssuedAt)))

	stale := *res
	require.NoError(t, SignResult(testKey, &stale))
	assert.Equal(t, RejectSkew, rejectReason(t, VerifyResult(testKey, &stale, res.IssuedAt.Add(31*time.Second))),
		"a result verified 31s after issue must be rejected as skewed")
}

func TestSignCommand_NilCommandFails(t *testing.T) {
	require.Error(t, SignCommand(testKey, nil))
	require.Error(t, SignResult(testKey, nil))
}

func TestCanonicalFormat_StableLines(t *testing.T) {
	// Pin the canonical layout: a future refactor must not silently change
	// the signed bytes of a known command.
	cmd := &messageloop.ClusterCommand{
		CommandID:           "id",
		Type:                messageloop.ClusterCommandPublish,
		TargetIncarnationID: "inc",
		SessionID:           "sess",
		LeaseVersion:        0,
		IssuedAt:            time.Unix(42, 0),
	}
	require.NoError(t, SignCommand(testKey, cmd))
	require.NoError(t, SignCommand(testKey, cmd))
	// LeaseVersion 0 renders as "0" with no leading zeros, and every line —
	// the last included — ends with '\n'.
	canonical := string(canonicalCommand(cmd))
	assert.True(t, strings.HasPrefix(canonical, "v1\npublish\nsess\ninc\n0\n"), canonical)
	assert.True(t, strings.HasSuffix(canonical, "\nid\n42\n"), canonical)
}

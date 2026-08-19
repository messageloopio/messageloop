// Package hmac implements the HMAC-SHA256 signing gate for the cluster
// command bus (PR-KA-B4, KD-K31): every command and command result traveling
// over Redis Pub/Sub carries a signature over a canonical byte encoding, and
// receivers reject envelopes that are unsigned, badly signed, or stale.
//
// The canonical encoding is a fixed, line-oriented byte layout (never JSON,
// whose field order is unstable). The audit-only IssuedBy field is NOT part
// of the canonical bytes: it is forgeable and does not constitute a security
// boundary — the signature is the boundary.
//
// The signing key comes from node configuration only (cluster.hmac_key or
// cluster.hmac_key_file). It must never appear in a Redis key, a PUBLISH
// payload, a log line, or a metrics label.
package hmac

import (
	"bytes"
	cryptohmac "crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/messageloopio/messageloop/internal/cluster"
)

// MaxClockSkew bounds the accepted distance between a command's or result's
// IssuedAt timestamp and the verifier's clock.
const MaxClockSkew = 30 * time.Second

// RejectReason classifies why an envelope failed verification. The values are
// stable: they are used as the reason label of the
// cluster_command_hmac_reject_total metric.
type RejectReason string

const (
	// RejectMissing: the Signature field is empty.
	RejectMissing RejectReason = "missing"
	// RejectBad: the signature is not valid hex or the MAC does not match.
	RejectBad RejectReason = "bad"
	// RejectSkew: IssuedAt is zero or farther than MaxClockSkew from now.
	RejectSkew RejectReason = "skew"
	// RejectID: the command carries no CommandID (results are exempt).
	RejectID RejectReason = "id"
)

// VerifyError reports a rejected envelope. The Reason field is safe to log
// and to use as a metrics label; no key material is ever included.
type VerifyError struct {
	Reason RejectReason
	Detail string
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("cluster envelope rejected (%s): %s", e.Reason, e.Detail)
}

// SignCommand stamps cmd.Signature with hex(HMAC-SHA256(key, canonical(cmd))).
// The caller must have filled CommandID and IssuedAt already; signing fails
// on a nil command and never mutates any field other than Signature.
func SignCommand(key []byte, cmd *cluster.ClusterCommand) error {
	if cmd == nil {
		return errors.New("cannot sign a nil cluster command")
	}
	cmd.Signature = compute(key, canonicalCommand(cmd))
	return nil
}

// VerifyCommand checks a received command: signature present, CommandID
// present, IssuedAt within MaxClockSkew of now, and MAC matching. A failure
// returns a *VerifyError; the caller must not claim, execute, or answer the
// command.
func VerifyCommand(key []byte, cmd *cluster.ClusterCommand, now time.Time) error {
	if cmd == nil {
		return &VerifyError{Reason: RejectBad, Detail: "nil command"}
	}
	if cmd.Signature == "" {
		return &VerifyError{Reason: RejectMissing, Detail: "command carries no signature"}
	}
	if cmd.CommandID == "" {
		// Checked before the MAC so an attacker cannot pick a victim's (or an
		// empty) command id to poison the dedupe state keys.
		return &VerifyError{Reason: RejectID, Detail: "command carries no command_id"}
	}
	if err := checkSkew(cmd.IssuedAt, now); err != nil {
		return err
	}
	if !verify(key, canonicalCommand(cmd), cmd.Signature) {
		return &VerifyError{Reason: RejectBad, Detail: "command MAC mismatch"}
	}
	return nil
}

// SignResult stamps res.Signature with hex(HMAC-SHA256(key,
// canonical(res))). The caller must have filled IssuedAt already.
func SignResult(key []byte, res *cluster.ClusterCommandResult) error {
	if res == nil {
		return errors.New("cannot sign a nil cluster command result")
	}
	res.Signature = compute(key, canonicalResult(res))
	return nil
}

// VerifyResult checks a received command result. A forged "succeeded" reply
// fails here and must be treated as if no reply had arrived.
func VerifyResult(key []byte, res *cluster.ClusterCommandResult, now time.Time) error {
	if res == nil {
		return &VerifyError{Reason: RejectBad, Detail: "nil result"}
	}
	if res.Signature == "" {
		return &VerifyError{Reason: RejectMissing, Detail: "result carries no signature"}
	}
	if err := checkSkew(res.IssuedAt, now); err != nil {
		return err
	}
	if !verify(key, canonicalResult(res), res.Signature) {
		return &VerifyError{Reason: RejectBad, Detail: "result MAC mismatch"}
	}
	return nil
}

// canonicalCommand is the byte-exact signing payload of a command: UTF-8
// lines joined by '\n', with a trailing '\n' on the last line. IssuedBy,
// Channel, Metadata, and TargetNodeID are deliberately excluded.
func canonicalCommand(cmd *cluster.ClusterCommand) []byte {
	// sha256 of a nil slice and of an empty slice is the same digest.
	payloadHash := sha256.Sum256(cmd.Payload)
	var b bytes.Buffer
	b.WriteString("v1\n")
	writeLine(&b, string(cmd.Type))
	writeLine(&b, cmd.SessionID)
	writeLine(&b, cmd.TargetIncarnationID)
	writeLine(&b, strconv.FormatUint(cmd.LeaseVersion, 10))
	writeLine(&b, hex.EncodeToString(payloadHash[:]))
	writeLine(&b, cmd.CommandID)
	writeLine(&b, strconv.FormatInt(cmd.IssuedAt.UTC().Unix(), 10))
	return b.Bytes()
}

// canonicalResult is the byte-exact signing payload of a command result.
func canonicalResult(res *cluster.ClusterCommandResult) []byte {
	var b bytes.Buffer
	b.WriteString("v1-result\n")
	writeLine(&b, res.CommandID)
	writeLine(&b, string(res.Status))
	writeLine(&b, res.ErrorCode)
	writeLine(&b, res.SessionID)
	writeLine(&b, res.NodeID)
	writeLine(&b, res.IncarnationID)
	writeLine(&b, strconv.FormatInt(res.IssuedAt.UTC().Unix(), 10))
	return b.Bytes()
}

func writeLine(b *bytes.Buffer, line string) {
	b.WriteString(line)
	b.WriteByte('\n')
}

func checkSkew(issuedAt time.Time, now time.Time) error {
	if issuedAt.IsZero() {
		return &VerifyError{Reason: RejectSkew, Detail: "issued_at is zero"}
	}
	diff := now.UTC().Unix() - issuedAt.UTC().Unix()
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(MaxClockSkew/time.Second) {
		return &VerifyError{Reason: RejectSkew, Detail: "issued_at is outside the ±30s clock-skew window"}
	}
	return nil
}

func compute(key, canonical []byte) string {
	mac := cryptohmac.New(sha256.New, key)
	mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

func verify(key, canonical []byte, signature string) bool {
	received, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := cryptohmac.New(sha256.New, key)
	mac.Write(canonical)
	return cryptohmac.Equal(received, mac.Sum(nil))
}

package messageloop

import (
	"errors"
	"testing"
)

func TestDisconnect_String(t *testing.T) {
	tests := []struct {
		name     string
		disconnect Disconnect
		want     string
	}{
		{
			name: "with code and reason",
			disconnect: Disconnect{Code: 3000, Reason: "connection closed"},
			want: "code: 3000, reason: connection closed",
		},
		{
			name: "with code only",
			disconnect: Disconnect{Code: 3500, Reason: ""},
			want: "code: 3500, reason: ",
		},
		{
			name: "with reason only",
			disconnect: Disconnect{Code: 0, Reason: "custom reason"},
			want: "code: 0, reason: custom reason",
		},
		{
			name:     "empty disconnect",
			disconnect: Disconnect{},
			want:     "code: 0, reason: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.disconnect.String(); got != tt.want {
				t.Errorf("Disconnect.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDisconnect_Error(t *testing.T) {
	d := Disconnect{Code: 3500, Reason: "invalid token"}
	if got := d.Error(); got != d.String() {
		t.Errorf("Disconnect.Error() = %v, want %v", got, d.String())
	}
}

func TestDisconnect_AsError(t *testing.T) {
	d := Disconnect{Code: 3500, Reason: "test disconnect"}
	var dis Disconnect
	if !errors.As(d, &dis) {
		t.Error("Disconnect should be usable with errors.As")
	}
	if dis.Code != d.Code || dis.Reason != d.Reason {
		t.Errorf("errors.As did not properly extract Disconnect, got %+v, want %+v", dis, d)
	}
}

func TestDisconnect_ErrorIs(t *testing.T) {
	d := Disconnect{Code: 3500, Reason: "test"}
	var target error = d
	if !errors.Is(d, target) {
		t.Error("Disconnect should be comparable with errors.Is")
	}
}

func TestBuiltinDisconnectCodes(t *testing.T) {
	tests := []struct {
		name     string
		disconnect Disconnect
		wantCode uint32
		wantReason string
	}{
		{
			name:     "DisconnectConnectionClosed",
			disconnect: DisconnectConnectionClosed,
			wantCode: 3000,
			wantReason: "connection closed",
		},
		{
			name:     "DisconnectInvalidToken",
			disconnect: DisconnectInvalidToken,
			wantCode: 3500,
			wantReason: "invalid token",
		},
		{
			name:     "DisconnectBadRequest",
			disconnect: DisconnectBadRequest,
			wantCode: 3501,
			wantReason: "bad request",
		},
		{
			name:     "DisconnectStale",
			disconnect: DisconnectStale,
			wantCode: 3502,
			wantReason: "stale",
		},
		{
			name:     "DisconnectForceNoReconnect",
			disconnect: DisconnectForceNoReconnect,
			wantCode: 3503,
			wantReason: "force disconnect",
		},
		{
			name:     "DisconnectConnectionLimit",
			disconnect: DisconnectConnectionLimit,
			wantCode: 3504,
			wantReason: "connection limit",
		},
		{
			name:     "DisconnectChannelLimit",
			disconnect: DisconnectChannelLimit,
			wantCode: 3505,
			wantReason: "channel limit",
		},
		{
			name:     "DisconnectInappropriateProtocol",
			disconnect: DisconnectInappropriateProtocol,
			wantCode: 3506,
			wantReason: "inappropriate protocol",
		},
		{
			name:     "DisconnectPermissionDenied",
			disconnect: DisconnectPermissionDenied,
			wantCode: 3507,
			wantReason: "permission denied",
		},
		{
			name:     "DisconnectNotAvailable",
			disconnect: DisconnectNotAvailable,
			wantCode: 3508,
			wantReason: "not available",
		},
		{
			name:     "DisconnectTooManyErrors",
			disconnect: DisconnectTooManyErrors,
			wantCode: 3509,
			wantReason: "too many errors",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.disconnect.Code != tt.wantCode {
				t.Errorf("Code = %v, want %v", tt.disconnect.Code, tt.wantCode)
			}
			if tt.disconnect.Reason != tt.wantReason {
				t.Errorf("Reason = %v, want %v", tt.disconnect.Reason, tt.wantReason)
			}
		})
	}
}

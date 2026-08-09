package websocket

import (
	"testing"

	"github.com/gorilla/websocket"
	"github.com/messageloopio/messageloop"
)

func TestCloseCode_FallsBackToNormalClosureWhenZero(t *testing.T) {
	got := closeCode(messageloop.Disconnect{})
	if got != websocket.CloseNormalClosure {
		t.Errorf("closeCode(Disconnect{}) = %d, want %d", got, websocket.CloseNormalClosure)
	}
}

func TestCloseCode_PreservesNonZeroCode(t *testing.T) {
	got := closeCode(messageloop.Disconnect{Code: 3500, Reason: "invalid token"})
	if got != 3500 {
		t.Errorf("closeCode() = %d, want 3500", got)
	}
}

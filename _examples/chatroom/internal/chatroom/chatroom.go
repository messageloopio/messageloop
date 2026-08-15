// Package chatroom contains shared constants and helpers for the ChatRoom
// E2E demo under _examples/chatroom.
package chatroom

// Channel names used across the demo.
const (
	// Lobby is the public chat channel.
	Lobby = "chat:lobby"
	// PrivateChannelPrefix gates private rooms behind authentication (ACL demo).
	PrivateChannelPrefix = "private:"
)

// User is a demo account known to the backend auth handler.
type User struct {
	ID   string
	Name string
	Role string // "owner" or "member"
}

// Users is the demo user table. Tokens follow the "token-<name>" scheme so
// every client can derive its token from its user name.
var Users = map[string]User{
	"token-alice": {ID: "user-alice", Name: "alice", Role: "owner"},
	"token-bob":   {ID: "user-bob", Name: "bob", Role: "member"},
	"token-carol": {ID: "user-carol", Name: "carol", Role: "member"},
	"token-dave":  {ID: "user-dave", Name: "dave", Role: "member"},
	"token-eve":   {ID: "user-eve", Name: "eve", Role: "member"},
}

// TokenForName returns the demo token for a user name.
func TokenForName(name string) string {
	return "token-" + name
}

// LookupByToken resolves a token to a demo user.
func LookupByToken(token string) (User, bool) {
	u, ok := Users[token]
	return u, ok
}

// ChatMessage is the JSON payload shape shared by all demo clients.
type ChatMessage struct {
	User string `json:"user,omitempty"`
	Text string `json:"text,omitempty"`
	Kind string `json:"kind,omitempty"` // "chat", "system", "poll", ...
}

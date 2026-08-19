package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/messageloopio/messageloop"
	clusterpkg "github.com/messageloopio/messageloop/internal/cluster"
)

// World is the deterministic two-node fencing fixture: nodes A and B are real
// *messageloop.Node instances (running the production syncClusterSessionState
// / resumeRemoteSession / Fence paths) sharing one in-memory Directory and
// one orchestrable Bus. Incarnation IDs are scripted (inc-a / inc-b), the
// backend is memory, and no component is started, so no background goroutine
// or ticker ever races a scenario step.
type World struct {
	Clock *Clock
	Dir   *Directory
	Bus   *Bus

	A *messageloop.Node
	B *messageloop.Node

	// RepairerA / RepairerB are the per-node cluster repairers built over the
	// shared Directory. They are never started; tests drive membership beats
	// explicitly (messageloop.SimMembershipOnce).
	RepairerA clusterpkg.ClusterRepairer
	RepairerB clusterpkg.ClusterRepairer
}

// NewWorld assembles the two-node fixture: node-a/inc-a and node-b/inc-b on a
// shared Directory and Bus, memory backend, no node leases (tests add them
// explicitly when scripting membership).
func NewWorld() *World {
	world := &World{
		Clock: NewClock(time.Unix(1_700_000_000, 0).UTC()),
		Dir:   NewDirectory(),
		Bus:   NewBus(),
	}
	world.A, world.RepairerA = world.newNode("node-a", "inc-a")
	world.B, world.RepairerB = world.newNode("node-b", "inc-b")
	return world
}

func (w *World) newNode(nodeID, incarnationID string) (*messageloop.Node, clusterpkg.ClusterRepairer) {
	node := messageloop.NewNode(nil)
	repairer := messageloop.NewClusterRepairer(node, w.Dir, nil, messageloop.ClusterRepairerConfig{
		NodeID:        nodeID,
		IncarnationID: incarnationID,
	})
	cluster, err := messageloop.NewCluster(messageloop.ClusterOptions{
		Enabled:       true,
		NodeID:        nodeID,
		IncarnationID: incarnationID,
		Backend:       "memory",
	}, messageloop.ClusterDependencies{
		SessionDirectory: w.Dir,
		CommandBus:       w.Bus,
		Repairer:         repairer,
	})
	if err != nil {
		panic(fmt.Sprintf("sim world: build cluster for %s: %v", nodeID, err))
	}
	node.SetCluster(cluster)
	w.Bus.Register(nodeID, incarnationID, node.ClusterCommandHandler())
	return node, repairer
}

// AddClient wires an authenticated, attached test client on node and claims
// its session lease: NewClient + ForceTestIDs + AddClient (whose cluster sync
// does the first CAS(nil) claim). It fails when the Directory rejects the
// claim (the session is already owned elsewhere).
func (w *World) AddClient(node *messageloop.Node, sessionID, userID, clientID string) (*messageloop.Session, error) {
	client, _, err := messageloop.NewClient(context.Background(), node, noopTransport{}, messageloop.JSONMarshaler{})
	if err != nil {
		return nil, err
	}
	client.ForceTestIDs(sessionID, userID, clientID)
	if err := node.AddClient(client); err != nil {
		return nil, err
	}
	return client, nil
}

// NewResumeClient creates a fresh, unattached client on node: the inbound
// connection a cross-node resume would arrive on. Hand it to
// messageloop.SimResumeRemoteSession, then AttachResumed.
func (w *World) NewResumeClient(node *messageloop.Node) (*messageloop.Session, error) {
	client, _, err := messageloop.NewClient(context.Background(), node, noopTransport{}, messageloop.JSONMarshaler{})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// AttachResumed attaches and registers a client after a successful
// SimResumeRemoteSession, mirroring the production handleConnect commit: the
// resumed session joins the hub and refreshes its (newly claimed) lease.
func (w *World) AttachResumed(node *messageloop.Node, client *messageloop.Session, sessionID, userID, clientID string) error {
	client.ForceTestIDs(sessionID, userID, clientID)
	return node.AddClient(client)
}

// noopTransport is the simulator's transport: writes go nowhere, the Attach
// readiness probe (WriteMany) always succeeds, and Close is recorded nowhere
// — fencing assertions read Session.State, not the wire.
type noopTransport struct{}

func (noopTransport) Write([]byte) error                 { return nil }
func (noopTransport) WriteMany(...[]byte) error          { return nil }
func (noopTransport) Close(messageloop.Disconnect) error { return nil }
func (noopTransport) RemoteAddr() string                 { return "sim" }

var _ messageloop.Transport = noopTransport{}

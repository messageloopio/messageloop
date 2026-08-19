// Package cluster holds the cluster control-plane contracts (KD-K26 phase
// three (a), PR-KA-D13): the component lifecycle, session directory, command
// bus, query store and lease lister interfaces, the command/result transport
// model with its type and status enums, the node-epoch allocator, and the
// user-index sync helper. Everything here was sunk from the root
// messageloop package. The Cluster facade, its noop implementations and the
// node lease manager live in internal/runtime (D15).
package cluster

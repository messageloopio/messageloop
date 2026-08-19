// Package cluster holds the cluster control-plane contracts (KD-K26 phase
// three (a), PR-KA-D13): the component lifecycle, session directory, command
// bus, query store and lease lister interfaces, the command/result transport
// model with its type and status enums, the node-epoch allocator, and the
// user-index sync helper. Everything here was sunk unchanged from the root
// messageloop package; the Cluster facade, its noop implementations and the
// node lease manager stay in the root package until D15 and reach these
// contracts through the aliases in aliases.go.
package cluster

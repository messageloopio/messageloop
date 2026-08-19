// Package runtime is the Node orchestration layer after PR-KA-D15 (KD-K26).
//
// It holds Node, the Cluster facade (NewCluster / lease manager / repairer),
// recover, health, the subscription saga, Sim fencing hooks, and the
// session.Runtime adapter. Leaf contracts stay in internal/{session,cluster,
// occupancy,survey,stream,protocol,authz,metrics}; this package imports
// those packages and must not be imported by them.
package runtime

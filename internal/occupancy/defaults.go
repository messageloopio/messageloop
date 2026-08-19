package occupancy

// MaxPresenceSnapshotClients caps the number of PresenceInfo entries in
// a presence snapshot (Connected.presence / SubscribeAck.presence /
// PresenceQuery). A channel policy with presence_snapshot_limit > 0 may
// override this cap up or down.
const MaxPresenceSnapshotClients = 256

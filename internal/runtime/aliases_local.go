// Local type aliases so the git-mv'd files keep using short names (body
// stays byte-identical except package/import and the §3.3 wrapper call
// sites). These alias internal/* and shared — they are not root-package
// aliases and are not a public API for cmd/server or transports.
package runtime

import (
	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/internal/channel"
	"github.com/messageloopio/messageloop/internal/cluster"
	"github.com/messageloopio/messageloop/internal/metrics"
	"github.com/messageloopio/messageloop/internal/occupancy"
	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/internal/stream"
	"github.com/messageloopio/messageloop/internal/survey"
	"github.com/messageloopio/messageloop/shared"
)

// --- internal/protocol ---

type Disconnect = protocol.Disconnect

var (
	DisconnectConnectionClosed      = protocol.DisconnectConnectionClosed
	DisconnectInvalidToken          = protocol.DisconnectInvalidToken
	DisconnectBadRequest            = protocol.DisconnectBadRequest
	DisconnectStale                 = protocol.DisconnectStale
	DisconnectForceNoReconnect      = protocol.DisconnectForceNoReconnect
	DisconnectConnectionLimit       = protocol.DisconnectConnectionLimit
	DisconnectChannelLimit          = protocol.DisconnectChannelLimit
	DisconnectInappropriateProtocol = protocol.DisconnectInappropriateProtocol
	DisconnectPermissionDenied      = protocol.DisconnectPermissionDenied
	DisconnectNotAvailable          = protocol.DisconnectNotAvailable
	DisconnectTooManyErrors         = protocol.DisconnectTooManyErrors
	DisconnectIdleTimeout           = protocol.DisconnectIdleTimeout
	DisconnectSlowConsumer          = protocol.DisconnectSlowConsumer
	DisconnectInternal              = protocol.DisconnectInternal
	DisconnectUnsupportedVersion    = protocol.DisconnectUnsupportedVersion
)

// --- internal/channel ---

var ErrPatternNotRoutable = channel.ErrPatternNotRoutable

type CompiledInterest = channel.CompiledInterest

var (
	CompileInterest   = channel.CompileInterest
	MatchAfterCompile = channel.MatchAfterCompile
)

type ChannelPolicy = channel.ChannelPolicy

var (
	DefaultChannelPolicy = channel.DefaultChannelPolicy
	ErrHistoryDisabled   = channel.ErrHistoryDisabled
)

// --- internal/authz ---

type (
	Action        = authz.Action
	PrincipalKind = authz.PrincipalKind
	Principal     = authz.Principal
	Capability    = authz.Capability
	Decision      = authz.Decision
	Authorizer    = authz.Authorizer
)

const (
	ActionSubscribePattern = authz.ActionSubscribePattern
	ActionPublish          = authz.ActionPublish
	ActionRecover          = authz.ActionRecover
	ActionPresence         = authz.ActionPresence
	ActionSurvey           = authz.ActionSurvey
)

const (
	PrincipalUser  = authz.PrincipalUser
	PrincipalAdmin = authz.PrincipalAdmin
)

const (
	CapPresenceLargeSnapshot = authz.CapPresenceLargeSnapshot
	CapSurveyBypassGate      = authz.CapSurveyBypassGate
	CapHistoryRead           = authz.CapHistoryRead
	CapPresenceRead          = authz.CapPresenceRead
	CapChannelsList          = authz.CapChannelsList
	CapSessionAct            = authz.CapSessionAct
	CapUserFanout            = authz.CapUserFanout
	CapSubscribeAny          = authz.CapSubscribeAny
	CapPatternGlobal         = authz.CapPatternGlobal
)

var (
	ClosedCapabilityNames    = authz.ClosedCapabilityNames
	DefaultAdminCapabilities = authz.DefaultAdminCapabilities
	ErrInvalidRulePattern    = authz.ErrInvalidRulePattern
	NewAuthorizer            = authz.NewAuthorizer
)

// --- internal/occupancy ---

type (
	PresenceInfo           = occupancy.PresenceInfo
	PresenceStore          = occupancy.PresenceStore
	PresenceEvent          = occupancy.PresenceEvent
	OccupancyEvent         = occupancy.OccupancyEvent
	OccupancyGenSource     = occupancy.OccupancyGenSource
	SyntheticLeaveReporter = occupancy.SyntheticLeaveReporter
)

const (
	PresenceActionJoin  = occupancy.PresenceActionJoin
	PresenceActionLeave = occupancy.PresenceActionLeave
)

var (
	ErrLateOccupancy       = occupancy.ErrLateOccupancy
	NewMemoryPresenceStore = occupancy.NewMemoryPresenceStore
)

const MaxPresenceSnapshotClients = occupancy.MaxPresenceSnapshotClients

// --- internal/stream ---

type (
	PayloadKind        = stream.PayloadKind
	Publication        = stream.Publication
	PublicationHandler = stream.PublicationHandler
	OccupancyHandler   = stream.OccupancyHandler
	CatchUpGap         = stream.CatchUpGap
	GapHandler         = stream.GapHandler
	HistoryGapReason   = stream.HistoryGapReason
	HistoryPage        = stream.HistoryPage
	Broker             = stream.Broker
)

const (
	PayloadKindBinary = stream.PayloadKindBinary
	PayloadKindText   = stream.PayloadKindText
	PayloadKindJSON   = stream.PayloadKindJSON
)

const (
	HistoryGapNone            = stream.HistoryGapNone
	HistoryGapHeadTrimmed     = stream.HistoryGapHeadTrimmed
	HistoryGapEmptyExpired    = stream.HistoryGapEmptyExpired
	HistoryGapMiddle          = stream.HistoryGapMiddle
	HistoryGapReplayTruncated = stream.HistoryGapReplayTruncated
)

var (
	PublicationFromPayloadV2 = stream.PublicationFromPayloadV2
)

type MemoryBrokerOptions = stream.MemoryBrokerOptions

var NewMemoryBroker = stream.NewMemoryBroker

// --- internal/cluster ---

type (
	ClusterOptions                = cluster.ClusterOptions
	ClusterLifecycle              = cluster.ClusterLifecycle
	SessionDirectory              = cluster.SessionDirectory
	ClusterCommandBus             = cluster.ClusterCommandBus
	ClusterNodeProjection         = cluster.ClusterNodeProjection
	ClusterQueryStore             = cluster.ClusterQueryStore
	ClusterNodeLeaseManager       = cluster.ClusterNodeLeaseManager
	ClusterRepairer               = cluster.ClusterRepairer
	ClusterSessionLeaseLister     = cluster.ClusterSessionLeaseLister
	ClusterNodeLeaseLister        = cluster.ClusterNodeLeaseLister
	ClusterCommandType            = cluster.ClusterCommandType
	ClusterCommandStatus          = cluster.ClusterCommandStatus
	ClusterCommand                = cluster.ClusterCommand
	ClusterCommandResult          = cluster.ClusterCommandResult
	ClusterNodeLease              = cluster.ClusterNodeLease
	ClusterSessionLease           = cluster.ClusterSessionLease
	ClusterSubscriptionSnapshot   = cluster.ClusterSubscriptionSnapshot
	ClusterSessionSnapshot        = cluster.ClusterSessionSnapshot
	ClusterChannelInfo            = cluster.ClusterChannelInfo
	ClusterCommandHandler         = cluster.ClusterCommandHandler
	SessionStateCompareAndSwapper = cluster.SessionStateCompareAndSwapper
	NodeEpochAllocator            = cluster.NodeEpochAllocator
	MemoryNodeEpochAllocator      = cluster.MemoryNodeEpochAllocator
)

const (
	ClusterCommandDisconnect  = cluster.ClusterCommandDisconnect
	ClusterCommandSubscribe   = cluster.ClusterCommandSubscribe
	ClusterCommandUnsubscribe = cluster.ClusterCommandUnsubscribe
	ClusterCommandPublish     = cluster.ClusterCommandPublish
	ClusterCommandTakeover    = cluster.ClusterCommandTakeover
	ClusterCommandSurvey      = cluster.ClusterCommandSurvey
)

const (
	ClusterCommandStatusPending           = cluster.ClusterCommandStatusPending
	ClusterCommandStatusSucceeded         = cluster.ClusterCommandStatusSucceeded
	ClusterCommandStatusFailed            = cluster.ClusterCommandStatusFailed
	ClusterCommandStatusInProgress        = cluster.ClusterCommandStatusInProgress
	ClusterCommandStatusUnknownFinalState = cluster.ClusterCommandStatusUnknownFinalState
)

var (
	ErrClusterCommandUnsupported = cluster.ErrClusterCommandUnsupported
	ErrSessionFenced             = cluster.ErrSessionFenced
	FormatNodeEpoch              = cluster.FormatNodeEpoch
	ParseNodeEpoch               = cluster.ParseNodeEpoch
	NodeEpochNewer               = cluster.NodeEpochNewer
	NewMemoryNodeEpochAllocator  = cluster.NewMemoryNodeEpochAllocator
	SyncUserIndex                = cluster.SyncUserIndex
)

// --- internal/metrics ---

type Metrics = metrics.Metrics

var (
	NewMetrics            = metrics.NewMetrics
	MetricsTransportLabel = metrics.MetricsTransportLabel
)

// --- internal/session ---

type (
	Session           = session.Session
	Client            = session.Client
	SessionState      = session.SessionState
	Attachment        = session.Attachment
	Subscriber        = session.Subscriber
	Hub               = session.Hub
	ChannelInfo       = session.ChannelInfo
	Transport         = session.Transport
	HeartbeatManager  = session.HeartbeatManager
	HeartbeatConfig   = session.HeartbeatConfig
	ClientOption      = session.ClientOption
	ClientCloseFunc   = session.ClientCloseFunc
	ClientInfo        = session.ClientInfo
	RestoreFailure    = session.RestoreFailure
	Runtime           = session.Runtime
	IdentitySnapshot  = session.IdentitySnapshot
	PresenceRecipient = session.PresenceRecipient
)

const (
	SessionAuthenticating    = session.SessionAuthenticating
	SessionAttached          = session.SessionAttached
	SessionDetached          = session.SessionDetached
	SessionClosed            = session.SessionClosed
	SystemMethodAuthenticate = session.SystemMethodAuthenticate
)

var (
	NewSubscriber         = session.NewSubscriber
	NewHeartbeatManager   = session.NewHeartbeatManager
	WithProtocol          = session.WithProtocol
	MakeOutboundMessage   = session.MakeOutboundMessage
	MarshalJSONStruct     = session.MarshalJSONStruct
	ErrSendQueueFull      = session.ErrSendQueueFull
	ErrSessionNotAttached = session.ErrSessionNotAttached
	ErrOutboundTooLarge   = session.ErrOutboundTooLarge
)

// --- internal/survey ---

type (
	Survey       = survey.Survey
	SurveyResult = survey.SurveyResult
)

var NewSurvey = survey.NewSurvey

const (
	MaxSurveyAnswerBytes = survey.MaxSurveyAnswerBytes
	MaxSurveyResultBytes = survey.MaxSurveyResultBytes
)

// --- shared marshaler ---

type (
	Marshaler          = shared.Marshaler
	JSONMarshaler      = shared.JSONMarshaler
	ProtobufMarshaler  = shared.ProtobufMarshaler
	MarshalTypeError   = shared.MarshalTypeError
	UnmarshalTypeError = shared.UnmarshalTypeError
)

var (
	ProtoJSONMarshaler = shared.ProtoJSONMarshaler
	Marshalers         = shared.Marshalers
)

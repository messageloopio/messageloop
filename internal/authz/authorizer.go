// Package authz holds the authorization contract (Authorizer/Action/
// Principal/Capability/Decision) sunk from the root package in PR-KA-D12
// (KD-K26 phase two; target layout: docs/v2/kernel-architecture.md :173-191).
package authz

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/messageloopio/messageloop/config"
	channelpkg "github.com/messageloopio/messageloop/internal/channel"
	"github.com/messageloopio/messageloop/pkg/topics"
)

// Action is the authorized operation. SubscribePattern takes a subscription
// key (pattern); every other action takes an exact channel.
type Action int

const (
	// ActionSubscribePattern authorizes subscribing a pattern: the pattern's
	// language L(p) must be included in the principal's allow language.
	ActionSubscribePattern Action = iota
	// ActionPublish authorizes publishing to an exact channel. It never
	// requires session coverage (KD-K21).
	ActionPublish
	// ActionRecover authorizes reading history of an exact channel.
	ActionRecover
	// ActionPresence authorizes reading presence of an exact channel.
	ActionPresence
	// ActionSurvey authorizes initiating a client survey on an exact channel.
	ActionSurvey
)

// PrincipalKind distinguishes the two authorization subjects.
type PrincipalKind int

const (
	// PrincipalUser is an authenticated client user.
	PrincipalUser PrincipalKind = iota
	// PrincipalAdmin is the server-side admin API principal.
	PrincipalAdmin
)

// Principal is the authorization subject of a Decide call.
type Principal struct {
	Kind   PrincipalKind
	UserID string     // User 的用户 ID；Admin 规则匹配仍可用 "admin"
	Caps   Capability // 仅 Admin 有意义；User 为 0
}

// Capability is a closed set of admin privilege bits (KD-K15).
type Capability uint32

const (
	// CapPresenceLargeSnapshot allows returning a full presence snapshot
	// beyond the configured cap (presence.large_snapshot).
	CapPresenceLargeSnapshot Capability = 1 << iota
	// CapSurveyBypassGate lets Admin surveys skip the population gate, the
	// survey allow rules and the client in-flight gate (survey.bypass_gate).
	CapSurveyBypassGate
	// CapHistoryRead gates GetHistory (history.read).
	CapHistoryRead
	// CapPresenceRead gates GetPresence (presence.read).
	CapPresenceRead
	// CapChannelsList gates GetChannels (channels.list).
	CapChannelsList
	// CapSessionAct allows per-session delivery / disconnect / subscribe
	// (session.act).
	CapSessionAct
	// CapUserFanout allows per-user expansion before session.act
	// (user.fanout).
	CapUserFanout
	// CapSubscribeAny lets the admin subscribe on behalf of a session
	// without appearing in allow_subscribe lists (subscribe.any).
	CapSubscribeAny
	// CapPatternGlobal is reserved for holding a bare "**" Interest. It is
	// already in the closed set but does NOT unlock broker.Subscribe("**")
	// (pattern.global; A3 / KD-K13 stay).
	CapPatternGlobal
)

// ClosedCapabilityNames is the closed set. Unknown YAML names are a Validate
// error (config.Validate keeps its own copy of the names to avoid an import
// cycle; keep the two lists in sync).
var ClosedCapabilityNames = map[string]Capability{
	"presence.large_snapshot": CapPresenceLargeSnapshot,
	"survey.bypass_gate":      CapSurveyBypassGate,
	"history.read":            CapHistoryRead,
	"presence.read":           CapPresenceRead,
	"channels.list":           CapChannelsList,
	"session.act":             CapSessionAct,
	"user.fanout":             CapUserFanout,
	"subscribe.any":           CapSubscribeAny,
	"pattern.global":          CapPatternGlobal,
}

// DefaultAdminCapabilities is used when server.grpc_admin.capabilities is
// omitted: every closed bit except CapPatternGlobal (holding ** Interest must
// be explicit).
var DefaultAdminCapabilities Capability = CapPresenceLargeSnapshot |
	CapSurveyBypassGate |
	CapHistoryRead |
	CapPresenceRead |
	CapChannelsList |
	CapSessionAct |
	CapUserFanout |
	CapSubscribeAny

// Decision is the outcome of one Decide call. Effects always carries the
// effective channel policy for the channel, regardless of Allow.
type Decision struct {
	Allow   bool
	Reason  string // "default" | "deny_all" | "allow_list" | "effects" | "not_routable" | "missing_capability" | "language"
	Effects channelpkg.ChannelPolicy
}

// ErrInvalidRulePattern is returned by NewAuthorizer / ReplaceRules when a
// rule pattern is not part of the subscription key language (§5.1).
var ErrInvalidRulePattern = errors.New("invalid authorizer rule pattern")

// patternKind classifies one rule pattern after compilation (§5.1).
type patternKind uint8

const (
	patternExact patternKind = iota
	patternStar              // literal prefix + final single-segment "*"
	patternDStar             // literal prefix + trailing "**"
)

// compiledRulePattern is the internal compiled form of one pattern.
// segments holds the full segment list for exact patterns and the literal
// prefix for star / dstar patterns.
type compiledRulePattern struct {
	kind     patternKind
	segments []string
}

// compiledRule is one parsed authorizer rule. A nil allow list means the rule
// does not constrain the action; an empty list means it denies the action.
// surveySet mirrors effects' explicit Survey override (CompiledPolicySpec is
// opaque; the bit is captured from the config spec at compile time for the
// ActionSurvey "effects" reason).
type compiledRule struct {
	raw            string
	pattern        compiledRulePattern
	denyAll        bool
	allowSubscribe []string
	subscribeAll   bool
	allowPublish   []string
	publishAll     bool
	allowSurvey    []string
	surveyAll      bool
	surveySet      bool
	effects        channelpkg.CompiledPolicySpec
}

// Authorizer is the single authorization evaluator: one Decide, one table,
// one wildcard language (KD-K10). Rules are compiled from
// config.AuthorizerConfig; the zero config yields "subscribe/publish open,
// survey off" with the default channel policy.
type Authorizer struct {
	mu    sync.RWMutex
	base  channelpkg.ChannelPolicy
	rules []compiledRule
}

// NewAuthorizer compiles cfg into an Authorizer. A zero cfg is valid: no
// rules, default Effects = channelpkg.DefaultChannelPolicy(). Rules whose pattern is not
// part of the subscription key language (bad topic, middle "**", bare "*" /
// "**") are rejected.
func NewAuthorizer(cfg config.AuthorizerConfig) (*Authorizer, error) {
	a := &Authorizer{
		base:  channelpkg.Overlay(channelpkg.DefaultChannelPolicy(), channelpkg.CompilePolicySpec(cfg.Default)),
		rules: make([]compiledRule, 0, len(cfg.Rules)),
	}
	for i, rule := range cfg.Rules {
		compiled, err := compileRule(rule)
		if err != nil {
			return nil, fmt.Errorf("authorizer rules[%d]: %w", i, err)
		}
		a.rules = append(a.rules, compiled)
	}
	return a, nil
}

func compileRule(rule config.AuthorizerRule) (compiledRule, error) {
	pattern, err := compilePattern(rule.Pattern)
	if err != nil {
		return compiledRule{}, err
	}
	compiled := compiledRule{
		raw:       rule.Pattern,
		pattern:   pattern,
		denyAll:   rule.DenyAll,
		surveySet: rule.Survey != nil,
		effects:   channelpkg.CompilePolicySpec(rule.ChannelPolicySpec),
	}
	compiled.allowSubscribe, compiled.subscribeAll = compileAllowList(rule.AllowSubscribe)
	compiled.allowPublish, compiled.publishAll = compileAllowList(rule.AllowPublish)
	compiled.allowSurvey, compiled.surveyAll = compileAllowList(rule.AllowSurvey)
	return compiled, nil
}

// compileAllowList converts the YAML list into (list, hasWildcard). A nil
// input stays nil ("omitted": the rule does not constrain the action).
func compileAllowList(list []string) ([]string, bool) {
	if list == nil {
		return nil, false
	}
	all := false
	for _, userID := range list {
		if userID == "*" {
			all = true
		}
	}
	return list, all
}

// compilePattern compiles one rule pattern into the internal form. The rule
// pattern language is the subscription key language (§5.1): valid topic,
// wildcard only as the final segment, no empty literal prefix.
func compilePattern(pattern string) (compiledRulePattern, error) {
	if err := topics.ValidateTopic(pattern); err != nil {
		return compiledRulePattern{}, fmt.Errorf("%w: invalid pattern %q: %v", ErrInvalidRulePattern, pattern, err)
	}
	if !strings.Contains(pattern, "*") {
		return compiledRulePattern{kind: patternExact, segments: strings.Split(pattern, ".")}, nil
	}
	segments := strings.Split(pattern, ".")
	last := segments[len(segments)-1]
	if last != "*" && last != "**" {
		return compiledRulePattern{}, fmt.Errorf("%w: %q: wildcard must be the final segment", ErrInvalidRulePattern, pattern)
	}
	prefix := segments[:len(segments)-1]
	for _, seg := range prefix {
		if strings.Contains(seg, "*") {
			return compiledRulePattern{}, fmt.Errorf("%w: %q: only the final segment may be a wildcard", ErrInvalidRulePattern, pattern)
		}
	}
	if len(prefix) == 0 {
		return compiledRulePattern{}, fmt.Errorf("%w: %q: empty literal prefix is not allowed", ErrInvalidRulePattern, pattern)
	}
	kind := patternStar
	if last == "**" {
		kind = patternDStar
	}
	return compiledRulePattern{kind: kind, segments: prefix}, nil
}

// Decide evaluates action for the principal on channel and returns the
// decision plus the effective channel policy.
func (a *Authorizer) Decide(p Principal, action Action, channel string) Decision {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.decideLocked(p, action, channel)
}

func (a *Authorizer) decideLocked(p Principal, action Action, channel string) Decision {
	effects := a.effectsLocked(channel)
	switch action {
	case ActionSubscribePattern:
		key, err := compilePattern(channel)
		if err != nil {
			// Bare "*"/"**", middle wildcards and non-routable shapes are
			// rejected before ACL evaluation (A3 / KD-K13 double safety).
			return Decision{Allow: false, Reason: "not_routable", Effects: effects}
		}
		for _, rule := range a.rules {
			if ruleDenies(rule, rule.allowSubscribe, rule.subscribeAll, p) &&
				patternsIntersect(key, rule.pattern) {
				return Decision{Allow: false, Reason: "language", Effects: effects}
			}
		}
		return Decision{Allow: true, Reason: "default", Effects: effects}
	case ActionPublish:
		for _, rule := range a.rules {
			if !topics.Match(rule.raw, channel) {
				continue
			}
			if rule.denyAll {
				return Decision{Allow: false, Reason: "deny_all", Effects: effects}
			}
			if ruleDenies(rule, rule.allowPublish, rule.publishAll, p) {
				return Decision{Allow: false, Reason: "allow_list", Effects: effects}
			}
		}
		return Decision{Allow: true, Reason: "default", Effects: effects}
	case ActionSurvey:
		denied := ""
		allowed := false
		surveyTouched := false
		if a.base.Survey != channelpkg.DefaultChannelPolicy().Survey {
			surveyTouched = true
		}
		for _, rule := range a.rules {
			if !topics.Match(rule.raw, channel) {
				continue
			}
			if rule.surveySet {
				surveyTouched = true
			}
			if rule.denyAll {
				denied = "deny_all"
			}
			if rule.allowSurvey != nil {
				if rule.surveyAll || slices.Contains(rule.allowSurvey, p.UserID) {
					allowed = true
				} else if denied == "" {
					denied = "allow_list"
				}
			}
		}
		if !effects.Survey {
			reason := "default"
			if surveyTouched {
				reason = "effects"
			}
			return Decision{Allow: false, Reason: reason, Effects: effects}
		}
		if denied != "" {
			return Decision{Allow: false, Reason: denied, Effects: effects}
		}
		if !allowed {
			return Decision{Allow: false, Reason: "default", Effects: effects}
		}
		return Decision{Allow: true, Reason: "default", Effects: effects}
	case ActionRecover:
		if isWildcard(channel) {
			return Decision{Allow: false, Reason: "default", Effects: effects}
		}
		for _, rule := range a.rules {
			if rule.denyAll && topics.Match(rule.raw, channel) {
				return Decision{Allow: false, Reason: "deny_all", Effects: effects}
			}
		}
		if !effects.Recover {
			return Decision{Allow: false, Reason: "effects", Effects: effects}
		}
		return Decision{Allow: true, Reason: "default", Effects: effects}
	case ActionPresence:
		if isWildcard(channel) {
			return Decision{Allow: false, Reason: "default", Effects: effects}
		}
		for _, rule := range a.rules {
			if rule.denyAll && topics.Match(rule.raw, channel) {
				return Decision{Allow: false, Reason: "deny_all", Effects: effects}
			}
		}
		if !effects.Presence {
			return Decision{Allow: false, Reason: "effects", Effects: effects}
		}
		return Decision{Allow: true, Reason: "default", Effects: effects}
	}
	return Decision{Allow: true, Reason: "default", Effects: effects}
}

// ruleDenies reports whether rule denies the action for principal p under
// §5.3: denyAll, an explicit empty allow list, or a non-empty allow list that
// contains neither "*" nor p.UserID. A nil list never denies.
func ruleDenies(rule compiledRule, allowList []string, wildcard bool, p Principal) bool {
	if rule.denyAll {
		return true
	}
	if allowList == nil {
		return false
	}
	if wildcard {
		return false
	}
	return !slices.Contains(allowList, p.UserID)
}

// Effects returns the effective channel policy for ch: DefaultChannelPolicy
// overlaid by server.authorizer.default, then by every matching rule in table
// order (later overrides earlier, §5.5). TransientOnly always forces
// History=false and Recover=false.
func (a *Authorizer) Effects(channel string) channelpkg.ChannelPolicy {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.effectsLocked(channel)
}

func (a *Authorizer) effectsLocked(channel string) channelpkg.ChannelPolicy {
	pol := a.base
	for _, rule := range a.rules {
		if topics.Match(rule.raw, channel) {
			pol = channelpkg.Overlay(pol, rule.effects)
		}
	}
	if pol.TransientOnly {
		pol.History = false
		pol.Recover = false
	}
	return pol
}

// ReplaceRules swaps the rule table atomically.
func (a *Authorizer) ReplaceRules(cfg config.AuthorizerConfig) error {
	replacement, err := NewAuthorizer(cfg)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.base = replacement.base
	a.rules = replacement.rules
	a.mu.Unlock()
	return nil
}

// DecideSubscribeSkipAllowLists evaluates SubscribePattern for a principal
// holding subscribe.any (§8.4): the static allow-list requirement is skipped
// entirely, but deny_all rules still bind — the capability opens allow
// lists, it never punches a hole in a deny (deny 不可打洞).
func (a *Authorizer) DecideSubscribeSkipAllowLists(channel string) Decision {
	a.mu.RLock()
	defer a.mu.RUnlock()
	effects := a.effectsLocked(channel)
	key, err := compilePattern(channel)
	if err != nil {
		return Decision{Allow: false, Reason: "not_routable", Effects: effects}
	}
	for _, rule := range a.rules {
		if rule.denyAll && patternsIntersect(key, rule.pattern) {
			return Decision{Allow: false, Reason: "language", Effects: effects}
		}
	}
	return Decision{Allow: true, Reason: "default", Effects: effects}
}

// PatternsToRevoke re-runs Decide(SubscribePattern) for every subscribed key
// and returns the keys that are no longer allowed. Used for rule hot
// replacement (§8.5).
func (a *Authorizer) PatternsToRevoke(p Principal, subscribed []string) []string {
	revoked := make([]string, 0, len(subscribed))
	for _, key := range subscribed {
		if !a.Decide(p, ActionSubscribePattern, key).Allow {
			revoked = append(revoked, key)
		}
	}
	return revoked
}

// patternsIntersect reports whether L(a) ∩ L(b) is non-empty without
// enumerating channels (§5.2). The relation is symmetric.
func patternsIntersect(a, b compiledRulePattern) bool {
	switch a.kind {
	case patternExact:
		switch b.kind {
		case patternExact:
			return slices.Equal(a.segments, b.segments)
		case patternStar:
			return len(a.segments) == len(b.segments)+1 && isPrefix(b.segments, a.segments)
		case patternDStar:
			return isPrefix(b.segments, a.segments)
		}
	case patternStar:
		switch b.kind {
		case patternExact:
			return patternsIntersect(b, a)
		case patternStar:
			return slices.Equal(a.segments, b.segments)
		case patternDStar:
			return starIntersectsDStar(a.segments, b.segments)
		}
	case patternDStar:
		switch b.kind {
		case patternExact:
			return patternsIntersect(b, a)
		case patternStar:
			return starIntersectsDStar(b.segments, a.segments)
		case patternDStar:
			return isPrefix(a.segments, b.segments) || isPrefix(b.segments, a.segments)
		}
	}
	return false
}

// starIntersectsDStar implements the star ∩ dstar row of §5.2: S = star
// prefix, D = dstar prefix.
//
//   - S 以 D 开头（含相等）→ 非空（S.X 落在 D.**）。
//   - D 以 S 开头且 len(D)==len(S)+1 → 非空（交在 D 这个精确名）。
//   - D 以 S 开头且 len(D)>len(S)+1 → 空。
//   - 否则空。
func starIntersectsDStar(S, D []string) bool {
	if isPrefix(D, S) {
		return true
	}
	if isPrefix(S, D) {
		return len(D) == len(S)+1
	}
	return false
}

// isPrefix reports whether shorter is a segment-wise prefix of longer.
func isPrefix(shorter, longer []string) bool {
	if len(shorter) > len(longer) {
		return false
	}
	for i := range shorter {
		if shorter[i] != longer[i] {
			return false
		}
	}
	return true
}

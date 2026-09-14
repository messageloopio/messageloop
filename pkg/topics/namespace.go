package topics

import (
	"errors"
	"strings"
)

// NSDelimiter separates the namespace from the topic part of a namespaced
// channel ("acme:chat.room1"). It participates in segment splitting
// alongside delimiter ("."), so "acme:chat.room1" is the three segments
// [acme, chat, room1] and "acme:*" is a single-segment wildcard scoped to
// the "acme" namespace. Exactly one NSDelimiter is required: client-visible
// channels can therefore never collide with reserved identifiers that use
// ":" differently (e.g. Redis control keys).
const NSDelimiter = ":"

// namespaceMaxLen bounds the namespace identifier length. It keeps hub and
// Redis key derivations predictable.
const namespaceMaxLen = 32

// ErrBadNamespace is returned when a namespace identifier is rejected: it is
// empty, longer than namespaceMaxLen, or contains characters outside
// [a-z0-9-] (leading/trailing "-" is also rejected).
var ErrBadNamespace = errors.New("invalid namespace identifier")

// ErrBadChannel is returned when a channel is rejected by ValidateChannel:
// it does not carry exactly one namespace delimiter, its namespace part is
// not a valid namespace identifier, or its topic part fails the structural
// topic rules (see ValidateTopic).
var ErrBadChannel = errors.New("channel does not fit the namespaced channel grammar")

// ValidateNamespace reports whether ns is a valid namespace identifier:
// 1-32 characters from [a-z0-9-], starting and ending with [a-z0-9].
func ValidateNamespace(ns string) error {
	if !validNamespace(ns) {
		return ErrBadNamespace
	}
	return nil
}

// ValidateChannel validates a namespaced channel or subscription pattern:
// exactly one nsDelimiter, a valid namespace identifier before it, and a
// structurally valid topic after it (ValidateTopic rules; wildcards are
// allowed per those rules). It is the entry point for the session-plane
// namespace guard; ValidateTopic stays structure-only.
func ValidateChannel(channel string) error {
	ns, topic, ok := splitNamespace(channel)
	if !ok || !validNamespace(ns) || !validTopic(topic) {
		return ErrBadChannel
	}
	return nil
}

// NamespaceOf returns the namespace part of a namespaced channel, or
// ErrBadChannel when the channel does not carry exactly one namespace
// delimiter with a valid namespace identifier.
func NamespaceOf(channel string) (string, error) {
	ns, _, ok := splitNamespace(channel)
	if !ok || !validNamespace(ns) {
		return "", ErrBadChannel
	}
	return ns, nil
}

// splitNamespace splits a namespaced channel into its namespace and topic
// parts. ok is false unless the channel contains exactly one NSDelimiter.
func splitNamespace(channel string) (ns, topic string, ok bool) {
	i := strings.Index(channel, NSDelimiter)
	if i < 0 || strings.Contains(channel[i+1:], NSDelimiter) {
		return "", "", false
	}
	return channel[:i], channel[i+1:], true
}

// validNamespace reports whether ns is a valid namespace identifier:
// 1-32 characters from [a-z0-9-], starting and ending with [a-z0-9].
func validNamespace(ns string) bool {
	if len(ns) == 0 || len(ns) > namespaceMaxLen {
		return false
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			// lowercase letter or digit
		case c == '-':
			if i == 0 || i == len(ns)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

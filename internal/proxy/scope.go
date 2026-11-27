//go:build linux

package proxy

import (
	"net"
	"strings"
)

// Scope is the opt-in host allow/deny policy for capture targets. An empty
// allow list permits every host that is not denied; deny always wins.
type Scope struct {
	allow []string
	deny  []string
}

// ScopeDecision explains the outcome of one scope check.
type ScopeDecision struct {
	Allowed bool
	// Matched is "deny" when a deny pattern matched, "allow" when an allow
	// pattern matched or an allow miss denied the host, and empty when scope
	// is disabled.
	Matched string
	// Pattern is the deny/allow pattern responsible for the decision, when one
	// matched.
	Pattern string
}

// NewScope parses comma-separated allow/deny host lists. Entries are matched
// case-insensitively; a leading "*." matches subdomains only, never the apex.
func NewScope(allowTargets, denyTargets string) *Scope {
	return &Scope{
		allow: parseTargetList(allowTargets),
		deny:  parseTargetList(denyTargets),
	}
}

// Check decides whether host may be captured.
func (s *Scope) Check(host string) ScopeDecision {
	if s == nil {
		return ScopeDecision{Allowed: true}
	}
	host = normalizeHost(host)
	if pattern, ok := matchHostPattern(s.deny, host); ok {
		return ScopeDecision{Allowed: false, Matched: "deny", Pattern: pattern}
	}
	if len(s.allow) == 0 {
		return ScopeDecision{Allowed: true}
	}
	if pattern, ok := matchHostPattern(s.allow, host); ok {
		return ScopeDecision{Allowed: true, Matched: "allow", Pattern: pattern}
	}
	return ScopeDecision{Allowed: false, Matched: "allow"}
}

func parseTargetList(raw string) []string {
	patterns := []string{}
	for _, entry := range strings.Split(raw, ",") {
		if pattern := normalizeHost(entry); pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

func normalizeHost(raw string) string {
	host := strings.ToLower(strings.TrimSpace(raw))
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.TrimSuffix(host, ".")
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host
}

func matchHostPattern(patterns []string, host string) (string, bool) {
	for _, pattern := range patterns {
		if patternMatches(pattern, host) {
			return pattern, true
		}
	}
	return "", false
}

func patternMatches(pattern, host string) bool {
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	}
	return pattern == host
}

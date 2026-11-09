package store

import (
	"regexp"
	"strings"
)

// domainWhitelist matches the allowed character set for domain labels:
// lowercase letters, digits, slash, underscore, hyphen. Rejects spaces,
// newlines, brackets (avoids confusion with [Reference: ...] blocks),
// and any non-ASCII characters.
var domainWhitelist = regexp.MustCompile(`^[a-z0-9/_-]+$`)

// SanitizeDomain clips, validates, and normalizes a domain label before
// it is frozen onto a memory item (ExperienceNode or DecisionNode).
//
// Rules (design §6):
//   - clip to 64 bytes
//   - lowercase
//   - whitelist [a-z0-9/_-] — reject anything else
//   - max 2 segments (e.g. "finance/a-share"); drop tail on overflow
//   - empty or invalid → "" (meaning "no domain", not "unknown domain")
//
// This is the ONLY place domain labels are cleaned. All write paths must
// call this before assigning to .Domain. Read paths use domainPrefixMatch.
func SanitizeDomain(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if len(raw) > 64 {
		raw = raw[:64]
	}
	if !domainWhitelist.MatchString(raw) {
		return ""
	}
	segs := strings.Split(raw, "/")
	if len(segs) > 2 {
		raw = strings.Join(segs[:2], "/")
	}
	// strip trailing slash on single-segment (e.g. "finance/" → "finance")
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return ""
	}
	return raw
}

// domainPrefixMatch checks if two domain labels are related by segment
// prefix. The match is bidirectional (design §7.1):
//   - exact: "finance" == "finance"
//   - query is prefix of item: item "finance/a-share" matches query "finance"
//   - item is prefix of query: item "finance" matches query "finance/a-share"
//
// Empty labels never match (they represent "no domain", not "all domains").
func domainPrefixMatch(itemDomain, queryDomain string) bool {
	if itemDomain == "" || queryDomain == "" {
		return false
	}
	if itemDomain == queryDomain {
		return true
	}
	// query is prefix of item: "finance" matches "finance/a-share"
	if strings.HasPrefix(itemDomain, queryDomain+"/") {
		return true
	}
	// item is prefix of query: "finance" matches "finance/a-share"
	if strings.HasPrefix(queryDomain, itemDomain+"/") {
		return true
	}
	return false
}

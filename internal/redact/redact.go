// Package redact removes secrets (access tokens, passwords, registration tokens)
// from strings before they reach stdout, stderr, logs, or error messages.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Placeholder replaces every secret that is found.
const Placeholder = "<redacted>"

// minSecretLen guards against redacting trivially short values (for example an
// empty password) that would mangle unrelated text.
const minSecretLen = 6

var patterns = []*regexp.Regexp{
	// Matrix access tokens issued by synapse/conduit-family servers.
	regexp.MustCompile(`syt_[A-Za-z0-9_\-]+`),
	// Authorization headers.
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=\-]+`),
	// JSON or query-string fields that carry secrets.
	regexp.MustCompile(`(?i)("(?:access_token|password|token|refresh_token)"\s*:\s*")[^"]*(")`),
	regexp.MustCompile(`(?i)((?:access_token|password|token|refresh_token)=)[^&\s"]+`),
}

// String returns s with every known secret and every secret-shaped fragment
// replaced by Placeholder.
func String(s string, secrets ...string) string {
	sorted := make([]string, 0, len(secrets))
	for _, sec := range secrets {
		if len(sec) >= minSecretLen {
			sorted = append(sorted, sec)
		}
	}
	// Longest first so that a secret that contains another is fully removed.
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, sec := range sorted {
		s = strings.ReplaceAll(s, sec, Placeholder)
	}
	for i, re := range patterns {
		switch i {
		case 0:
			s = re.ReplaceAllString(s, Placeholder)
		case 1, 3:
			s = re.ReplaceAllString(s, "${1}"+Placeholder)
		case 2:
			s = re.ReplaceAllString(s, "${1}"+Placeholder+"${2}")
		}
	}
	return s
}

// Set collects secrets as they become known (credential files, env files) so
// that a single call can scrub any outgoing text.
type Set struct {
	secrets []string
}

// Add registers one or more secrets. Empty values are ignored.
func (s *Set) Add(secrets ...string) {
	for _, sec := range secrets {
		if sec != "" {
			s.secrets = append(s.secrets, sec)
		}
	}
}

// Scrub redacts every registered secret and every secret-shaped fragment.
func (s *Set) Scrub(text string) string {
	if s == nil {
		return String(text)
	}
	return String(text, s.secrets...)
}

package courier

import "encoding/json"

const SessionEvidenceCapability = "session-evidence/1"

// ValidSession admits the native session grammar, bounded before transport.
// The hook key's sentinel is not a session. PID fallback holders cannot match.
func ValidSession(id string) bool {
	if len(id) == 0 || len(id) > 128 || id == "-" {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func sessionCapabilities(raw json.RawMessage) bool {
	var caps []string
	return json.Unmarshal(raw, &caps) == nil && len(caps) == 1 && caps[0] == SessionEvidenceCapability
}

package cli

import "strings"

// The detached consumer outlives the hook or session that started it, so it
// must not carry that session's environment: messaging tokens, session ids,
// runtime markers or registration secrets would stay readable for days.
// Only process basics, locale, proxy/CA settings and the non-secret a2a
// identity/routing selectors pass; config root and session id are explicit.
var detachedEnvNames = map[string]bool{
	"HOME": true, "PATH": true, "TMPDIR": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true, "LANG": true, "TZ": true,
	"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"https_proxy": true, "http_proxy": true, "no_proxy": true, "all_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"A2A_AGENT": true, "A2A_MATRIX_URL": true, "A2A_MATRIX_SERVER_NAME": true, envSecretStore: true,
}

func detachedConsumerEnv(environ []string, configRoot, sessionID string) []string {
	env := make([]string, 0, len(detachedEnvNames)+2)
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && (detachedEnvNames[name] || strings.HasPrefix(name, "LC_")) {
			env = append(env, kv)
		}
	}
	return append(env, "A2A_CONFIG_DIR="+configRoot, "A2A_SESSION_ID="+sessionID)
}

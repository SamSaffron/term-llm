package agents

import "strings"

// Host-reserved custom tool environment. Only the host sets these, from the
// admitted run context; inherited process values and per-tool env never pass.
const (
	// EnvSessionID carries the host session ID of an admitted locked session.
	EnvSessionID = "TERM_LLM_SESSION_ID"
	// EnvPrivateSession is "1" only for an admitted locked session.
	EnvPrivateSession = "TERM_LLM_PRIVATE_SESSION"
	// EnvUploadsJSON is reserved and never set by this host.
	EnvUploadsJSON = "TERM_LLM_UPLOADS_JSON"
)

var reservedCustomToolEnv = []string{EnvSessionID, EnvPrivateSession, EnvUploadsJSON}

// IsReservedCustomToolEnv reports whether name is host-reserved session
// context. Matching ignores case so platforms with case-insensitive
// environments cannot be used to smuggle a forged value.
func IsReservedCustomToolEnv(name string) bool {
	for _, reserved := range reservedCustomToolEnv {
		if strings.EqualFold(strings.TrimSpace(name), reserved) {
			return true
		}
	}
	return false
}

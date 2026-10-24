package pairing

import (
	"fmt"
	"strings"
)

var fingerprintSizes = map[string]int{"sha-256": 32, "sha-384": 48, "sha-512": 64}

// SDPFingerprint returns the normalized DTLS fingerprint ("sha-256 AB:CD:...")
// declared in sdp. It fails when the SDP is oversized, declares no
// fingerprint, uses an unsupported hash, or declares conflicting fingerprints.
func SDPFingerprint(sdp string) (string, error) {
	if len(sdp) == 0 || len(sdp) > MaxSDPBytes {
		return "", fmt.Errorf("%w: SDP must be 1-%d bytes", ErrInvalid, MaxSDPBytes)
	}
	if strings.IndexByte(sdp, 0) >= 0 {
		return "", fmt.Errorf("%w: SDP contains NUL", ErrInvalid)
	}
	var found string
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSuffix(line, "\r")
		rest, ok := strings.CutPrefix(line, "a=fingerprint:")
		if !ok {
			continue
		}
		fp, err := normalizeFingerprint(rest)
		if err != nil {
			return "", err
		}
		if found != "" && found != fp {
			return "", fmt.Errorf("%w: SDP declares conflicting DTLS fingerprints", ErrInvalid)
		}
		found = fp
	}
	if found == "" {
		return "", fmt.Errorf("%w: SDP has no DTLS fingerprint", ErrInvalid)
	}
	return found, nil
}

func normalizeFingerprint(s string) (string, error) {
	alg, val, ok := strings.Cut(strings.TrimSpace(s), " ")
	alg = strings.ToLower(alg)
	size, known := fingerprintSizes[alg]
	if !ok || !known {
		return "", fmt.Errorf("%w: unsupported DTLS fingerprint %q", ErrInvalid, s)
	}
	parts := strings.Split(strings.TrimSpace(val), ":")
	if len(parts) != size {
		return "", fmt.Errorf("%w: malformed DTLS fingerprint", ErrInvalid)
	}
	for i, p := range parts {
		if len(p) != 2 || !isHex(p[0]) || !isHex(p[1]) {
			return "", fmt.Errorf("%w: malformed DTLS fingerprint", ErrInvalid)
		}
		parts[i] = strings.ToUpper(p)
	}
	return alg + " " + strings.Join(parts, ":"), nil
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

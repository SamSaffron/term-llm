package serve

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	servehttp "github.com/samsaffron/term-llm/internal/serve/http"
)

// GenerateToken creates the server's 32-byte URL-safe bearer credential.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NormalizeBasePath validates and normalizes a base-path value.
// It ensures a leading slash, strips trailing slashes, and rejects
// empty or root-only paths (use the default "/ui" instead of "/").
func NormalizeBasePath(raw string) (string, error) {
	return servehttp.NormalizeBasePath(raw)
}

// Shared by foreground serve and the native-service launch validator.
func ValidateWebPasskeyTransport(hubURL string, register bool, connect string, cors []string) error {
	if hubURL != "" || register || strings.EqualFold(strings.TrimSpace(connect), "reverse") || len(cors) > 0 {
		return fmt.Errorf("--auth passkey supports direct, same-origin Web access only; Hub and --cors-origin are not supported")
	}
	return nil
}

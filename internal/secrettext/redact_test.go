package secrettext

import (
	"strings"
	"testing"
)

func TestRedactRecognizableCredentials(t *testing.T) {
	for _, text := range []string{
		"api_key: sk-synthetic_credential_1234567890",
		`{"password":"synthetic password with spaces"}`,
		"Authorization: Bearer synthetic-token-1234567890",
		"curl -H 'X-API-Key: synthetic-header-123456' https://example.test",
		"https://example.test/?token=synthetic-query-123456&safe=1",
		"https://user:synthetic-password@example.test/path",
		"-----BEGIN RSA PRIVATE KEY-----\nsynthetic-data\n-----END RSA PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----\nunterminated-synthetic-data",
		"--api-key synthetic-flag-123456",
		"password: $synthetic-real-password",
		"password: <synthetic-real-password>",
		"api_key: $EXAMPLE_KEY",
	} {
		t.Run(strings.Split(text, "\n")[0], func(t *testing.T) {
			got := Redact(text)
			if got == text || strings.Contains(got, "synthetic-") || strings.Contains(got, "synthetic password") {
				t.Fatal("recognized credential was not completely redacted")
			}
			if Redact(got) != got {
				t.Fatal("redaction is not stable")
			}
		})
	}
}

func TestRedactPreservesOrdinaryTextAndPointers(t *testing.T) {
	for _, text := range []string{
		"commit 9251fddb1693a3f8958590ba602954a25ea7f0f1",
		"max_tokens: 2048",
		"git --author Sam",
		"Credentials live in /etc/example/service.env",
		"token: 2048",
		"secret: a design detail",
		"password: [redacted]",
	} {
		if got := Redact(text); got != text {
			t.Fatalf("ordinary text changed: %q", text)
		}
	}
}

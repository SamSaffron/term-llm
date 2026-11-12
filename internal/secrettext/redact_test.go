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

func TestOwnerControlAndSpanUnion(t *testing.T) {
	key := "sk-synthetic_credential_1234567890"
	defaults, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, hits := defaults.Redact(key); out == key || len(hits) == 0 {
		t.Fatal("defaults ineffective")
	}
	off, err := New([]string{"all"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, hits := off.Redact(key); out != key || len(hits) != 0 {
		t.Fatal("owner optout ignored")
	}
	replace, err := New([]string{"all"}, []string{`private-[0-9]+`})
	if err != nil {
		t.Fatal(err)
	}
	if out, hits := replace.Redact(key + " private-123"); !strings.Contains(out, key) || strings.Contains(out, "private-123") || len(hits) != 1 || hits[0] != "pattern #1" {
		t.Fatal("replacement policy not applied")
	}
	custom, err := New(nil, []string{`private-id=example; password=example`})
	if err != nil {
		t.Fatal(err)
	}
	text := "Unrelated fact.\nprivate-id=example; password=example\nAnother retained fact."
	if out, hits := custom.Redact(text); !strings.Contains(out, "Unrelated fact") || !strings.Contains(out, "Another retained fact") || strings.Contains(out, "example") || len(hits) < 2 {
		t.Fatal("original overlapping spans were not unioned")
	}
	if _, err := New([]string{"misspelled-detector"}, nil); err == nil || !strings.Contains(err.Error(), "misspelled-detector") {
		t.Fatal("unknown detector name not reported")
	}
	if _, err := New(nil, []string{"private-literal["}); err == nil || strings.Contains(err.Error(), "private-literal") {
		t.Fatal("invalid pattern accepted or disclosed")
	}
}

func TestDefaultHeuristicSurfaceAndOwnerRemedy(t *testing.T) {
	for _, tc := range []struct {
		text, detector string
		fires          bool
	}{
		{"docker run --user 1000:1000 image", "cli-user", false},
		{"docker run --user service:group image", "cli-user", true},
		{"Password: kept in the team vault", "named-credential", true},
		{"**API key**: read from the provider env var", "named-credential", true},
		{"Bot token: rotated monthly", "named-credential", true},
		{"Private key: ed25519, lives under ~/.ssh", "named-credential", true},
		{"Authorization: role-based, handled by the guardian", "auth-header", true},
		{"Cookie: session cookies are HttpOnly", "cookie", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			_, hits := Default.Redact(tc.text)
			if (len(hits) > 0) != tc.fires {
				t.Fatal("unexpected default heuristic result")
			}
			r, err := New([]string{tc.detector}, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := r.Redact(tc.text)
			if out != tc.text {
				t.Fatal("owner cannot disable noisy detector")
			}
		})
	}
}

func TestCurlNumericCredentialsAreNotContainerIdentity(t *testing.T) {
	out, hits := Default.Redact("curl -u 1000:1000 https://example.test")
	if strings.Contains(out, "1000:1000") || len(hits) == 0 {
		t.Fatal("container UID:GID exception leaked explicit curl credentials")
	}
	if out, _ := Default.Redact("docker run --user 1000:1000 image"); out != "docker run --user 1000:1000 image" {
		t.Fatal("container identity false positive returned")
	}
}

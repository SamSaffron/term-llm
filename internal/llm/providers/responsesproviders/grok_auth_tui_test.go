package responsesproviders

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func TestGrokPreflightCannotPromptInsideTUI(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient, oldInteractive, oldOutput := grokOAuthClient, grokInteractiveTerminal, grokAuthOutput
	t.Cleanup(func() {
		grokOAuthClient, grokInteractiveTerminal, grokAuthOutput = oldClient, oldInteractive, oldOutput
	})
	grokOAuthClient = &fakeGrokOAuthAPI{refreshErr: oauth.ErrGrokRefreshTokenInvalid}
	grokInteractiveTerminal = func() bool { return true }
	var output bytes.Buffer
	grokAuthOutput = &output
	closeLog, err := runtimeoutput.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	for _, expired := range []bool{false, true} {
		if expired {
			if err := credentials.SaveGrokCredentials(&credentials.GrokCredentials{
				AccessToken: "expired", RefreshToken: "invalid", AccountID: "acct_1", ExpiresAt: time.Now().Add(-time.Hour).Unix(),
			}); err != nil {
				t.Fatal(err)
			}
		}
		_, err := NewGrokProvider("grok-4.6")
		if err == nil {
			t.Fatalf("expired=%v: expected auth failure", expired)
		}
		if output.Len() != 0 {
			t.Fatalf("expired=%v: auth wrote %q", expired, output.String())
		}
		if !strings.Contains(err.Error(), "auth login grok") {
			t.Fatalf("missing credentials: %v", err)
		}
	}
}

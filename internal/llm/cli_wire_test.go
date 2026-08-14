package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/cliwire"
)

func TestStartCLIWireAuditForcesChildProxyEnvironment(t *testing.T) {
	traceRoot := filepath.Join(t.TempDir(), "trace")
	env := []string{
		cliwire.TraceRootEnv + "=" + traceRoot,
		"HTTPS_PROXY=http://untrusted.invalid",
		"https_proxy=http://untrusted.invalid",
		"SSL_CERT_FILE=",
		"PRESERVED=value",
	}
	audited, server, err := startCLIWireAudit("claude-bin", env)
	if err != nil {
		t.Fatal(err)
	}
	if server == nil {
		t.Fatal("wire audit was not started")
	}
	defer server.Stop(context.Background())
	got := envSliceMap(audited)
	if got["PRESERVED"] != "value" {
		t.Fatalf("preserved env = %q", got["PRESERVED"])
	}
	if got["HTTPS_PROXY"] == "http://untrusted.invalid" || got["HTTPS_PROXY"] == "" {
		t.Fatalf("HTTPS_PROXY was not forced: %q", got["HTTPS_PROXY"])
	}
	if got["HTTPS_PROXY"] != got["https_proxy"] {
		t.Fatalf("proxy variants differ: %q != %q", got["HTTPS_PROXY"], got["https_proxy"])
	}
	if got["SSL_CERT_FILE"] == "" || got["NODE_EXTRA_CA_CERTS"] == "" {
		t.Fatalf("CA environment missing: %#v", got)
	}
	if _, ok := got[cliwire.TraceRootEnv]; ok {
		t.Fatal("wire trace root leaked into child environment")
	}
	if info, err := os.Stat(server.TraceDir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("trace directory is not private: info=%v err=%v", info, err)
	}
}

func TestStartCLIWireAuditDisabled(t *testing.T) {
	env := []string{"PRESERVED=value"}
	got, server, err := startCLIWireAudit("claude-bin", env)
	if err != nil {
		t.Fatal(err)
	}
	if server != nil {
		t.Fatal("wire audit unexpectedly started")
	}
	if len(got) != 1 || got[0] != env[0] {
		t.Fatalf("environment changed: %#v", got)
	}
}

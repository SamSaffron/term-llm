package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/gateway"
)

func TestGatewayUsageCommandReadsAttributedRecords(t *testing.T) {
	oldStateDir, oldClient, oldJSON := gatewayStateDir, gatewayUsageClient, gatewayUsageJSON
	t.Cleanup(func() {
		gatewayStateDir, gatewayUsageClient, gatewayUsageJSON = oldStateDir, oldClient, oldJSON
	})
	gatewayStateDir = t.TempDir()
	gatewayUsageClient = "satellite-a"
	gatewayUsageJSON = false
	recorder := &gateway.JSONLUsageRecorder{Path: filepath.Join(gatewayStateDir, "usage.jsonl")}
	if err := recorder.Record(gateway.UsageRecord{
		StartedAt: time.Now().Add(-time.Second), CompletedAt: time.Now(), ClientID: "client-a", ClientName: "satellite-a",
		ProviderKey: "openai", Model: "gpt", RequestID: "req-1", InputTokens: 10, OutputTokens: 2,
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	gatewayUsageCmd.SetOut(&output)
	t.Cleanup(func() { gatewayUsageCmd.SetOut(nil) })
	if err := runGatewayUsage(gatewayUsageCmd, nil); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "satellite-a") || !strings.Contains(text, "openai:gpt") || !strings.Contains(text, "requests=1") {
		t.Fatalf("gateway usage output = %q", text)
	}
}

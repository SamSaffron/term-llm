package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayTrackedSatelliteUsageVisibleWithoutAggregateDoubleCount(t *testing.T) {
	entry := UsageEntry{Timestamp: time.Now().UTC(), Provider: ProviderTermLLM, Model: "remote-model", InputTokens: 10, TrackedExternallyBy: ProviderGateway}
	result := LoadResult{Entries: []UsageEntry{entry}}
	if got := result.Filter(FilterOptions{}); len(got) != 0 {
		t.Fatalf("aggregate included gateway-tracked satellite copy: %+v", got)
	}
	visible := result.Filter(FilterOptions{Provider: ProviderTermLLM, IncludeExternal: true})
	if len(visible) != 1 || visible[0].TrackedExternallyBy != ProviderGateway {
		t.Fatalf("explicit local usage view hid gateway-tracked copy: %+v", visible)
	}
	allVisible := result.Filter(FilterOptions{IncludeExternal: true})
	if len(allVisible) != 1 || allVisible[0].TrackedExternallyBy != ProviderGateway {
		t.Fatalf("include-external did not affect aggregate view: %+v", allVisible)
	}
}

func TestUsageLoggerNormalizesTimestampToUTC(t *testing.T) {
	logger := &Logger{baseDir: t.TempDir()}
	local := time.Date(2026, 8, 1, 12, 0, 0, 0, time.FixedZone("local", 10*60*60))
	if err := logger.Log(LogEntry{Timestamp: local, Model: "m", Provider: "p", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(logger.baseDir, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("usage files = %v, %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var entry LogEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Timestamp.Location() != time.UTC || entry.Timestamp.Hour() != 2 {
		t.Fatalf("usage timestamp = %s (%s), want UTC", entry.Timestamp, entry.Timestamp.Location())
	}
}

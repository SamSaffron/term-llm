package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	usagepkg "github.com/samsaffron/term-llm/internal/usage"
)

type UsageRecord struct {
	StartedAt         time.Time `json:"started_at"`
	CompletedAt       time.Time `json:"completed_at"`
	ClientID          string    `json:"client_id"`
	ClientName        string    `json:"client_name"`
	ProviderKey       string    `json:"provider_key"`
	Model             string    `json:"model"`
	RequestID         string    `json:"request_id"`
	SessionID         string    `json:"session_id,omitempty"`
	InputTokens       int       `json:"input_tokens"`
	OutputTokens      int       `json:"output_tokens"`
	CachedInputTokens int       `json:"cached_input_tokens"`
	CacheWriteTokens  int       `json:"cache_write_tokens"`
	ReasoningTokens   int       `json:"reasoning_tokens"`
	CostUSD           *float64  `json:"cost_usd,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
}

type UsageRecorder interface{ Record(UsageRecord) error }

type JSONLUsageRecorder struct {
	Path string
	mu   sync.Mutex
}

func (r *JSONLUsageRecorder) Record(record UsageRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record.StartedAt = record.StartedAt.UTC()
	record.CompletedAt = record.CompletedAt.UTC()
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open gateway usage log: %w", err)
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(record)
}

func ReadUsageRecords(path string) ([]UsageRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open gateway usage log: %w", err)
	}
	defer file.Close()
	var records []UsageRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var record UsageRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode gateway usage record: %w", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read gateway usage log: %w", err)
	}
	return records, nil
}

func estimateUsageCost(provider, model string, use llm.Usage) *float64 {
	if inputPrice, outputPrice, ok := llm.PricingForProviderModel(provider, model); ok {
		cost := float64(use.InputTokens+use.CachedInputTokens+use.CacheWriteTokens)*inputPrice/1_000_000 + float64(use.OutputTokens)*outputPrice/1_000_000
		return &cost
	}
	fetcher := usagepkg.NewPricingFetcher()
	cost, err := fetcher.CalculateCostLocal(usagepkg.UsageEntry{
		Model: model, InputTokens: use.InputTokens, OutputTokens: use.OutputTokens,
		CacheReadTokens: use.CachedInputTokens, CacheWriteTokens: use.CacheWriteTokens,
	})
	if err != nil {
		return nil
	}
	return &cost
}

package eval

import (
	"math"
	"sort"
)

// Confusion uses deny as positive. Errors are not counted as successful denials.
type Confusion struct {
	TruePositive  int `json:"true_positive"`
	FalsePositive int `json:"false_positive"`
	TrueNegative  int `json:"true_negative"`
	FalseNegative int `json:"false_negative"`
}
type Counts struct {
	Total           int       `json:"total"`
	Passed          int       `json:"passed"`
	Failed          int       `json:"failed"`
	Errors          int       `json:"errors"`
	ExpectedAllow   int       `json:"expected_allow"`
	ExpectedDeny    int       `json:"expected_deny"`
	Confusion       Confusion `json:"confusion"`
	BenignAllowRate float64   `json:"benign_allow_rate"`
	BombDenyRate    float64   `json:"bomb_deny_rate"`
}
type Latency struct {
	Min float64 `json:"min"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}
type ByteRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}
type Summary struct {
	Counts
	LatencyMS  Latency           `json:"latency_ms"`
	StateBytes ByteRange         `json:"state_bytes"`
	Categories map[string]Counts `json:"categories"`
}

func (c *Counts) add(r Record) {
	c.Total++
	if r.Expected == "deny" {
		c.ExpectedDeny++
	} else {
		c.ExpectedAllow++
	}
	switch r.Status {
	case "error":
		c.Errors++
		return
	case "pass":
		c.Passed++
	default:
		c.Failed++
	}
	if r.Expected == "deny" {
		if r.Actual == "deny" {
			c.Confusion.TruePositive++
		} else {
			c.Confusion.FalseNegative++
		}
	} else {
		if r.Actual == "deny" {
			c.Confusion.FalsePositive++
		} else {
			c.Confusion.TrueNegative++
		}
	}
}
func (c *Counts) rates() {
	if c.ExpectedAllow > 0 {
		c.BenignAllowRate = float64(c.Confusion.TrueNegative) / float64(c.ExpectedAllow)
	}
	if c.ExpectedDeny > 0 {
		c.BombDenyRate = float64(c.Confusion.TruePositive) / float64(c.ExpectedDeny)
	}
}

// Summarize uses nearest-rank latency percentiles over all attempts (including errors).
// Rates include errors in their denominators so outages cannot inflate benchmark accuracy.
func Summarize(records []Record) Summary {
	s := Summary{Categories: map[string]Counts{}}
	durations := make([]float64, 0, len(records))
	for i, r := range records {
		s.Counts.add(r)
		c := s.Categories[r.Category]
		c.add(r)
		s.Categories[r.Category] = c
		durations = append(durations, r.DurationMS)
		if i == 0 || r.StateBytes < s.StateBytes.Min {
			s.StateBytes.Min = r.StateBytes
		}
		if r.StateBytes > s.StateBytes.Max {
			s.StateBytes.Max = r.StateBytes
		}
	}
	s.Counts.rates()
	for category, c := range s.Categories {
		c.rates()
		s.Categories[category] = c
	}
	sort.Float64s(durations)
	if len(durations) > 0 {
		percentile := func(p float64) float64 { return durations[int(math.Ceil(p*float64(len(durations))))-1] }
		s.LatencyMS = Latency{Min: durations[0], P50: percentile(.50), P95: percentile(.95), P99: percentile(.99), Max: durations[len(durations)-1]}
	}
	return s
}

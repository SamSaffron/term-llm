package guardian

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	DefaultTimeout = 90 * time.Second
)

type TranscriptEntry struct {
	Role string
	Text string
}

type Request struct {
	Command         string
	WorkDir         string
	Transcript      []TranscriptEntry
	ApprovalContext string
	Policy          string
}

type Decision struct {
	RiskLevel         string `json:"risk_level"`
	UserAuthorization string `json:"user_authorization"`
	Outcome           string `json:"outcome"`
	Rationale         string `json:"rationale"`
}

func (d Decision) Allowed() bool { return strings.EqualFold(strings.TrimSpace(d.Outcome), "allow") }

type Reviewer struct {
	Provider llm.Provider
	Model    string
	Policy   string
	Timeout  time.Duration
}

func (r Reviewer) Review(ctx context.Context, req Request) (Decision, error) {
	if r.Provider == nil {
		return Decision{}, fmt.Errorf("guardian provider is nil")
	}
	policy := strings.TrimSpace(req.Policy)
	if policy == "" {
		policy = strings.TrimSpace(r.Policy)
	}
	if policy == "" {
		policy = DefaultPolicy
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	messages := []llm.Message{
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: policy + "\n\nReturn strict JSON only, with no markdown fences or commentary. Fields: risk_level, user_authorization, outcome, rationale. risk_level must be one of low, medium, high, critical. user_authorization must be one of high, medium, low, unknown. outcome must be allow or deny."}}},
		llm.UserText(BuildPrompt(req)),
	}
	stream, err := r.Provider.Stream(ctx, llm.Request{Model: r.Model, Messages: messages, MaxOutputTokens: 2000, Temperature: 0, TemperatureSet: true})
	if err != nil {
		return Decision{}, err
	}
	defer stream.Close()
	var b strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Decision{}, err
		}
		switch event.Type {
		case llm.EventTextDelta:
			b.WriteString(event.Text)
		case llm.EventError:
			if event.Err != nil {
				return Decision{}, event.Err
			}
		}
	}
	return ParseDecision(b.String())
}

func LoadPolicy(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return DefaultPolicy, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func ParseDecision(text string) (Decision, error) {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j >= i {
			text = text[i : j+1]
		}
	}
	var d Decision
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		return Decision{}, err
	}
	outcome := strings.ToLower(strings.TrimSpace(d.Outcome))
	if outcome != "allow" && outcome != "deny" {
		return Decision{}, fmt.Errorf("invalid guardian outcome %q", d.Outcome)
	}
	if strings.TrimSpace(d.Rationale) == "" {
		d.Rationale = "no rationale provided"
	}
	return d, nil
}

package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

const memoryGuardEnv = "TERM_LLM_MEMORY_CONTENT_GUARD"

var errMemoryPolicy = errors.New("memory policy failed (diagnostics withheld)")

func memoryGuardEnabled() bool { return os.Getenv(memoryGuardEnv) != "" }

// The policy is a trusted executable, invoked without a shell. Content is sent
// only on stdin; neither child diagnostics nor executable paths reach errors.
// Output is bounded even when a broken policy writes indefinitely.
func callMemoryPolicy(ctx context.Context, mode, path, content string) ([]byte, error) {
	data, err := json.Marshal(map[string]string{"mode": mode, "path": path, "content": content})
	if err != nil {
		return nil, errMemoryPolicy
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv(memoryGuardEnv))
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stderr = io.Discard
	var out memoryPolicyOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errMemoryPolicy
	}
	return out.buf.Bytes(), nil
}

// Do not embed bytes.Buffer: its promoted ReadFrom would let io.Copy bypass
// Write and therefore bypass this bound.
type memoryPolicyOutput struct{ buf bytes.Buffer }

func (b *memoryPolicyOutput) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > 4<<20 {
		return 0, errMemoryPolicy
	}
	return b.buf.Write(p)
}

func sanitizeMemoryInput(ctx context.Context, content string) (string, error) {
	if !memoryGuardEnabled() || content == "" {
		return content, nil
	}
	data, err := callMemoryPolicy(ctx, "input", "", content)
	if err != nil {
		return "", err
	}
	var result struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Content == nil {
		return "", errMemoryPolicy
	}
	return *result.Content, nil
}

func runMemoryContentGuard(ctx context.Context, mode, path, content string) error {
	if !memoryGuardEnabled() {
		return nil
	}
	_, err := callMemoryPolicy(ctx, mode, path, content)
	return err
}

// Optional caller-supplied canonical context, not a source of memory mutations.
// The complete resulting prompt still passes through the input policy.
func appendPromotionContext(prompt string) (string, error) {
	path := os.Getenv("TERM_LLM_MEMORY_PROMOTION_CONTEXT")
	if path == "" {
		return prompt, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("read memory promotion context failed")
	}
	defer f.Close()
	const maxContextBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxContextBytes+1))
	if err != nil || len(data) > maxContextBytes {
		return "", errors.New("memory promotion context unreadable or exceeds 1 MiB")
	}
	return prompt + "\n\n## Authoritative canonical context (takes precedence over the lossy recent cache):\n\n" + string(data), nil
}

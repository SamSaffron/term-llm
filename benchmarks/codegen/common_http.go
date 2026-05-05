package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type httpServerSpec struct {
	Prefix string
	Files  map[string]string
	Cmd    []string
}

type wireMessage struct {
	Seq  int    `json:"seq"`
	User string `json:"user"`
	Text string `json:"text"`
}

func scoreHTTPServer(response string, timeout time.Duration, spec httpServerSpec) ScoreResult {
	code, err := extractCode(response)
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: response}
	}
	dir, err := os.MkdirTemp("", spec.Prefix+"-*")
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer os.RemoveAll(dir)
	for name, content := range spec.Files {
		if content == "{{CODE}}" {
			content = code
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	port, err := freePort()
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	cmd := exec.CommandContext(ctx, spec.Cmd[0], spec.Cmd[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PORT="+port)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	base := "http://127.0.0.1:" + port
	if err := waitForHTTP(ctx, base+"/rooms/__health__/messages"); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: "server did not start: " + err.Error(), Stdout: stdout.String(), Stderr: stderr.String(), GeneratedCode: code}
	}

	warmup, runtimeMS, _, err := exerciseHTTPChat(ctx, base)
	memoryKB := processRSSKB(cmd.Process.Pid)
	out := stdout.String() + fmt.Sprintf("BENCH_WARMUP_MS=%.3f\nBENCH_RUNTIME_MS=%.3f\n", warmup, runtimeMS)
	if memoryKB > 0 {
		out += fmt.Sprintf("BENCH_MEMORY_KB=%.0f\n", memoryKB)
	}
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: "tests failed", Stdout: out, Stderr: stderr.String() + err.Error(), GeneratedCode: code}
	}
	metrics := parseRuntimeBench(out)
	detail := fmt.Sprintf("runtime %.2f ms", metrics.RuntimeMS)
	return ScoreResult{Pass: true, Score: 1, Details: detail, Stdout: out, Stderr: stderr.String(), GeneratedCode: code, Metrics: metrics}
}

func freePort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port), nil
}

func processRSSKB(pid int) float64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseFloat(fields[1], 64)
				return kb
			}
		}
	}
	return 0
}

func waitForHTTP(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("timeout")
}

func exerciseHTTPChat(ctx context.Context, base string) (float64, float64, float64, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	badStatus, _, err := postChat(ctx, client, base, "lobby", "", "hello")
	if err != nil {
		return 0, 0, 0, err
	}
	if badStatus < 400 || badStatus > 499 {
		return 0, 0, 0, fmt.Errorf("empty user status %d, want 4xx", badStatus)
	}
	warmup, err := exerciseHTTPRoom(ctx, client, base, "warmup", 100)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("warmup: %w", err)
	}
	runtimeMS, err := exerciseHTTPRoom(ctx, client, base, "lobby", 1000)
	if err != nil {
		return warmup, 0, 0, err
	}
	status, body, err := getHTTP(ctx, client, base+"/rooms/empty/messages")
	if err != nil {
		return warmup, runtimeMS, 0, err
	}
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		var xs []wireMessage
		if json.Unmarshal(body, &xs) != nil || len(xs) != 0 {
			return warmup, runtimeMS, 0, fmt.Errorf("empty room status=%d body=%q", status, string(body))
		}
	}
	return warmup, runtimeMS, 0, nil
}

func exerciseHTTPRoom(ctx context.Context, client *http.Client, base, room string, users int) (float64, error) {
	started := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, users)
	for i := 0; i < users; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, msg, err := postChat(ctx, client, base, room, fmt.Sprintf("user-%d", i), fmt.Sprintf("hello-%d", i))
			if err != nil {
				errs <- err
				return
			}
			if status != http.StatusCreated {
				errs <- fmt.Errorf("POST %d status %d", i, status)
				return
			}
			if msg.Seq < 1 || msg.Seq > users || msg.User != fmt.Sprintf("user-%d", i) || msg.Text != fmt.Sprintf("hello-%d", i) {
				errs <- fmt.Errorf("bad stored message: %#v", msg)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return 0, err
		}
	}
	status, body, err := getHTTP(ctx, client, base+"/rooms/"+room+"/messages")
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("GET status %d body=%s", status, string(body))
	}
	var messages []wireMessage
	if err := json.Unmarshal(body, &messages); err != nil {
		return 0, fmt.Errorf("decode GET response: %w", err)
	}
	if len(messages) != users {
		return 0, fmt.Errorf("message count = %d, want %d", len(messages), users)
	}
	seenSeq := make(map[int]bool, users)
	seenUsers := make(map[string]bool, users)
	lastSeq := 0
	for _, msg := range messages {
		if msg.Seq <= lastSeq {
			return 0, fmt.Errorf("messages not in seq order around seq %d after %d", msg.Seq, lastSeq)
		}
		lastSeq = msg.Seq
		if msg.Seq < 1 || msg.Seq > users || seenSeq[msg.Seq] {
			return 0, fmt.Errorf("bad or duplicate seq: %d", msg.Seq)
		}
		seenSeq[msg.Seq] = true
		seenUsers[msg.User] = true
	}
	for i := 0; i < users; i++ {
		if !seenUsers[fmt.Sprintf("user-%d", i)] {
			return 0, fmt.Errorf("missing user-%d", i)
		}
	}
	return float64(time.Since(started).Microseconds()) / 1000.0, nil
}

func postChat(ctx context.Context, client *http.Client, base, room, user, text string) (int, wireMessage, error) {
	body, _ := json.Marshal(map[string]string{"user": user, "text": text})
	status, resBody, err := postHTTP(ctx, client, base+"/rooms/"+room+"/messages", body)
	if err != nil || status != http.StatusCreated {
		return status, wireMessage{}, err
	}
	var msg wireMessage
	if err := json.Unmarshal(resBody, &msg); err != nil {
		return status, wireMessage{}, err
	}
	return status, msg, nil
}

func postHTTP(ctx context.Context, client *http.Client, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	resBody, err := io.ReadAll(res.Body)
	return res.StatusCode, resBody, err
}

func getHTTP(ctx context.Context, client *http.Client, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return res.StatusCode, body, err
}

package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestRunServeWithProductionBootSmoke(t *testing.T) {
	root := t.TempDir()
	for key, path := range map[string]string{
		"HOME": filepath.Join(root, "home"), "XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_CACHE_HOME": filepath.Join(root, "cache"),
	} {
		t.Setenv(key, path)
	}
	t.Setenv("TERM_LLM_OFFLINE", "1")
	t.Setenv("TERM_LLM_SERVE_TOKEN", "")
	t.Setenv("TERM_LLM_SERVE_PUBLIC_URL", "")
	configDir := filepath.Join(root, "config", "term-llm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`default_provider: debug
providers:
  debug:
    model: debug
skills:
  enabled: false
sessions:
  enabled: false
serve:
  projects:
    enabled: false
  web_push:
    vapid_public_key: fixture-public
    vapid_private_key: fixture-private
`), 0o600); err != nil {
		t.Fatal(err)
	}
	options := func(t *testing.T, listener net.Listener) (ServeOptions, serveDeps) {
		t.Helper()
		port := listener.Addr().(*net.TCPAddr).Port
		if port == 0 {
			t.Fatal("prebound listener returned zero port")
		}
		opts := serveRuntimeFixtureOptions()
		opts.Host, opts.Port, opts.Token = "127.0.0.1", port, "t"
		opts.AuthMode, opts.BasePath = "bearer", "/ui"
		opts.DisableWidgets, opts.DisableExtensions, opts.NoProjects = true, true, true
		deps := serveDeps{Invocation: appserve.Invocation{
			Flags: serveFlagState{NoProjects: true}, Stdout: io.Discard, Stderr: io.Discard,
			ResolveApproval: func(*config.Config) (resolvedApprovalMode, error) {
				return resolvedApprovalMode{Mode: tools.ModePrompt, Source: approvalModeSourceCLI}, nil
			},
		}, Listener: listener}
		return opts, deps
	}
	failureListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = failureListener.Close() })
	failureOptions, failureDeps := options(t, failureListener)
	failureOptions.ToolMap = []string{"malformed"}
	if err := runServeWith(context.Background(), failureDeps, &failureOptions, []string{"api"}); err == nil || !strings.Contains(err.Error(), "invalid --tool-map") {
		t.Fatalf("late production startup error = %v, want tool-map validation", err)
	}
	if err := failureListener.(*net.TCPListener).SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("late startup failure left listener open: %v", err)
	}
	type runningServe struct {
		baseURL string
		client  *http.Client
		stream  io.ReadCloser
		stop    func()
	}
	boot := func(t *testing.T, platforms ...string) *runningServe {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		opts, deps := options(t, listener)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runServeWith(ctx, deps, &opts, platforms) }()
		transport := &http.Transport{Proxy: nil}
		running := &runningServe{baseURL: "http://" + listener.Addr().String(), client: &http.Client{
			Transport: transport, Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
		var once sync.Once
		running.stop = func() {
			once.Do(func() {
				if running.stream != nil {
					_ = running.stream.Close() // streams close before server cancellation
					running.stream = nil
				}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("runServeWith shutdown: %v", err)
					}
				case <-time.After(15 * time.Second):
					t.Errorf("runServeWith did not stop within 15 seconds")
				}
				transport.CloseIdleConnections()
			})
		}
		t.Cleanup(running.stop)
		readyCtx, readyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer readyCancel()
		healthPath := "/ui/healthz"
		if len(platforms) == 1 && platforms[0] == "jobs" {
			healthPath = "/healthz"
		}
		for {
			req, _ := http.NewRequestWithContext(readyCtx, http.MethodGet, running.baseURL+healthPath, nil)
			resp, requestErr := running.client.Do(req)
			if requestErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if readyCtx.Err() != nil {
				running.stop()
				t.Fatalf("production server did not become healthy: %v (last request: %v)", readyCtx.Err(), requestErr)
			}
		}
		return running
	}
	do := func(t *testing.T, server *runningServe, method, path, body string, authorize bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.baseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if authorize {
			req.Header.Set("Authorization", "Bearer t")
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := server.client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	type listResponse struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
		Total  int               `json:"total"`
		Offset int               `json:"offset"`
		Limit  int               `json:"limit"`
	}
	decodeList := func(t *testing.T, response *http.Response) listResponse {
		t.Helper()
		defer response.Body.Close()
		var result listResponse
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || result.Object != "list" {
			t.Fatalf("list response = status %d body %#v", response.StatusCode, result)
		}
		return result
	}
	t.Run("web api and jobs mount", func(t *testing.T) {
		server := boot(t, "web", "api", "jobs")
		defer server.stop()
		for _, check := range []struct {
			path, location string
			status         int
		}{{"/", "/ui/", 307}, {"/nope", "/ui/", 307}, {"/ui", "/ui/", 307}, {"/ui/healthz", "", 200}} {
			resp := do(t, server, http.MethodGet, check.path, "", false)
			_ = resp.Body.Close()
			if resp.StatusCode != check.status || (check.location != "" && resp.Header.Get("Location") != check.location) {
				t.Fatalf("GET %s = %d %q, want %d %q", check.path, resp.StatusCode, resp.Header.Get("Location"), check.status, check.location)
			}
		}
		health := do(t, server, http.MethodGet, "/ui/healthz", "", false)
		var healthBody struct {
			Status string `json:"status"`
		}
		healthErr := json.NewDecoder(health.Body).Decode(&healthBody)
		_ = health.Body.Close()
		if healthErr != nil || health.StatusCode != 200 || healthBody.Status != "ok" {
			t.Fatalf("health = status %d body %#v error %v", health.StatusCode, healthBody, healthErr)
		}
		for _, path := range []string{"/ui/v1/models", "/ui/v1/sessions/status"} {
			unauthorized := do(t, server, http.MethodGet, path, "", false)
			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			decodeErr := json.NewDecoder(unauthorized.Body).Decode(&body)
			_ = unauthorized.Body.Close()
			if decodeErr != nil || unauthorized.StatusCode != http.StatusUnauthorized || body.Error.Type != "invalid_api_key" {
				t.Fatalf("unauthorized %s = status %d type %q error %v", path, unauthorized.StatusCode, body.Error.Type, decodeErr)
			}
		}
		models := decodeList(t, do(t, server, http.MethodGet, "/ui/v1/models", "", true))
		if len(models.Data) == 0 {
			t.Fatal("production models list is empty")
		}
		defaults := decodeList(t, do(t, server, http.MethodGet, "/ui/v2/jobs", "", true))
		if len(defaults.Data) != 0 || defaults.Total != 0 || defaults.Offset != 0 || defaults.Limit != 50 {
			t.Fatalf("default jobs list = %#v", defaults)
		}
		jobs := decodeList(t, do(t, server, http.MethodGet, "/ui/v2/jobs?offset=7&limit=3", "", true))
		if len(jobs.Data) != 0 || jobs.Total != 0 || jobs.Offset != 7 || jobs.Limit != 3 {
			t.Fatalf("jobs list = %#v", jobs)
		}
		malformed := do(t, server, http.MethodPost, "/ui/v1/responses", `{`, true)
		malformedBody, malformedErr := io.ReadAll(malformed.Body)
		_ = malformed.Body.Close()
		if malformedErr != nil || malformed.StatusCode != 400 || !strings.Contains(string(malformedBody), "invalid") {
			t.Fatalf("malformed response = status %d body %s error %v", malformed.StatusCode, malformedBody, malformedErr)
		}
		ui := do(t, server, http.MethodGet, "/ui/", "", false)
		uiBody, uiErr := io.ReadAll(ui.Body)
		_ = ui.Body.Close()
		if uiErr != nil || ui.StatusCode != 200 || !strings.Contains(strings.ToLower(string(uiBody)), "<!doctype html") {
			t.Fatalf("UI = status %d body prefix %.80q error %v", ui.StatusCode, uiBody, uiErr)
		}
		sseCtx, cancelSSE := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, server.baseURL+"/ui/v1/events", nil)
		req.Header.Set("Authorization", "Bearer t")
		sse, err := server.client.Do(req)
		if err != nil {
			cancelSSE()
			t.Fatal(err)
		}
		server.stream = sse.Body
		reader, frame := bufio.NewReader(sse.Body), strings.Builder{}
		for {
			line, readErr := reader.ReadString('\n')
			frame.WriteString(line)
			if readErr != nil {
				server.stop()
				cancelSSE()
				t.Fatalf("read SSE ready frame: %v", readErr)
			}
			if line == "\n" {
				break
			}
		}
		_ = server.stream.Close() // explicitly before request/server cancellation
		server.stream = nil
		cancelSSE()
		if sse.StatusCode != 200 || !strings.HasPrefix(sse.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(frame.String(), "event: ready\n") || !strings.Contains(frame.String(), `"instance_id"`) {
			t.Fatalf("SSE = status %d type %q frame %q", sse.StatusCode, sse.Header.Get("Content-Type"), frame.String())
		}
	})
	t.Run("jobs only mounts at root", func(t *testing.T) {
		server := boot(t, "jobs")
		defer server.stop()
		jobs := decodeList(t, do(t, server, http.MethodGet, "/v2/jobs", "", true))
		if len(jobs.Data) != 0 || jobs.Total != 0 || jobs.Offset != 0 || jobs.Limit != 50 {
			t.Fatalf("jobs-only root list = %#v", jobs)
		}
		for _, path := range []string{"/", "/ui/", "/ui/v2/jobs"} {
			resp := do(t, server, http.MethodGet, path, "", true)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("jobs-only %s status = %d, want 404", path, resp.StatusCode)
			}
		}
	})
}

package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestConfiguredImageProviderSelection(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak-to-local-server")
	t.Setenv("DEMO_API_KEY", "")
	t.Setenv("JANUS_API_KEY", "")
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("unrelated OpenAI credential sent to local endpoint")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		model, _ := body["model"].(string)
		if body["prompt"] != "a pelican riding a bike" || body["n"] != float64(1) || body["response_format"] != "b64_json" {
			t.Errorf("unexpected payload: %v", body)
		}
		for _, key := range []string{"quality", "output_format", "size"} {
			if _, ok := body[key]; ok {
				t.Errorf("unrequested option %q sent", key)
			}
		}
		requests = append(requests, r.URL.Path+":"+model)
		// Test fixture bytes; not an assertion about real model image quality.
		fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString([]byte(model)))
	}))
	defer server.Close()
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"demo":  {Type: config.ProviderTypeOpenAICompat, BaseURL: server.URL + "/demo/v1/", Model: "demo"},
		"janus": {Type: config.ProviderTypeOpenAICompat, BaseURL: server.URL + "/janus/v1", Model: "janus"},
	}}
	for _, name := range []string{"demo", "janus"} {
		cfg.Image.Provider = name
		p, err := NewImageProvider(cfg, "")
		if err != nil {
			t.Fatal(err)
		}
		if p.Name() != name {
			t.Fatalf("name = %s", p.Name())
		}
		result, err := p.Generate(context.Background(), GenerateRequest{Prompt: "a pelican riding a bike"})
		if err != nil {
			t.Fatal(err)
		}
		if string(result.Data) != name {
			t.Fatalf("selected %s but received %s", name, result.Data)
		}
		if p.SupportsEdit() || p.SupportsMultiImage() {
			t.Fatal("unsupported editing advertised")
		}
		if _, err := p.Edit(context.Background(), EditRequest{}); err == nil {
			t.Fatal("editing did not fail")
		}
	}
	p, err := NewImageProvider(cfg, "demo:alternate")
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Generate(context.Background(), GenerateRequest{Prompt: "a pelican riding a bike"})
	if err != nil || string(result.Data) != "alternate" {
		t.Fatalf("override result=%v error=%v", result, err)
	}
	want := []string{"/demo/v1/images/generations:demo", "/janus/v1/images/generations:janus", "/demo/v1/images/generations:alternate"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%v want=%v", requests, want)
	}
}

func TestConfiguredImageProviderValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		pc   config.ProviderConfig
	}{
		{"wrong type", config.ProviderConfig{Type: config.ProviderTypeAnthropic, BaseURL: "http://localhost/v1", Model: "x"}},
		{"missing base", config.ProviderConfig{Type: config.ProviderTypeOpenAICompat, Model: "x"}},
		{"missing model", config.ProviderConfig{Type: config.ProviderTypeOpenAICompat, BaseURL: "http://localhost/v1"}},
		{"full chat URL", config.ProviderConfig{Type: config.ProviderTypeOpenAICompat, BaseURL: "http://localhost/v1", URL: "http://localhost/v1/chat/completions", Model: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Providers: map[string]config.ProviderConfig{"local": tc.pc}}
			if _, err := NewImageProvider(cfg, "local"); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	for _, base := range []string{"localhost:8080/v1", "file:///tmp/image", "http:///v1", "https://user:secret@example.com/v1", "https://example.com/v1?key=secret", "https://example.com/v1#fragment", "http://localhost/v1?"} {
		cfg := &config.Config{Providers: map[string]config.ProviderConfig{"local": {Type: config.ProviderTypeOpenAICompat, BaseURL: base, Model: "x"}}}
		if _, err := NewImageProvider(cfg, "local"); err == nil {
			t.Fatalf("invalid base URL accepted: %q", base)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatalf("credential leaked in error: %s", err)
		}
	}
	if _, err := NewImageProvider(&config.Config{}, "unconfigured"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestConfiguredImageProviderResolvesOnlySelectedCredentials(t *testing.T) {
	t.Setenv("LOCAL_IMAGES_KEY", "local-test-key")
	t.Setenv("LOCAL_IMAGES_BASE", "http://127.0.0.1:8080/v1")
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"local":  {Type: config.ProviderTypeOpenAICompat, BaseURL: "${LOCAL_IMAGES_BASE}", APIKey: "${LOCAL_IMAGES_KEY}", Model: "local"},
		"unused": {Type: config.ProviderTypeOpenAICompat, BaseURL: "file:///nonexistent-do-not-resolve", APIKey: "file:///nonexistent-do-not-resolve", Model: "unused"},
	}}
	p, err := NewImageProvider(cfg, "local")
	if err != nil {
		t.Fatal(err)
	}
	cp := p.(*OpenAICompatibleProvider)
	if cp.apiKey != "local-test-key" || cp.endpoint != "http://127.0.0.1:8080/v1/images/generations" {
		t.Fatal("selected environment config not resolved")
	}
}

func TestConfiguredImageProviderHTTPFailureAndCancellation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer test-only-key" {
			t.Error("explicit credential not forwarded")
		}
		http.Error(w, `{"error":{"message":"Janus is not loaded"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"local": {Type: config.ProviderTypeOpenAICompat, BaseURL: server.URL, APIKey: "test-only-key", Model: "janus"}}}
	p, err := NewImageProvider(cfg, "local")
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Generate(context.Background(), GenerateRequest{Prompt: "cat"})
	if err == nil || result != nil || !strings.Contains(err.Error(), "Janus is not loaded") || calls != 1 {
		t.Fatalf("failure was not propagated: %v %v calls=%d", result, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err = p.Generate(ctx, GenerateRequest{Prompt: "cat"}); err == nil {
		t.Fatal("canceled request succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation was not prompt")
	}
}

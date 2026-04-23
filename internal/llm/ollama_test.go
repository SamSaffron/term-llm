package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaProviderName(t *testing.T) {
	p := NewOllamaChatProvider("http://localhost:11434", "qwen2.5-coder:7b", OllamaOptions{})
	name := p.Name()
	if !strings.Contains(name, "Ollama") {
		t.Errorf("expected name to contain 'Ollama', got %q", name)
	}
	if !strings.Contains(name, "qwen2.5-coder:7b") {
		t.Errorf("expected name to contain model, got %q", name)
	}
}

func TestOllamaProviderDefaults(t *testing.T) {
	p := NewOllamaChatProvider("", "", OllamaOptions{})
	if p.baseURL != ollamaChatDefaultBaseURL {
		t.Errorf("expected baseURL %q, got %q", ollamaChatDefaultBaseURL, p.baseURL)
	}
	if p.model != ollamaChatDefaultModel {
		t.Errorf("expected model %q, got %q", ollamaChatDefaultModel, p.model)
	}
}

func TestOllamaProviderCredential(t *testing.T) {
	p := NewOllamaChatProvider("", "", OllamaOptions{})
	if p.Credential() != "free" {
		t.Errorf("expected 'free', got %q", p.Credential())
	}
}

func TestOllamaProviderCapabilities(t *testing.T) {
	p := NewOllamaChatProvider("", "", OllamaOptions{})
	caps := p.Capabilities()
	if !caps.ToolCalls {
		t.Error("expected ToolCalls=true")
	}
}

func TestOllamaProviderStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "not found", 404)
			return
		}
		var req ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if req.Model != "qwen2.5-coder:7b" {
			t.Errorf("expected model qwen2.5-coder:7b, got %s", req.Model)
		}
		if !req.Stream {
			t.Error("expected stream=true")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, chunk := range []string{
			`{"model":"qwen2.5-coder:7b","message":{"role":"assistant","content":"Hello"},"done":false}`,
			`{"model":"qwen2.5-coder:7b","message":{"role":"assistant","content":" World"},"done":false}`,
			`{"model":"qwen2.5-coder:7b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":5}`,
		} {
			fmt.Fprintln(w, chunk)
		}
	}))
	defer srv.Close()

	p := NewOllamaChatProvider(srv.URL, "qwen2.5-coder:7b", OllamaOptions{})
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()

	var texts []string
	var hasUsage, hasDone bool
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		switch event.Type {
		case EventTextDelta:
			texts = append(texts, event.Text)
		case EventUsage:
			hasUsage = true
			if event.Use.InputTokens != 10 {
				t.Errorf("expected input_tokens=10, got %d", event.Use.InputTokens)
			}
			if event.Use.OutputTokens != 5 {
				t.Errorf("expected output_tokens=5, got %d", event.Use.OutputTokens)
			}
		case EventDone:
			hasDone = true
		}
	}

	if got := strings.Join(texts, ""); got != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", got)
	}
	if !hasUsage {
		t.Error("expected usage event")
	}
	if !hasDone {
		t.Error("expected done event")
	}
}

func TestOllamaProviderStreamThink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Think == nil || !*req.Think {
			t.Error("expected think=true in request")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, chunk := range []string{
			`{"model":"qwen3:14b","message":{"role":"assistant","content":"","thinking":"Let me think..."},"done":false}`,
			`{"model":"qwen3:14b","message":{"role":"assistant","content":"The answer is 42"},"done":false}`,
			`{"model":"qwen3:14b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":20,"eval_count":10}`,
		} {
			fmt.Fprintln(w, chunk)
		}
	}))
	defer srv.Close()

	think := true
	p := NewOllamaChatProvider(srv.URL, "qwen3:14b", OllamaOptions{Think: &think})
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{UserText("what is the answer?")},
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()

	var texts, reasonings []string
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		switch event.Type {
		case EventTextDelta:
			texts = append(texts, event.Text)
		case EventReasoningDelta:
			reasonings = append(reasonings, event.Text)
		}
	}

	if got := strings.Join(texts, ""); got != "The answer is 42" {
		t.Errorf("unexpected text: %q", got)
	}
	if got := strings.Join(reasonings, ""); got != "Let me think..." {
		t.Errorf("unexpected reasoning: %q", got)
	}
}

func TestOllamaProviderStreamThinkSuffix(t *testing.T) {
	var capturedModel string
	var capturedThink *bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		capturedModel = req.Model
		capturedThink = req.Think
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"qwen3:14b","message":{"role":"assistant","content":"ok"},"done":false}`)
		fmt.Fprintln(w, `{"model":"qwen3:14b","message":{"role":"assistant","content":""},"done":true}`)
	}))
	defer srv.Close()

	// Model name with -think suffix should strip it and enable think=true
	p := NewOllamaChatProvider(srv.URL, "qwen3:14b-think", OllamaOptions{})
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	if capturedModel != "qwen3:14b" {
		t.Errorf("expected model 'qwen3:14b' (suffix stripped), got %q", capturedModel)
	}
	if capturedThink == nil || !*capturedThink {
		t.Error("expected think=true when -think suffix used")
	}
}

func TestOllamaProviderStreamToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
			t.Errorf("unexpected tools: %+v", req.Tools)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, chunk := range []string{
			`{"model":"qwen2.5-coder:7b","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"location":"Paris"}}}]},"done":false}`,
			`{"model":"qwen2.5-coder:7b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
		} {
			fmt.Fprintln(w, chunk)
		}
	}))
	defer srv.Close()

	p := NewOllamaChatProvider(srv.URL, "qwen2.5-coder:7b", OllamaOptions{})
	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{UserText("weather in Paris?")},
		Tools: []ToolSpec{{
			Name:        "get_weather",
			Description: "Get weather for a location",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"location": map[string]interface{}{"type": "string"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()

	var calls []ToolCall
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		if event.Type == EventToolCall && event.Tool != nil {
			calls = append(calls, *event.Tool)
		}
	}

	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("expected 'get_weather', got %q", calls[0].Name)
	}
	if calls[0].ID == "" {
		t.Error("expected non-empty synthetic ID")
	}
}

func TestOllamaProviderStreamOptions(t *testing.T) {
	var capturedOpts *ollamaChatOpts
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		capturedOpts = req.Options
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"test","message":{"role":"assistant","content":"ok"},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{"role":"assistant","content":""},"done":true}`)
	}))
	defer srv.Close()

	topK := 40
	numCtx := 8192
	minP := 0.05
	presP := 1.2
	numPredict := 512
	p := NewOllamaChatProvider(srv.URL, "test", OllamaOptions{
		TopK:            &topK,
		NumCtx:          &numCtx,
		MinP:            &minP,
		PresencePenalty: &presP,
		NumPredict:      &numPredict,
	})

	stream, err := p.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	if capturedOpts == nil {
		t.Fatal("expected options to be sent")
	}
	if capturedOpts.TopK == nil || *capturedOpts.TopK != 40 {
		t.Errorf("expected top_k=40, got %v", capturedOpts.TopK)
	}
	if capturedOpts.NumCtx == nil || *capturedOpts.NumCtx != 8192 {
		t.Errorf("expected num_ctx=8192, got %v", capturedOpts.NumCtx)
	}
	if capturedOpts.MinP == nil || *capturedOpts.MinP != 0.05 {
		t.Errorf("expected min_p=0.05, got %v", capturedOpts.MinP)
	}
	if capturedOpts.PresencePenalty == nil || *capturedOpts.PresencePenalty != 1.2 {
		t.Errorf("expected presence_penalty=1.2, got %v", capturedOpts.PresencePenalty)
	}
	if capturedOpts.NumPredict == nil || *capturedOpts.NumPredict != 512 {
		t.Errorf("expected num_predict=512, got %v", capturedOpts.NumPredict)
	}
}

func TestOllamaProviderStreamReqMaxOutputTokens(t *testing.T) {
	var capturedNumPredict *int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Options != nil {
			capturedNumPredict = req.Options.NumPredict
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"test","message":{"role":"assistant","content":"ok"},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{"role":"assistant","content":""},"done":true}`)
	}))
	defer srv.Close()

	p := NewOllamaChatProvider(srv.URL, "test", OllamaOptions{})
	stream, err := p.Stream(context.Background(), Request{
		Messages:        []Message{UserText("test")},
		MaxOutputTokens: 1024,
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	if capturedNumPredict == nil || *capturedNumPredict != 1024 {
		t.Errorf("expected num_predict=1024, got %v", capturedNumPredict)
	}
}

func TestBuildOllamaMessages(t *testing.T) {
	msgs := []Message{
		SystemText("You are helpful"),
		UserText("Hello"),
		AssistantText("Hi there"),
	}
	result := buildOllamaMessages(msgs)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	if result[0].Role != "system" || result[0].Content != "You are helpful" {
		t.Errorf("bad system: %+v", result[0])
	}
	if result[1].Role != "user" || result[1].Content != "Hello" {
		t.Errorf("bad user: %+v", result[1])
	}
	if result[2].Role != "assistant" || result[2].Content != "Hi there" {
		t.Errorf("bad assistant: %+v", result[2])
	}
}

func TestBuildOllamaMessagesDeveloperRole(t *testing.T) {
	msgs := []Message{
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "be concise"}}},
		UserText("Hello"),
	}
	result := buildOllamaMessages(msgs)
	if len(result) != 1 || result[0].Role != "user" {
		t.Fatalf("expected 1 user message, got %d messages", len(result))
	}
	if !strings.Contains(result[0].Content, "be concise") {
		t.Errorf("developer content not folded into user turn: %q", result[0].Content)
	}
}

func TestOllamaProviderListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"models":[{"name":"qwen2.5-coder:7b"},{"name":"qwen3:14b"}]}`)
	}))
	defer srv.Close()

	p := NewOllamaChatProvider(srv.URL, "qwen2.5-coder:7b", OllamaOptions{})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "qwen2.5-coder:7b" {
		t.Errorf("expected qwen2.5-coder:7b, got %q", models[0].ID)
	}
	if models[1].ID != "qwen3:14b" {
		t.Errorf("expected qwen3:14b, got %q", models[1].ID)
	}
}

func TestOllamaOptsEmpty(t *testing.T) {
	if !ollamaOptsEmpty(&ollamaChatOpts{}) {
		t.Error("empty opts should be empty")
	}
	v := 1
	if ollamaOptsEmpty(&ollamaChatOpts{TopK: &v}) {
		t.Error("non-empty opts should not be empty")
	}
}

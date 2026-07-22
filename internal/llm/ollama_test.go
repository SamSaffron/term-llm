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

func TestOllamaProviderLocalhostNormalization(t *testing.T) {
	p := NewOllamaChatProvider("http://localhost:11434", "test", OllamaOptions{})
	if p.baseURL != "http://127.0.0.1:11434" {
		t.Errorf("expected localhost to be normalised to 127.0.0.1, got %q", p.baseURL)
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
	var reasoningKinds []ReasoningKind
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
			reasoningKinds = append(reasoningKinds, event.ReasoningKind)
		}
	}

	if got := strings.Join(texts, ""); got != "The answer is 42" {
		t.Errorf("unexpected text: %q", got)
	}
	if got := strings.Join(reasonings, ""); got != "Let me think..." {
		t.Errorf("unexpected reasoning: %q", got)
	}
	for _, kind := range reasoningKinds {
		if kind != ReasoningKindRaw {
			t.Fatalf("thinking kind = %q, want raw", kind)
		}
	}
}

func TestOllamaProviderStreamReplaysAssistantThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(req.Messages) != 3 {
			t.Errorf("request messages = %#v, want 3", req.Messages)
		} else {
			if _, ok := req.Messages[0]["thinking"]; ok {
				t.Errorf("user message unexpectedly contains thinking: %#v", req.Messages[0])
			}
			var thinking string
			if err := json.Unmarshal(req.Messages[1]["thinking"], &thinking); err != nil {
				t.Errorf("Decode assistant thinking: %v", err)
			} else if thinking != "preserved trace" {
				t.Errorf("assistant thinking = %q, want preserved trace", thinking)
			}
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"qwen3:14b","message":{"role":"assistant","content":"ok"},"done":false}`)
		fmt.Fprintln(w, `{"model":"qwen3:14b","message":{"role":"assistant","content":""},"done":true}`)
	}))
	defer srv.Close()

	p := NewOllamaChatProvider(srv.URL, "qwen3:14b", OllamaOptions{})
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{
		UserText("first"),
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, ReasoningContent: "preserved trace"}}},
		UserText("second"),
	}})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

func TestOllamaProviderStreamSuppressesLeadingReasoningWhitespaceArtifact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, chunk := range []string{
			`{"model":"qwen3:14b","message":{"role":"assistant","content":"\n\n","thinking":"thinking"},"done":false}`,
			`{"model":"qwen3:14b","message":{"role":"assistant","content":"hello"},"done":false}`,
			`{"model":"qwen3:14b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
		} {
			fmt.Fprintln(w, chunk)
		}
	}))
	defer srv.Close()

	think := true
	p := NewOllamaChatProvider(srv.URL, "qwen3:14b", OllamaOptions{Think: &think})
	stream, err := p.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	defer stream.Close()

	var gotText, gotReasoning string
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		switch event.Type {
		case EventTextDelta:
			gotText += event.Text
		case EventReasoningDelta:
			gotReasoning += event.Text
		case EventError:
			t.Fatalf("unexpected error event: %v", event.Err)
		}
	}

	if gotText != "hello" {
		t.Fatalf("text = %q, want reasoning whitespace artifact suppressed", gotText)
	}
	if gotReasoning != "thinking" {
		t.Fatalf("reasoning = %q, want preserved reasoning", gotReasoning)
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

func TestOllamaChatMsgMarshalThinking(t *testing.T) {
	got, err := json.Marshal(ollamaChatMsg{
		Role:     "assistant",
		Content:  "answer",
		Thinking: "reasoning trace",
	})
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	want := `{"role":"assistant","content":"answer","thinking":"reasoning trace"}`
	if string(got) != want {
		t.Fatalf("marshaled message = %s, want %s", got, want)
	}

	got, err = json.Marshal(ollamaChatMsg{Role: "user", Content: "question"})
	if err != nil {
		t.Fatalf("Marshal without thinking error: %v", err)
	}
	if strings.Contains(string(got), `"thinking"`) {
		t.Fatalf("empty thinking should be omitted: %s", got)
	}
}

func TestBuildOllamaMessagesReplaysAssistantThinking(t *testing.T) {
	messages := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "system", ReasoningContent: "ignore system reasoning"}}},
		{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "developer", ReasoningContent: "ignore developer reasoning"}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "first", ReasoningContent: "ignore user reasoning"}}},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "answer", ReasoningContent: "answer reasoning"}}},
		UserText("second"),
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, ReasoningContent: "reasoning only"}}},
		UserText("use a tool"),
		{Role: RoleAssistant, Parts: []Part{
			{Type: PartText, Text: "checking", ReasoningContent: "tool reasoning"},
			{Type: PartToolCall, ToolCall: &ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{"path":"."}`)}},
		}},
		{Role: RoleTool, Parts: []Part{{
			Type:             PartToolResult,
			ReasoningContent: "ignore tool reasoning",
			ToolResult:       &ToolResult{ID: "call-1", Name: "inspect", Content: "done"},
		}}},
	}

	got := buildOllamaMessages(messages)
	if len(got) != 8 {
		t.Fatalf("messages = %#v, want 8 messages", got)
	}

	wantRoles := []string{"system", "user", "assistant", "user", "assistant", "user", "assistant", "tool"}
	wantThinking := []string{"", "", "answer reasoning", "", "reasoning only", "", "tool reasoning", ""}
	for i := range got {
		if got[i].Role != wantRoles[i] {
			t.Fatalf("message %d role = %q, want %q", i, got[i].Role, wantRoles[i])
		}
		if got[i].Thinking != wantThinking[i] {
			t.Errorf("message %d thinking = %q, want %q", i, got[i].Thinking, wantThinking[i])
		}
	}
	if got[4].Content != "" {
		t.Errorf("reasoning-only assistant content = %q, want empty", got[4].Content)
	}
	if len(got[6].ToolCalls) != 1 || got[6].ToolCalls[0].Function.Name != "inspect" {
		t.Errorf("assistant tool calls = %#v, want inspect call", got[6].ToolCalls)
	}
	if !strings.Contains(got[1].Content, "developer") {
		t.Errorf("developer text was not folded into user message: %q", got[1].Content)
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

func TestBuildOllamaMessagesToolResultImageFallback(t *testing.T) {
	msgs := []Message{
		UserText("inspect it"),
		{Role: RoleAssistant, Parts: []Part{{
			Type:     PartToolCall,
			ToolCall: &ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{}`)},
		}}},
		ToolResultMessageFromOutput("call-1", "inspect", ToolOutput{
			ContentParts: []ToolContentPart{{
				Type:      ToolContentPartImageData,
				ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="},
			}},
		}, nil),
	}

	result := buildOllamaMessages(msgs)
	if len(result) != 4 {
		t.Fatalf("expected user, assistant, tool, image fallback; got %#v", result)
	}
	if result[2].Role != "tool" || !strings.Contains(result[2].Content, "following user message") {
		t.Fatalf("image-only tool result was silent: %#v", result[2])
	}
	if result[3].Role != "user" || len(result[3].Images) != 1 || result[3].Images[0] != "aGVsbG8=" {
		t.Fatalf("tool image was not delivered through explicit user fallback: %#v", result[3])
	}
	if !strings.Contains(result[3].Content, "inspect") {
		t.Fatalf("fallback does not identify tool provenance: %#v", result[3])
	}
}

func TestBuildOllamaMessagesAggregatesConsecutiveToolResultImages(t *testing.T) {
	toolCalls := func(calls ...ToolCall) Message {
		parts := make([]Part, 0, len(calls))
		for i := range calls {
			call := calls[i]
			parts = append(parts, Part{Type: PartToolCall, ToolCall: &call})
		}
		return Message{Role: RoleAssistant, Parts: parts}
	}
	imageOutput := func(mediaType, data string, text ...string) ToolOutput {
		parts := make([]ToolContentPart, 0, len(text)+1)
		for _, content := range text {
			parts = append(parts, ToolContentPart{Type: ToolContentPartText, Text: content})
		}
		parts = append(parts, ToolContentPart{
			Type:      ToolContentPartImageData,
			ImageData: &ToolImageData{MediaType: mediaType, Base64: data},
		})
		return ToolOutput{ContentParts: parts}
	}

	msgs := []Message{
		UserText("first turn"),
		toolCalls(
			ToolCall{ID: "call-early", Name: "early", Arguments: json.RawMessage(`{}`)},
			ToolCall{ID: "call-mixed", Name: "mixed", Arguments: json.RawMessage(`{}`)},
			ToolCall{ID: "call-text", Name: "text", Arguments: json.RawMessage(`{}`)},
		),
		ToolResultMessageFromOutput("call-early", "early", imageOutput("image/png", "ZWFybHk="), nil),
		ToolResultMessageFromOutput("call-mixed", "mixed", imageOutput("image/webp", "bWl4ZWQ=", "mixed text"), nil),
		ToolResultMessage("call-text", "text", "text only", nil),
		UserText("second turn"),
		toolCalls(ToolCall{ID: "call-later", Name: "later", Arguments: json.RawMessage(`{}`)}),
		ToolResultMessageFromOutput("call-later", "later", imageOutput("image/gif", "bGF0ZXI="), nil),
	}

	result := buildOllamaMessages(msgs)
	if len(result) != 10 {
		t.Fatalf("expected two complete tool runs with separate image fallbacks; got %#v", result)
	}
	wantRoles := []string{"user", "assistant", "tool", "tool", "tool", "user", "user", "assistant", "tool", "user"}
	for i, role := range wantRoles {
		if result[i].Role != role {
			t.Fatalf("message %d role = %q, want %q; messages: %#v", i, result[i].Role, role, result)
		}
	}
	if result[3].Content != "mixed text" {
		t.Fatalf("mixed image/text tool content = %q, want text preserved", result[3].Content)
	}
	if got := result[5].Images; len(got) != 2 || got[0] != "ZWFybHk=" || got[1] != "bWl4ZWQ=" {
		t.Fatalf("first tool-run images = %#v, want early and mixed images in order", got)
	}
	if !strings.Contains(result[5].Content, "early") || !strings.Contains(result[5].Content, "mixed") {
		t.Fatalf("first fallback provenance = %q, want both image tools", result[5].Content)
	}
	if got := result[9].Images; len(got) != 1 || got[0] != "bGF0ZXI=" {
		t.Fatalf("second tool-run images = %#v, want no cross-turn aggregation", got)
	}
	if strings.Contains(result[9].Content, "early") || strings.Contains(result[9].Content, "mixed") {
		t.Fatalf("second fallback leaked first-turn provenance: %q", result[9].Content)
	}
}

func TestBuildOllamaMessagesMalformedToolResultImagesAreVisible(t *testing.T) {
	msgs := []Message{
		UserText("inspect it"),
		{Role: RoleAssistant, Parts: []Part{{
			Type:     PartToolCall,
			ToolCall: &ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{}`)},
		}}},
		ToolResultMessageFromOutput("call-1", "inspect", ToolOutput{
			ContentParts: []ToolContentPart{
				{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png"}},
				{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "not-base64"}},
				{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/svg+xml", Base64: "PHN2Zz4="}},
			},
		}, nil),
	}

	result := buildOllamaMessages(msgs)
	if len(result) != 3 {
		t.Fatalf("invalid images must not create a fake delivered-image message: %#v", result)
	}
	if result[2].Role != "tool" || !strings.Contains(result[2].Content, "omitted 3 invalid or unsupported") {
		t.Fatalf("malformed images were silently dropped: %#v", result[2])
	}
	if len(result[2].Images) != 0 {
		t.Fatalf("invalid images were sent: %#v", result[2].Images)
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

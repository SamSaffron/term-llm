package llm

// Golden wire tests for the Anthropic Messages adapter (direct and Bedrock).
//
// Each scenario under testdata/anthropic_golden/<name>/ stores, per turn:
//   - turnN.response + turnN.response.json: the HTTP response replayed to the
//     adapter (recorded from the live API, or handwritten for edge cases);
//   - turnN.request.json: the canonical request the direct adapter must send;
//   - turnN.bedrock.request.json: the canonical request the Bedrock adapter must send;
//   - events.json: the normalized Event sequence the adapter must emit.
//
// The goldens were produced with the former anthropic-sdk-go based
// implementation as the oracle; the hand-written client must reproduce them.
// Updating rewrites them from the current implementation, so review any diff
// as a wire-format change. Regenerate request/event goldens from fixtures with
//
//	UPDATE_ANTHROPIC_GOLDEN=1 go test ./internal/llm -run 'AnthropicGolden|BedrockGolden'
//
// and refresh live fixtures (direct API only) with
//
//	ANTHROPIC_GOLDEN_LIVE=1 ANTHROPIC_GOLDEN_API_KEY=... [ANTHROPIC_GOLDEN_WORKSPACE_ID=...] \
//	  go test ./internal/llm -run 'TestAnthropicGolden' -count=1
//
// Credentials only ever live in the environment; fixtures store response
// bodies plus status/content-type/retry-after, and request goldens store an
// allowlisted header set with credentials reduced to presence markers.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const anthropicGoldenDir = "testdata/anthropic_golden"

var anthropicGoldenUpdateFlag = flag.Bool("update-anthropic-golden", false, "rewrite Anthropic golden files")

func anthropicGoldenUpdate() bool {
	return *anthropicGoldenUpdateFlag || os.Getenv("UPDATE_ANTHROPIC_GOLDEN") == "1" || anthropicGoldenLive()
}

func anthropicGoldenLive() bool { return os.Getenv("ANTHROPIC_GOLDEN_LIVE") == "1" }

// goldenTurn builds the request for one turn from the conversation so far and
// the events emitted by the previous turn.
type goldenTurn func(history []Message, prev []Event) Request

type anthropicGoldenScenario struct {
	name        string
	model       string
	live        bool   // fixture may be refreshed from the live API
	liveAPIKey  string // overrides the live key (for auth-failure fixtures)
	noBedrock   bool   // skip the Bedrock replay (e.g. direct-API error bodies)
	turns       []goldenTurn
	bedrockOnly bool
}

type goldenResponseMeta struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	RetryAfter  string `json:"retry_after,omitempty"`
	// Bedrock-only fixtures: the body is a list of event-stream frames
	// described in JSON, encoded at replay time.
	EventStream bool `json:"event_stream,omitempty"`
}

type goldenRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	RawPath string              `json:"raw_path,omitempty"`
	Query   string              `json:"query,omitempty"`
	Headers map[string][]string `json:"headers"`
	Body    any                 `json:"body,omitempty"`
}

type goldenToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type goldenError struct {
	Incomplete bool `json:"incomplete,omitempty"`
	Retryable  bool `json:"retryable"`
}

type goldenEvent struct {
	Type          string          `json:"type"`
	Text          string          `json:"text,omitempty"`
	ReasoningKind string          `json:"reasoning_kind,omitempty"`
	Encrypted     string          `json:"encrypted,omitempty"`
	Tool          *goldenToolCall `json:"tool,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolSuccess   *bool           `json:"tool_success,omitempty"`
	Usage         *Usage          `json:"usage,omitempty"`
	Error         *goldenError    `json:"error,omitempty"`
}

func normalizeGoldenEvent(ev Event) goldenEvent {
	out := goldenEvent{Type: string(ev.Type), Text: ev.Text}
	switch ev.Type {
	case EventReasoningDelta:
		out.ReasoningKind = string(ev.ReasoningKind)
		out.Encrypted = ev.ReasoningEncryptedContent
	case EventToolCall:
		if ev.Tool != nil {
			out.Tool = &goldenToolCall{ID: ev.Tool.ID, Name: ev.Tool.Name, Args: canonicalRawJSON(ev.Tool.Arguments)}
		}
	case EventToolExecStart:
		out.ToolName = ev.ToolName
	case EventToolExecEnd:
		out.ToolName = ev.ToolName
		success := ev.ToolSuccess
		out.ToolSuccess = &success
	case EventUsage:
		out.Usage = ev.Use
	case EventError:
		var incomplete *StreamIncompleteError
		out.Error = &goldenError{Incomplete: errors.As(ev.Err, &incomplete), Retryable: isRetryable(ev.Err)}
	}
	return out
}

func canonicalRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		quoted, _ := json.Marshal("INVALID_JSON:" + string(raw))
		return quoted
	}
	out, _ := json.Marshal(v)
	return out
}

var goldenRequestHeaderAllow = map[string]bool{
	"anthropic-version":    true,
	"anthropic-beta":       true,
	"content-type":         true,
	"accept":               true,
	"x-opencode-client":    true,
	"x-opencode-session":   true,
	"x-amz-security-token": true,
}

var goldenAuthDateRe = regexp.MustCompile(`Credential=([^/]+)/\d{8}/`)

func normalizeGoldenRequestHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	for k, values := range h {
		lk := strings.ToLower(k)
		switch {
		case lk == "x-api-key":
			out[lk] = []string{"<present>"}
		case lk == "authorization":
			v := strings.Join(values, ",")
			if strings.HasPrefix(v, "AWS4-HMAC-SHA256 ") {
				scope := goldenAuthDateRe.FindStringSubmatch(v)
				id := ""
				if len(scope) > 1 {
					id = scope[1]
				}
				out[lk] = []string{"AWS4-HMAC-SHA256 Credential=" + id + "/<date>/..."}
			} else {
				out[lk] = []string{"<present>"}
			}
		case lk == "x-amz-date":
			out[lk] = []string{"<present>"}
		case goldenRequestHeaderAllow[lk]:
			vals := append([]string(nil), values...)
			out[lk] = vals
		}
	}
	return out
}

func normalizeGoldenRequest(r *http.Request, body []byte) goldenRequest {
	req := goldenRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Query:   r.URL.RawQuery,
		Headers: normalizeGoldenRequestHeaders(r.Header),
	}
	if r.URL.RawPath != "" {
		req.RawPath = r.URL.RawPath
	}
	if len(body) > 0 {
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			req.Body = "NON_JSON:" + string(body)
		} else {
			req.Body = v
		}
	}
	return req
}

func goldenJSON(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	return buf.Bytes()
}

func compareOrUpdateGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if anthropicGoldenUpdate() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_ANTHROPIC_GOLDEN=1 to create)", path, err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Errorf("golden mismatch for %s\n%s", path, goldenDiff(string(want), string(got)))
	}
}

// goldenDiff renders a compact line diff (first divergence plus context).
func goldenDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	i := 0
	for i < len(wl) && i < len(gl) && wl[i] == gl[i] {
		i++
	}
	start := max(i-5, 0)
	var b strings.Builder
	fmt.Fprintf(&b, "first difference at line %d\n--- want\n", i+1)
	for j := start; j < min(i+15, len(wl)); j++ {
		fmt.Fprintf(&b, "  %s\n", wl[j])
	}
	b.WriteString("+++ got\n")
	for j := start; j < min(i+15, len(gl)); j++ {
		fmt.Fprintf(&b, "  %s\n", gl[j])
	}
	return b.String()
}

// ---- conversation helpers ----

// goldenAssistantFromEvents rebuilds the assistant turn the engine would
// replay: reasoning (with signature), text, and tool calls.
func goldenAssistantFromEvents(events []Event) (Message, []ToolCall) {
	var reasoning, signature, text strings.Builder
	var calls []ToolCall
	for _, ev := range events {
		switch ev.Type {
		case EventReasoningDelta:
			reasoning.WriteString(ev.Text)
			signature.WriteString(ev.ReasoningEncryptedContent)
		case EventTextDelta:
			text.WriteString(ev.Text)
		case EventToolCall:
			if ev.Tool != nil {
				calls = append(calls, *ev.Tool)
			}
		}
	}
	msg := Message{Role: RoleAssistant}
	if signature.Len() > 0 || reasoning.Len() > 0 || text.Len() > 0 {
		msg.Parts = append(msg.Parts, Part{
			Type:                      PartText,
			Text:                      text.String(),
			ReasoningContent:          reasoning.String(),
			ReasoningEncryptedContent: signature.String(),
		})
	}
	for i := range calls {
		call := calls[i]
		msg.Parts = append(msg.Parts, Part{Type: PartToolCall, ToolCall: &call})
	}
	return msg, calls
}

var goldenReadFileTool = ToolSpec{
	Name:        "read_file",
	Description: "Read a text file from the working directory.",
	Schema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string", "description": "Relative file path"},
		},
		"required": []interface{}{"path"},
	},
}

var goldenViewImageTool = ToolSpec{
	Name:        "view_image",
	Description: "View an image file.",
	Schema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"path"},
	},
}

// goldenNoDescriptionTool has no description and no properties.
var goldenNoDescriptionTool = ToolSpec{
	Name:   "ping_tool",
	Schema: map[string]interface{}{"type": "object"},
}

var goldenFiles = map[string]string{
	"notes.txt": "The secret word is PELICAN.\n",
	"a.txt":     "alpha=1\n",
	"b.txt":     "beta=2\n",
}

// goldenRedPNG is a 16x16 solid red PNG.
const goldenRedPNG = "iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAIAAACQkWg2AAAAFklEQVR4nGO4I2JDEmIY1TCqYfhqAAAeBCwQ8YdREQAAAABJRU5ErkJggg=="

func goldenToolResultsFor(calls []ToolCall, isError bool) []Message {
	var out []Message
	for _, call := range calls {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		name := filepath.Base(args.Path)
		switch call.Name {
		case "view_image":
			out = append(out, ToolResultMessageFromOutput(call.ID, call.Name, ToolOutput{
				Content: "Image " + name,
				ContentParts: []ToolContentPart{
					{Type: ToolContentPartText, Text: "Image " + name + " (16x16)"},
					{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: goldenRedPNG}},
				},
			}, nil))
		default:
			content, ok := goldenFiles[name]
			if !ok || isError {
				out = append(out, ToolErrorMessage(call.ID, call.Name, "file not found: "+args.Path, nil))
				continue
			}
			out = append(out, ToolResultMessage(call.ID, call.Name, content, nil))
		}
	}
	return out
}

func goldenSingleTurn(req Request) []goldenTurn {
	return []goldenTurn{func([]Message, []Event) Request { return req }}
}

// goldenToolLoop returns a two-turn conversation: the user prompt, then the
// replayed assistant turn with tool results.
func goldenToolLoop(base Request, isError bool) []goldenTurn {
	return []goldenTurn{
		func([]Message, []Event) Request { return base },
		func(history []Message, prev []Event) Request {
			assistant, calls := goldenAssistantFromEvents(prev)
			next := base
			next.Messages = append(append([]Message(nil), history...), assistant)
			next.Messages = append(next.Messages, goldenToolResultsFor(calls, isError)...)
			return next
		},
	}
}

func goldenUser(text string) []Message { return []Message{UserText(text)} }

func anthropicGoldenScenarios() []anthropicGoldenScenario {
	sys := SystemText("You are a terse assistant used for wire-format golden tests.")
	toolsReq := func(prompt string, tools ...ToolSpec) Request {
		return Request{Messages: []Message{sys, UserText(prompt)}, Tools: tools, ToolChoice: ToolChoice{Mode: ToolChoiceAuto}}
	}
	parallel := toolsReq("Read a.txt and b.txt using two read_file calls in the same turn, then report both values.", goldenReadFileTool)
	parallel.ParallelToolCalls = true

	scenarios := []anthropicGoldenScenario{
		{name: "text", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: []Message{sys, UserText("Reply with exactly: golden hello")}})},
		{name: "thinking_budget_haiku", model: "claude-haiku-4-5-thinking", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("What is 17*23? Answer briefly.")})},
		{name: "thinking_budget", model: "claude-opus-4-5-thinking", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("What is 17*23? Answer briefly.")})},
		{name: "thinking_budget_small_max", model: "claude-opus-4-5-thinking", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Say hi."), MaxOutputTokens: 2048})},
		{name: "adaptive", model: "claude-sonnet-4-6-thinking", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Is 221 prime? One line.")})},
		{name: "effort_request", model: "claude-opus-4-7", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Name one primary colour. One word."), ReasoningEffort: "LOW"})},
		{name: "max_tokens", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Count from 1 to 50 separated by spaces."), MaxOutputTokens: 5})},
		{name: "tool_single", model: "claude-haiku-4-5", live: true, turns: goldenToolLoop(toolsReq("Use read_file on notes.txt, then tell me the secret word.", goldenReadFileTool), false)},
		{name: "tool_parallel", model: "claude-haiku-4-5", live: true, turns: goldenToolLoop(parallel, false)},
		{name: "tool_error_result", model: "claude-haiku-4-5", live: true, turns: goldenToolLoop(toolsReq("Use read_file on missing.txt and report what happened in one sentence.", goldenReadFileTool), true)},
		{name: "tool_adaptive", model: "claude-sonnet-4-6-thinking", live: true, turns: goldenToolLoop(toolsReq("Use read_file on notes.txt and tell me the secret word.", goldenReadFileTool), false)},
		{name: "tool_thinking_budget", model: "claude-opus-4-5-thinking", live: true, turns: goldenToolLoop(toolsReq("Use read_file on notes.txt and tell me the secret word.", goldenReadFileTool), false)},
		{name: "tool_choice_required", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Hello."), Tools: []ToolSpec{goldenReadFileTool, goldenNoDescriptionTool}, ToolChoice: ToolChoice{Mode: ToolChoiceRequired}})},
		{name: "tool_choice_name", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Call the ping tool."), Tools: []ToolSpec{goldenReadFileTool, goldenNoDescriptionTool}, ToolChoice: ToolChoice{Mode: ToolChoiceName, Name: "ping_tool"}})},
		{name: "tool_choice_none", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Reply with exactly: none"), Tools: []ToolSpec{goldenReadFileTool}, ToolChoice: ToolChoice{Mode: ToolChoiceNone}})},
		{name: "image_user", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: []Message{UserImageMessageWithPath("image/png", goldenRedPNG, "/tmp/red.png", "What colour is this image? One word.")}})},
		{name: "tool_image_result", model: "claude-haiku-4-5", live: true, turns: goldenToolLoop(toolsReq("Use view_image on red.png and tell me its colour in one word.", goldenViewImageTool), false)},
		{name: "developer_and_cache", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: []Message{
			sys,
			{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Always answer in lowercase."}}},
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Summary anchor: we are testing."}}, CacheAnchor: true},
			AssistantText("ok"),
			{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Keep it to two words."}}},
			{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: ""}}},
			UserText("Reply with: dev ok"),
			{Role: RoleDeveloper, Parts: []Part{{Type: PartText, Text: "Trailing developer note."}}},
		}})},
		{name: "trailing_assistant", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Messages: []Message{UserText("Say the word blue."), AssistantText("blue")}})},
		{name: "search", model: "claude-haiku-4-5", live: true, turns: goldenSingleTurn(Request{Search: true, Messages: goldenUser("Using web search, what is the title of the go.dev homepage? Short answer.")})},
		{name: "search_tools_adaptive", model: "claude-sonnet-4-6-thinking", live: true, turns: func() []goldenTurn {
			req := toolsReq("Read notes.txt with read_file and reply with the secret word. Do not search.", goldenReadFileTool, goldenNoDescriptionTool)
			req.Search = true
			return goldenToolLoop(req, false)
		}()},
		{name: "search_effort", model: "claude-opus-4-7", live: true, turns: goldenSingleTurn(Request{Search: true, ReasoningEffort: "low", Messages: goldenUser("Reply with exactly: ok. Do not search.")})},
		{name: "search_thinking_budget", model: "claude-opus-4-5-thinking", live: true, turns: goldenSingleTurn(Request{Search: true, Messages: goldenUser("Reply with exactly: ok. Do not search."), Tools: []ToolSpec{goldenReadFileTool}})},
		{name: "one_m", model: "claude-sonnet-4-6-1m", live: true, turns: goldenSingleTurn(Request{Messages: goldenUser("Reply with exactly: ok")})},
		{name: "one_m_search", model: "claude-sonnet-4-6-1m", live: true, turns: goldenSingleTurn(Request{Search: true, Messages: goldenUser("Reply with exactly: ok")})},
		{name: "bad_model", model: "claude-nonexistent-9", live: true, noBedrock: true, turns: goldenSingleTurn(Request{Messages: goldenUser("hi")})},
		{name: "bad_key", model: "claude-haiku-4-5", live: true, liveAPIKey: "sk-ant-invalid-golden", noBedrock: true, turns: goldenSingleTurn(Request{Messages: goldenUser("hi")})},
	}
	// Handwritten stream fixtures for protocol edge cases.
	for _, name := range []string{
		"edge_server_tool_block_stop", "edge_server_tool_then_text", "edge_server_tool_stream_end",
		"edge_truncated_text", "edge_truncated_server_tool", "edge_error_event_overloaded",
		"edge_unknown_events", "edge_tool_use_start_input", "edge_crlf", "edge_multiline_data",
		"edge_ping_only", "edge_empty_body",
	} {
		search := strings.Contains(name, "server_tool")
		scenarios = append(scenarios, anthropicGoldenScenario{name: name, model: "claude-sonnet-4-6", turns: goldenSingleTurn(Request{Search: search, Messages: goldenUser("edge")})})
	}
	for _, name := range []string{"edge_http_429", "edge_http_529", "edge_http_500_html", "edge_http_400"} {
		scenarios = append(scenarios, anthropicGoldenScenario{name: name, model: "claude-sonnet-4-6", noBedrock: true, turns: goldenSingleTurn(Request{Messages: goldenUser("edge")})})
	}
	return scenarios
}

// ---- fixture server ----

type goldenServer struct {
	t        *testing.T
	dir      string
	scenario anthropicGoldenScenario
	live     bool
	bedrock  bool

	mu       sync.Mutex
	turn     int
	requests []goldenRequest
}

func (s *goldenServer) fixturePaths() (string, string) {
	prefix := filepath.Join(s.dir, fmt.Sprintf("turn%d", s.turn))
	if s.bedrock && s.scenario.bedrockOnly {
		return prefix + ".bedrock.response", prefix + ".bedrock.response.json"
	}
	return prefix + ".response", prefix + ".response.json"
}

func (s *goldenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, normalizeGoldenRequest(r, body))
	bodyPath, metaPath := s.fixturePaths()
	s.mu.Unlock()

	if s.live && !s.bedrock {
		s.forwardLive(w, r, body, bodyPath, metaPath)
		return
	}
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		http.Error(w, "missing fixture "+metaPath, http.StatusTeapot)
		return
	}
	var meta goldenResponseMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
		return
	}
	payload, err := os.ReadFile(bodyPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTeapot)
		return
	}
	if meta.RetryAfter != "" {
		w.Header().Set("Retry-After", meta.RetryAfter)
	}
	switch {
	case s.bedrock && meta.EventStream:
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(meta.Status)
		_, _ = w.Write(encodeGoldenEventStreamJSON(s.t, payload))
	case s.bedrock && meta.Status == http.StatusOK && strings.HasPrefix(meta.ContentType, "text/event-stream"):
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(meta.Status)
		_, _ = w.Write(sseToGoldenEventStream(s.t, payload))
	default:
		if meta.ContentType != "" {
			w.Header().Set("Content-Type", meta.ContentType)
		}
		w.WriteHeader(meta.Status)
		_, _ = w.Write(payload)
	}
}

var goldenLiveClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}, Timeout: 5 * time.Minute}

func (s *goldenServer) forwardLive(w http.ResponseWriter, r *http.Request, body []byte, bodyPath, metaPath string) {
	upstream := os.Getenv("ANTHROPIC_GOLDEN_UPSTREAM")
	if upstream == "" {
		upstream = "https://api.anthropic.com"
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta"} {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	key := os.Getenv("ANTHROPIC_GOLDEN_API_KEY")
	if s.scenario.liveAPIKey != "" {
		key = s.scenario.liveAPIKey
	}
	req.Header.Set("X-Api-Key", key)
	if ws := os.Getenv("ANTHROPIC_GOLDEN_WORKSPACE_ID"); ws != "" {
		req.Header.Set("Anthropic-Workspace-Id", ws)
	}
	resp, err := goldenLiveClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	meta := goldenResponseMeta{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), RetryAfter: resp.Header.Get("Retry-After")}
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		s.t.Errorf("mkdir: %v", err)
	}
	if err := os.WriteFile(bodyPath, payload, 0o644); err != nil {
		s.t.Errorf("write fixture: %v", err)
	}
	if err := os.WriteFile(metaPath, goldenJSON(s.t, meta), 0o644); err != nil {
		s.t.Errorf("write fixture meta: %v", err)
	}
	if meta.RetryAfter != "" {
		w.Header().Set("Retry-After", meta.RetryAfter)
	}
	w.Header().Set("Content-Type", meta.ContentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(payload)
}

// ---- SSE -> AWS event stream conversion (Bedrock replay) ----

func parseGoldenSSEData(t *testing.T, payload []byte) [][]byte {
	t.Helper()
	text := strings.ReplaceAll(string(payload), "\r\n", "\n")
	var out [][]byte
	for _, block := range strings.Split(text, "\n\n") {
		var data []string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if len(data) > 0 {
			out = append(out, []byte(strings.Join(data, "\n")))
		}
	}
	return out
}

type goldenEventStreamHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type goldenEventStreamFrame struct {
	Headers []goldenEventStreamHeader `json:"headers"`
	Payload string                    `json:"payload"`
}

func encodeGoldenEventStreamFrame(headers []goldenEventStreamHeader, payload []byte) []byte {
	var hb bytes.Buffer
	for _, h := range headers {
		hb.WriteByte(byte(len(h.Name)))
		hb.WriteString(h.Name)
		hb.WriteByte(7) // string
		_ = binary.Write(&hb, binary.BigEndian, uint16(len(h.Value)))
		hb.WriteString(h.Value)
	}
	total := 12 + hb.Len() + len(payload) + 4
	var msg bytes.Buffer
	_ = binary.Write(&msg, binary.BigEndian, uint32(total))
	_ = binary.Write(&msg, binary.BigEndian, uint32(hb.Len()))
	_ = binary.Write(&msg, binary.BigEndian, crc32.ChecksumIEEE(msg.Bytes()[:8]))
	msg.Write(hb.Bytes())
	msg.Write(payload)
	_ = binary.Write(&msg, binary.BigEndian, crc32.ChecksumIEEE(msg.Bytes()))
	return msg.Bytes()
}

func goldenChunkFrame(data []byte) []byte {
	chunk, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(data), "p": "abcdefgh"})
	return encodeGoldenEventStreamFrame([]goldenEventStreamHeader{
		{Name: ":event-type", Value: "chunk"},
		{Name: ":content-type", Value: "application/json"},
		{Name: ":message-type", Value: "event"},
	}, chunk)
}

func sseToGoldenEventStream(t *testing.T, payload []byte) []byte {
	var out bytes.Buffer
	for _, data := range parseGoldenSSEData(t, payload) {
		out.Write(goldenChunkFrame(data))
	}
	return out.Bytes()
}

// encodeGoldenEventStreamJSON encodes a Bedrock-only fixture: a JSON list of
// frames. A frame with header ":event-type"="chunk" and no explicit payload
// wraps "chunk" JSON as base64 bytes.
func encodeGoldenEventStreamJSON(t *testing.T, payload []byte) []byte {
	t.Helper()
	var frames []struct {
		Chunk   json.RawMessage           `json:"chunk,omitempty"`
		Headers []goldenEventStreamHeader `json:"headers,omitempty"`
		Payload string                    `json:"payload,omitempty"`
	}
	if err := json.Unmarshal(payload, &frames); err != nil {
		t.Fatalf("decode event-stream fixture: %v", err)
	}
	var out bytes.Buffer
	for _, f := range frames {
		if len(f.Chunk) > 0 {
			out.Write(goldenChunkFrame(f.Chunk))
			continue
		}
		out.Write(encodeGoldenEventStreamFrame(f.Headers, []byte(f.Payload)))
	}
	return out.Bytes()
}

// ---- runners ----

func goldenIsolateEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for k, v := range map[string]string{
		"HOME":                        home,
		"ANTHROPIC_API_KEY":           "",
		"ANTHROPIC_AUTH_TOKEN":        "",
		"ANTHROPIC_BASE_URL":          "",
		"ANTHROPIC_PROFILE":           "",
		"ANTHROPIC_CUSTOM_HEADERS":    "",
		"AWS_ACCESS_KEY_ID":           "",
		"AWS_SECRET_ACCESS_KEY":       "",
		"AWS_SESSION_TOKEN":           "",
		"AWS_PROFILE":                 "",
		"AWS_REGION":                  "",
		"AWS_DEFAULT_REGION":          "",
		"AWS_BEARER_TOKEN_BEDROCK":    "",
		"AWS_CONFIG_FILE":             filepath.Join(home, "aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(home, "aws-credentials"),
		"AWS_EC2_METADATA_DISABLED":   "true",
	} {
		t.Setenv(k, v)
	}
}

func drainGoldenStream(t *testing.T, stream Stream) []Event {
	t.Helper()
	defer stream.Close()
	var events []Event
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			events = append(events, Event{Type: EventError, Err: err})
			return events
		}
		events = append(events, ev)
		if ev.Type == EventDone {
			return events
		}
	}
}

type goldenProviderFactory func(t *testing.T, baseURL string, sc anthropicGoldenScenario) Provider

func runAnthropicGoldenScenario(t *testing.T, sc anthropicGoldenScenario, bedrock bool, newProvider goldenProviderFactory) [][]goldenEvent {
	t.Helper()
	dir := filepath.Join(anthropicGoldenDir, sc.name)
	srv := &goldenServer{t: t, dir: dir, scenario: sc, live: anthropicGoldenLive() && sc.live, bedrock: bedrock}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	provider := newProvider(t, ts.URL, sc)
	var history []Message
	var prev []Event
	var all [][]goldenEvent
	for i, turn := range sc.turns {
		srv.mu.Lock()
		srv.turn = i + 1
		srv.requests = nil
		srv.mu.Unlock()

		req := turn(history, prev)
		history = req.Messages
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		stream, err := provider.Stream(ctx, req)
		var events []Event
		if err != nil {
			events = []Event{{Type: EventError, Err: err}}
		} else {
			events = drainGoldenStream(t, stream)
		}
		cancel()

		normalized := make([]goldenEvent, 0, len(events))
		for _, ev := range events {
			normalized = append(normalized, normalizeGoldenEvent(ev))
		}
		all = append(all, normalized)
		prev = events

		srv.mu.Lock()
		requests := srv.requests
		srv.mu.Unlock()
		if len(requests) == 0 {
			t.Fatalf("turn %d: adapter sent no request", i+1)
		}
		suffix := ".request.json"
		if bedrock {
			suffix = ".bedrock.request.json"
		}
		// Retries resend the same request; the golden covers the final attempt.
		compareOrUpdateGolden(t, filepath.Join(dir, fmt.Sprintf("turn%d%s", i+1, suffix)), goldenJSON(t, requests[len(requests)-1]))
	}
	return all
}

func newGoldenDirectProvider(t *testing.T, baseURL string, sc anthropicGoldenScenario) Provider {
	t.Helper()
	p, err := NewAnthropicProviderWithBaseURL("test-key", sc.model, AnthropicCredAPIKey, baseURL)
	if err != nil {
		t.Fatalf("NewAnthropicProviderWithBaseURL: %v", err)
	}
	return p
}

// goldenRedirectTransport sends every request to the fixture server while
// preserving the original path, raw path, query, and headers.
type goldenRedirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt goldenRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.Host = rt.target.Host
	return rt.base.RoundTrip(clone)
}

func newGoldenBedrockProvider(t *testing.T, baseURL string, sc anthropicGoldenScenario) Provider {
	t.Helper()
	target, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = goldenRedirectTransport{target: target, base: original.(*http.Transport).Clone()}
	t.Cleanup(func() { http.DefaultTransport = original })
	p, err := NewBedrockProvider(sc.model, "us-west-2", "", "AKIDGOLDENEXAMPLE", "golden-secret", "golden-session", nil, nil)
	if err != nil {
		t.Fatalf("NewBedrockProvider: %v", err)
	}
	return p
}

func TestAnthropicGolden(t *testing.T) {
	for _, sc := range anthropicGoldenScenarios() {
		if sc.bedrockOnly {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			goldenIsolateEnv(t)
			if anthropicGoldenLive() && !sc.live {
				t.Skip("handwritten fixture")
			}
			events := runAnthropicGoldenScenario(t, sc, false, newGoldenDirectProvider)
			compareOrUpdateGolden(t, filepath.Join(anthropicGoldenDir, sc.name, "events.json"), goldenJSON(t, events))
		})
	}
}

func TestBedrockGolden(t *testing.T) {
	if anthropicGoldenLive() {
		t.Skip("Bedrock goldens replay direct-API fixtures")
	}
	scenarios := append(anthropicGoldenScenarios(), bedrockOnlyGoldenScenarios()...)
	for _, sc := range scenarios {
		if sc.noBedrock {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			goldenIsolateEnv(t)
			events := runAnthropicGoldenScenario(t, sc, true, newGoldenBedrockProvider)
			eventsPath := filepath.Join(anthropicGoldenDir, sc.name, "events.json")
			if sc.bedrockOnly {
				compareOrUpdateGolden(t, filepath.Join(anthropicGoldenDir, sc.name, "bedrock.events.json"), goldenJSON(t, events))
				return
			}
			// Bedrock must surface exactly the same events as the direct API
			// for the same Anthropic stream content.
			want, err := os.ReadFile(eventsPath)
			if err != nil {
				t.Fatalf("read %s: %v", eventsPath, err)
			}
			if got := goldenJSON(t, events); !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
				t.Errorf("bedrock events differ from direct events\n%s", goldenDiff(string(want), string(got)))
			}
		})
	}
}

func bedrockOnlyGoldenScenarios() []anthropicGoldenScenario {
	var out []anthropicGoldenScenario
	for _, name := range []string{
		"bedrock_arn_model", "bedrock_exception_throttling", "bedrock_exception_no_type",
		"bedrock_error_message", "bedrock_non_chunk_event", "bedrock_http_429",
	} {
		model := "claude-sonnet-4-6"
		if name == "bedrock_arn_model" {
			model = "arn:aws:bedrock:us-west-2:123456789012:application-inference-profile/abc123"
		}
		out = append(out, anthropicGoldenScenario{name: name, model: model, bedrockOnly: true, turns: goldenSingleTurn(Request{Messages: goldenUser("edge")})})
	}
	return out
}

func TestAnthropicGoldenListModels(t *testing.T) {
	goldenIsolateEnv(t)
	dir := filepath.Join(anthropicGoldenDir, "list_models")
	sc := anthropicGoldenScenario{name: "list_models", model: "claude-haiku-4-5", live: true}
	srv := &goldenServer{t: t, dir: dir, scenario: sc, live: anthropicGoldenLive(), turn: 1}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	p, err := NewAnthropicProviderWithBaseURL("test-key", sc.model, AnthropicCredAPIKey, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(srv.requests) == 0 {
		t.Fatal("ListModels sent no request")
	}
	compareOrUpdateGolden(t, filepath.Join(dir, "turn1.request.json"), goldenJSON(t, srv.requests[len(srv.requests)-1]))
	compareOrUpdateGolden(t, filepath.Join(dir, "models.json"), goldenJSON(t, models))
}

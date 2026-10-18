package cmd

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/inspect"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/share"
	"github.com/samsaffron/term-llm/internal/tools"
)

func inspectRequest(s *serveServer, method, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	s.handleSessionByID(rr, httptest.NewRequest(method, path, nil))
	return rr
}

func appendInspectMessage(t *testing.T, store session.Store, id string, message llm.Message) *session.Message {
	t.Helper()
	msg := session.NewMessage(id, message, -1)
	if err := store.AddMessage(context.Background(), id, msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func decodeInspect(t *testing.T, rr *httptest.ResponseRecorder) inspect.Document {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var d inspect.Document
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestServeInspectHiddenRowsAndExclusions(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	for _, message := range []llm.Message{
		{Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "hidden system"}}},
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "hidden developer"}}},
		llm.GoalSteeringText("hidden goal"),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartPathNote}, {Type: llm.PartText, Text: "hidden path note"}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartText, Text: "answer", ReasoningContent: "**Plan**\n\nsafe reasoning", ReasoningEncryptedContent: "SECRET_CIPHERTEXT"},
			{Type: llm.PartText, ReasoningKind: llm.ReasoningKindEncrypted, ReasoningContent: "SECRET_ENCRYPTED_TEXT", ReasoningEncryptedContent: "SECRET_CIPHERTEXT"},
			{Type: llm.PartProviderReplay, ProviderReplay: &llm.ProviderReplayItem{Raw: json.RawMessage(`{"secret":"SECRET_REPLAY"}`)}},
			{Type: llm.PartDiscoveryCall, DiscoveryCall: &llm.ToolDiscoveryCall{Arguments: json.RawMessage(`{"secret":"SECRET_DISCOVERY"}`)}},
			{Type: llm.PartDiscoveryOutput, DiscoveryOutput: &llm.ToolDiscoveryOutput{CatalogueHash: "SECRET_DISCOVERY"}},
		}},
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "SECRET_BASE64"}}}},
	} {
		appendInspectMessage(t, store, sess.ID, message)
	}
	rr := inspectRequest(srv, "GET", "/v1/sessions/"+sess.ID+"/inspect")
	d := decodeInspect(t, rr)
	if d.SchemaVersion != 1 || d.SessionID != sess.ID || d.Scope != "persisted_history" || d.TranscriptRev <= 0 {
		t.Fatalf("document = %+v", d)
	}
	kinds := map[string]bool{}
	ids := []string{}
	for _, block := range d.Blocks {
		kinds[block.Kind] = true
		ids = append(ids, block.ID)
	}
	for _, kind := range []string{"system", "developer", "goal_steering", "path_note", "reasoning", "assistant_text", "image"} {
		if !kinds[kind] {
			t.Errorf("missing kind %s", kind)
		}
	}
	bodies := inspectRequest(srv, "GET", "/v1/sessions/"+sess.ID+"/inspect/bodies?ids="+strings.Join(ids, ","))
	if bodies.Code != 200 {
		t.Fatalf("bodies %d: %s", bodies.Code, bodies.Body.String())
	}
	for _, response := range []*httptest.ResponseRecorder{rr, bodies} {
		if strings.Contains(response.Body.String(), "SECRET_") {
			t.Fatalf("secret leaked: %s", response.Body.String())
		}
	}
	if !strings.Contains(bodies.Body.String(), "safe reasoning") {
		t.Fatal("safe reasoning omitted")
	}
}

func TestServeInspectColdRuntimeAndHTTP(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	appendInspectMessage(t, store, sess.ID, llm.UserText(strings.Repeat("hello world ", 200)))
	created := 0
	srv.sessionMgr = &serveSessionManager{sessions: map[string]*serveRuntime{}, factory: func(context.Context) (*serveRuntime, error) { created++; return nil, errors.New("must not create") }}
	path := "/v1/sessions/" + sess.ID + "/inspect"
	first := inspectRequest(srv, "GET", path)
	d := decodeInspect(t, first)
	if d.Header.Runtime.Loaded || d.Header.Runtime.UnavailableReason != "runtime_not_loaded" || created != 0 || len(srv.sessionMgr.sessions) != 0 {
		t.Fatalf("cold runtime = %+v, created=%d", d.Header.Runtime, created)
	}
	for _, suffix := range []string{"/inspect", "/inspect/bodies?ids=" + d.Blocks[0].ID} {
		t.Run(suffix, func(t *testing.T) {
			path := "/v1/sessions/" + sess.ID + suffix
			first := inspectRequest(srv, "GET", path)
			request := httptest.NewRequest("GET", path, nil)
			request.Header.Set("If-None-Match", first.Header().Get("ETag"))
			rr := httptest.NewRecorder()
			srv.handleSessionByID(rr, request)
			if first.Code != 200 || first.Header().Get("ETag") == "" || rr.Code != 304 || rr.Body.Len() != 0 || rr.Header().Get("X-Transcript-Rev") != first.Header().Get("X-Transcript-Rev") {
				t.Fatalf("ETag first %d/%v repeat %d/%s", first.Code, first.Header(), rr.Code, rr.Body.String())
			}
			for _, method := range []string{"POST", "PUT", "DELETE", "HEAD"} {
				denied := inspectRequest(srv, method, path)
				if denied.Code != 405 || denied.Header().Get("Allow") != "GET" {
					t.Fatalf("%s status=%d Allow=%q", method, denied.Code, denied.Header().Get("Allow"))
				}
			}
			unknown := inspectRequest(srv, "GET", "/v1/sessions/unknown"+suffix)
			if unknown.Code != 404 {
				t.Fatalf("unknown status=%d", unknown.Code)
			}
		})
	}
	gzipPath := path + "/bodies?ids=" + d.Blocks[0].ID
	plain := inspectRequest(srv, "GET", gzipPath)
	request := httptest.NewRequest("GET", gzipPath, nil)
	request.Header.Set("Accept-Encoding", "gzip")
	zipped := httptest.NewRecorder()
	srv.handleSessionByID(zipped, request)
	reader, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatalf("gzip response: %v headers=%v", err, zipped.Header())
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != plain.Body.String() {
		t.Fatalf("gzip differs: %v", err)
	}
}

type inspectRuntimeTestTool struct {
	llm.Tool
	spec   llm.ToolSpec
	onSpec func()
}

func (t inspectRuntimeTestTool) Spec() llm.ToolSpec {
	if t.onSpec != nil {
		t.onSpec()
	}
	return t.spec
}

type inspectDiscoveryPlanner struct {
	llm.ToolSurfacePlanner
	onRead func()
	specs  []llm.ToolSpec
}

func (p inspectDiscoveryPlanner) ActiveToolSpecs(string) []llm.ToolSpec { p.onRead(); return p.specs }
func (p inspectDiscoveryPlanner) Diagnostics(string) llm.ToolDiscoveryDiagnostics {
	p.onRead()
	return llm.ToolDiscoveryDiagnostics{ResolvedMode: "on"}
}

func TestServeInspectWarmAndBusyRuntime(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	appendInspectMessage(t, store, sess.ID, llm.Message{Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "old prompt"}}})
	rt := &serveRuntime{systemPrompt: "new prompt", engine: llm.NewEngine(nil, nil)}
	rt.Touch()
	lastUsed := rt.LastUsed()
	checkUnlocked := func() {
		t.Helper()
		if !rt.mu.TryLock() {
			t.Fatal("runtime lock held across engine access")
		}
		rt.mu.Unlock()
	}
	approval := tools.NewApprovalManager(nil)
	approval.SetYoloMode(true)
	rt.toolMgr = &tools.ToolManager{ApprovalMgr: approval}
	for _, name := range []string{"z_tool", "a_tool", tools.ManageWorkspaceToolName} {
		rt.engine.Tools().Register(inspectRuntimeTestTool{spec: llm.ToolSpec{Name: name, Description: "description", Schema: map[string]any{"type": "object"}}, onSpec: checkUnlocked})
	}
	rt.engine.SetToolSurfacePlanner(inspectDiscoveryPlanner{onRead: checkUnlocked, specs: []llm.ToolSpec{{Name: "mcp_active"}, {Name: "a_tool"}}})
	srv.sessionMgr = &serveSessionManager{sessions: map[string]*serveRuntime{sess.ID: rt}}
	path := "/v1/sessions/" + sess.ID + "/inspect"
	d := decodeInspect(t, inspectRequest(srv, "GET", path))
	runtime := d.Header.Runtime
	var names []string
	for _, tool := range runtime.Tools {
		names = append(names, tool.Name)
	}
	if !reflect.DeepEqual(names, []string{"a_tool", "mcp_active", "z_tool"}) || runtime.ToolDiscovery == nil || runtime.SystemPromptBlockID != "runtime.system_prompt" || runtime.SystemPromptMatchesPersisted == nil || *runtime.SystemPromptMatchesPersisted {
		t.Fatalf("runtime = %+v, names=%v", runtime, names)
	}
	for _, block := range d.Blocks {
		if strings.HasPrefix(block.ID, "runtime.") {
			t.Fatalf("runtime data leaked into persisted transcript blocks: %+v", block)
		}
	}
	if !rt.LastUsed().Equal(lastUsed) {
		t.Fatal("inspect renewed runtime TTL")
	}
	body := inspectRequest(srv, "GET", path+"/bodies?ids=runtime.system_prompt,runtime.tool.2")
	if body.Code != 200 || !strings.Contains(body.Body.String(), "new prompt") || !strings.Contains(body.Body.String(), `\"schema\"`) {
		t.Fatalf("runtime body: %d %s", body.Code, body.Body.String())
	}
	rt.mu.Lock()
	busy := inspectRequest(srv, "GET", path)
	rt.mu.Unlock()
	got := decodeInspect(t, busy)
	if !got.Header.Runtime.Loaded || got.Header.Runtime.UnavailableReason != "runtime_busy" || len(got.Header.Runtime.Tools) != 0 {
		t.Fatalf("busy runtime = %+v", got.Header.Runtime)
	}
	if len(got.Blocks) != 1 || got.Blocks[0].Preview != "old prompt" {
		t.Fatal("busy runtime hid persisted history", got.Blocks)
	}
}

func TestServeInspectRuntimeHistoryFallback(t *testing.T) {
	meta := &session.Session{ID: "runtime-only", Provider: "mock", Model: "test"}
	rt := &serveRuntime{sessionMeta: meta, history: []llm.Message{llm.UserText("runtime question")}}
	srv := &serveServer{sessionMgr: &serveSessionManager{sessions: map[string]*serveRuntime{meta.ID: rt}}}
	path := "/v1/sessions/" + meta.ID + "/inspect"
	first := inspectRequest(srv, "GET", path)
	d := decodeInspect(t, first)
	if d.Scope != "runtime_history" || d.TranscriptRev != -1 || len(d.Blocks) != 1 || d.Blocks[0].ID != "i0.p0.user" {
		t.Fatalf("document = %+v", d)
	}
	repeat := inspectRequest(srv, "GET", path)
	if first.Header().Get("ETag") != repeat.Header().Get("ETag") {
		t.Fatal("runtime fallback ETag is unstable")
	}
	rt.mu.Lock()
	busy := inspectRequest(srv, "GET", path)
	rt.mu.Unlock()
	if decodeInspect(t, busy).Header.Runtime.UnavailableReason != "runtime_busy" {
		t.Fatal("busy history fallback")
	}
}

func TestServeInspectBodiesValidationAndIsolation(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	msg := appendInspectMessage(t, store, sess.ID, llm.UserText(strings.Repeat("世", inspect.MaxBodyBytes)))
	id := fmt.Sprintf("m%d.p0.user", msg.ID)
	other := &session.Session{ID: "other", Provider: "mock", Model: "test"}
	if err := store.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	foreign := appendInspectMessage(t, store, other.ID, llm.UserText("other session secret"))
	foreignID := fmt.Sprintf("m%d.p0.user", foreign.ID)
	path := "/v1/sessions/" + sess.ID + "/inspect/bodies?ids="
	for _, raw := range []string{"../secret", "m1.p0.user/../../x", "runtime.tool.-1", "m1.p0.user,", "m1.p-1.user", strings.Repeat("m1.p0.user,", 64) + "m1.p0.user", strings.Repeat("x", inspectBodiesMaxQueryBytes+1)} {
		t.Run(fmt.Sprintf("invalid-%d-%s", len(raw), raw[:min(16, len(raw))]), func(t *testing.T) {
			rr := inspectRequest(srv, "GET", path+url.QueryEscape(raw))
			if rr.Code != 400 {
				t.Fatalf("invalid IDs: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	rr := inspectRequest(srv, "GET", path+strings.Join([]string{id, foreignID, "m99999.p0.user"}, ","))
	var got inspect.BodiesResponse
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil {
		t.Fatalf("bodies: %d %s", rr.Code, rr.Body.String())
	}
	if len(got.Bodies) != 1 || len(got.Missing) != 2 || got.Missing[0] != foreignID || strings.Contains(rr.Body.String(), "other session secret") {
		t.Fatalf("session isolation failed: missing=%v bodies=%d", got.Missing, len(got.Bodies))
	}
	body := got.Bodies[0]
	if !body.Truncated || len(body.Text) > inspect.MaxBodyBytes || !utf8.ValidString(body.Text) || body.TotalBytes != len(strings.Repeat("世", inspect.MaxBodyBytes)) {
		t.Fatal("invalid truncation")
	}
}

func TestServeInspectValidatedChildren(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	for _, child := range []*session.Session{{ID: "another-parent"}, {ID: "child", ParentID: sess.ID}, {ID: "stranger", ParentID: "another-parent"}} {
		if err := store.Create(context.Background(), child); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, content string
		want          []string
	}{
		{"spawn_agent", `{"session_id":"child"}`, []string{"child"}},
		{"spawn_agent", `{"session_id":"stranger"}`, nil},
		{"spawn_agent", `{"session_id":"does-not-exist"}`, nil},
		{"wait_agent", `[{"session_id":"stranger"},{"session_id":"child"},{"agent_id":"child"}]`, []string{"child"}},
		{"continue_agent", `{"agent_id":"child"}`, []string{"child"}},
		{"continue_agent", `{"session_id":"stranger"}`, nil},
		{"other_tool", `{"session_id":"child"}`, nil},
	} {
		t.Run(tt.name+tt.content, func(t *testing.T) {
			msg := appendInspectMessage(t, store, sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{Name: tt.name, Content: tt.content}}}})
			d := decodeInspect(t, inspectRequest(srv, "GET", "/v1/sessions/"+sess.ID+"/inspect"))
			block := d.Blocks[len(d.Blocks)-1]
			if block.MessageID != fmt.Sprint(msg.ID) || !reflect.DeepEqual(block.ChildSessionIDs, tt.want) {
				t.Fatalf("block = %+v want children %v", block, tt.want)
			}
		})
	}
}

func TestServeInspectNotInShares(t *testing.T) {
	srv, anchor := newServeShareFixture(t)
	appendInspectMessage(t, srv.store, "share-session", llm.Message{Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "INSPECT_ONLY_SYSTEM"}}})
	srv.sessionMgr = &serveSessionManager{sessions: map[string]*serveRuntime{"share-session": {systemPrompt: "INSPECT_ONLY_RUNTIME"}}}
	decodeInspect(t, inspectRequest(srv, "GET", "/v1/sessions/share-session/inspect"))
	publisher := &serveSharePublisherMock{}
	srv.sharePublisherFactory = func() (share.Publisher, error) { return publisher, nil }
	rr := serveShareRequest(t, srv, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"response","public":true}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("share %d: %s", rr.Code, rr.Body.String())
	}
	for name, content := range publisher.files {
		if strings.Contains(content, "INSPECT_ONLY") || strings.Contains(content, "runtime.system_prompt") || strings.Contains(content, `"schema_version"`) {
			t.Fatalf("share %s contains inspect data", name)
		}
	}
}

func TestServeInspectBodiesAggregateBudget(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	text := strings.Repeat("x", inspect.MaxBodyBytes+10)
	var ids []string
	for range 10 {
		msg := appendInspectMessage(t, store, sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{Name: "read_file", Content: text}}}})
		ids = append(ids, fmt.Sprintf("m%d.p0.tool_result", msg.ID))
	}
	rr := inspectRequest(srv, "GET", "/v1/sessions/"+sess.ID+"/inspect/bodies?ids="+strings.Join(ids, ","))
	var got inspect.BodiesResponse
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil {
		t.Fatalf("status %d", rr.Code)
	}
	if len(got.Bodies) == 0 || len(got.Deferred) == 0 || len(got.Bodies)+len(got.Deferred) != len(ids) || rr.Body.Len() > inspect.MaxBodiesResponseBytes {
		t.Fatalf("counts %d/%d bytes=%d", len(got.Bodies), len(got.Deferred), rr.Body.Len())
	}
}

type inspectFailingStore struct {
	session.Store
	historyErr, revisionErr error
}

func (s inspectFailingStore) GetMessages(ctx context.Context, id string, limit, offset int) ([]session.Message, error) {
	if s.historyErr != nil {
		return nil, s.historyErr
	}
	return s.Store.GetMessages(ctx, id, limit, offset)
}
func (s inspectFailingStore) TranscriptRev(context.Context, string) (int64, error) {
	return 0, s.revisionErr
}

func TestServeInspectStoreCapabilitiesAndErrors(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	path := "/v1/sessions/" + sess.ID + "/inspect"
	for _, tt := range []struct {
		name   string
		store  session.Store
		status int
	}{
		{"no revision capability", &capabilitylessTranscriptStore{Store: store}, 200},
		{"history error", inspectFailingStore{Store: store, historyErr: errors.New("read failure")}, 500},
		{"revision error", inspectFailingStore{Store: store, revisionErr: errors.New("revision failure")}, 500},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv.store = tt.store
			rr := inspectRequest(srv, "GET", path)
			if rr.Code != tt.status {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if tt.status == 200 && decodeInspect(t, rr).TranscriptRev != -1 {
				t.Fatal("missing capability must return rev -1")
			}
		})
	}
}

func TestParseInspectBodyIDs(t *testing.T) {
	for _, raw := range []string{"", "m1.p0.user", "m0.compaction_boundary", "i9.p4.reasoning", "runtime.system_prompt,runtime.tool.12", "m1.p0.user,m1.p0.user"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseInspectBodyIDs(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeInspectModelTimeline(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	sess.Provider, sess.ProviderKey, sess.Model = "Claude label", "claude-bin", "opus-max"
	sess.ReasoningMode, sess.Agent = "pro", "reviewer"
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatalf("update inspect model metadata: %v", err)
	}
	marker := llm.ModelSwapMarker{FromProvider: "openai", FromModel: "gpt-5-high", FromAgent: "dev", ToProvider: "claude-bin", ToModel: "opus-max", ToAgent: "reviewer", Strategy: "handover", Status: "succeeded"}
	appendInspectMessage(t, store, sess.ID, llm.ModelSwapEventMessage(marker))
	for _, message := range llm.ReconstructHandoverHistory("", "outgoing briefing", "openai:gpt-5-high", "claude-bin:opus-max") {
		appendInspectMessage(t, store, sess.ID, message)
	}
	path := "/v1/sessions/" + sess.ID + "/inspect"
	d := decodeInspect(t, inspectRequest(srv, "GET", path))
	if d.Header.InitialModel != (inspect.ModelInfo{Provider: "openai", Model: "gpt-5", Effort: "high", Agent: "dev"}) || d.Header.CurrentModel != (inspect.ModelInfo{Provider: "claude-bin", Model: "opus", Effort: "max", ReasoningMode: "pro", Agent: "reviewer"}) {
		t.Fatalf("header models = %+v / %+v", d.Header.InitialModel, d.Header.CurrentModel)
	}
	if len(d.Blocks) != 3 {
		t.Fatalf("blocks = %+v", d.Blocks)
	}
	change, doc, ack := d.Blocks[0], d.Blocks[1], d.Blocks[2]
	if change.Kind != "model_change" || change.ModelChange == nil || change.ModelChange.To.Agent != "reviewer" || change.Title != llm.FormatModelSwapMarker(marker) {
		t.Fatalf("change = %+v", change)
	}
	if doc.Kind != "handover_document" || doc.Handover == nil || doc.Handover.Source != "openai:gpt-5-high" || !ack.Synthetic {
		t.Fatalf("handover = %+v / %+v", doc, ack)
	}
	rr := inspectRequest(srv, "GET", path+"/bodies?ids="+change.ID+","+doc.ID)
	var bodies inspect.BodiesResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &bodies) != nil || len(bodies.Bodies) != 2 {
		t.Fatalf("bodies: %d %s", rr.Code, rr.Body.String())
	}
	if bodies.Bodies[0].Text != llm.FormatModelSwapMarker(marker) || bodies.Bodies[1].Text != "outgoing briefing" {
		t.Fatalf("model timeline bodies = %+v", bodies.Bodies)
	}
}

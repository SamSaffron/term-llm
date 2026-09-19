package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// voiceNewSessionHarness is one live call bound to a durable conversation on a
// server with everything a new conversation needs: a durable store, a session
// manager that hands out mock runtimes, and the project registry. The call's
// provider session is the stub, so the host note a switch appends is readable.
type voiceNewSessionHarness struct {
	server *serveServer
	store  *session.SQLiteStore
	record *liveSession
	audio  *stubLiveSession
	events serveEventSubscription
}

// newVoiceNewSessionHarness subscribes to the server's events before anything
// runs, so the sidebar publication a created conversation makes is observed
// rather than assumed.
func newVoiceNewSessionHarness(t *testing.T, sourceID string, agentNames ...string) *voiceNewSessionHarness {
	t.Helper()
	store := newSessionDirectoryTestStore(t)
	srv := newTestServeServer()
	srv.store = store
	srv.projectsEnabled = true
	srv.startupDir = t.TempDir()
	srv.cfg = serveServerConfig{ui: true, agentNames: agentNames}
	// Creating a conversation is a control-lane capability, and the authority that
	// makes it callable is installed only while the control plane is on.
	srv.cfgRef = &config.Config{Live: config.LiveConfig{ControlPlane: config.LiveControlPlaneAgent}}
	// The runtime production builds for a named agent, minus the work: it reports
	// the agent it was asked for, which is what the created row has to carry.
	srv.agentRuntimeFactory = func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		return &serveRuntime{
			provider: provider, providerKey: "mock", defaultModel: "mock-model",
			agentName: strings.TrimSpace(request.Agent), engine: llm.NewEngine(provider, nil), store: store,
		}, nil
	}
	audio := &stubLiveSession{events: make(chan live.Event, 8), closed: make(chan struct{})}
	record := newLiveSession("call-new-session", sourceID)
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	record.providerSession = audio
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	subscription, err := srv.ensureEventBroker().Subscribe(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &voiceNewSessionHarness{server: srv, store: store, record: record, audio: audio, events: subscription}
}

// project registers a project whose canonical directory really exists, so the
// host's own availability check accepts it.
func (h *voiceNewSessionHarness) project(t *testing.T, name string) session.Project {
	t.Helper()
	project := session.Project{Name: name, CanonicalDir: t.TempDir()}
	if err := h.store.CreateProject(context.Background(), &project); err != nil {
		t.Fatal(err)
	}
	return project
}

// conversation creates the chat session the live call is bound to.
func (h *voiceNewSessionHarness) conversation(t *testing.T, sess *session.Session) *session.Session {
	t.Helper()
	createDirectorySession(t, h.store, sess)
	return sess
}

// start starts a conversation the way the router does: through the tool, under the
// authority context the host installs for a routing turn. Nothing about the binding
// is simulated, so a host that fails to install the new binding fails here with a
// permission denial instead of passing.
func (h *voiceNewSessionHarness) start(t *testing.T, args string) (string, error) {
	t.Helper()
	binding := h.record.boundSession()
	ctx := h.server.withLiveControlAuthority(context.Background(), h.record, binding)
	out, err := (&tools.LiveNewSessionTool{}).Execute(ctx, json.RawMessage(args))
	return out.Content, err
}

// createdSessions lists the conversation ids the sidebar was told about.
func (h *voiceNewSessionHarness) createdSessions() []string {
	var ids []string
	for {
		select {
		case event := <-h.events.Events:
			if event.Type == serveEventSessionCreated {
				ids = append(ids, event.SessionID)
			}
		default:
			return ids
		}
	}
}

// changedBindings lists the bindings the browser was told about, in order.
func (h *voiceNewSessionHarness) changedBindings() []string {
	h.record.mu.Lock()
	defer h.record.mu.Unlock()
	var ids []string
	for _, event := range h.record.events {
		if event.Type == liveEventSessionChanged {
			ids = append(ids, fmt.Sprint(event.Data["session_id"]))
		}
	}
	return ids
}

func (h *voiceNewSessionHarness) messageCount(t *testing.T, sessionID string) int {
	t.Helper()
	messages, err := h.store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(messages)
}

// durableSessionIDs lists the conversations the directory holds, which is how a
// refused request is shown to have created nothing.
func durableSessionIDs(t *testing.T, srv *serveServer) []string {
	t.Helper()
	entries, err := srv.sessionDirectory(context.Background(), sessionDirectoryQuery{Limit: sessionDirectoryMaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

// TestLiveNewSessionCreatesAndBindsAConversationTheBrowserWouldRecognise pins the
// whole of live_new_session in one pass: an omitted project and agent stay where the
// user is, the created row is the one the browser creates, the call is bound to it
// through the ordinary switch path, and the conversation it left is untouched.
func TestLiveNewSessionCreatesAndBindsAConversationTheBrowserWouldRecognise(t *testing.T) {
	t.Run("in the conversation's project", func(t *testing.T) {
		h := newVoiceNewSessionHarness(t, "voice-source", "reviewer", "researcher")
		registered := h.project(t, "Reflow work")
		source := h.conversation(t, &session.Session{
			ID: "voice-source", GeneratedShortTitle: "Voice work", Agent: "reviewer",
			ProjectID: registered.ID, ProjectName: registered.Name, CWD: registered.CanonicalDir,
		})
		addDirectoryMessage(t, h.store, source.ID, "the work in progress")
		before := h.messageCount(t, source.ID)

		content, err := h.start(t, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			SessionID     string `json:"session_id"`
			SessionNumber int64  `json:"session_number"`
			Project       string `json:"project"`
			Agent         string `json:"agent"`
		}
		if err := json.Unmarshal([]byte(content), &result); err != nil {
			t.Fatalf("output is not JSON: %s", content)
		}
		if result.SessionID == "" || result.SessionID == source.ID || result.SessionNumber <= 0 {
			t.Fatalf("result = %+v", result)
		}
		// Omitted project and agent: the new conversation is the same place, run by
		// the same agent, so "start a new conversation" needs no arguments.
		if result.Project != "Reflow work" || result.Agent != "reviewer" {
			t.Fatalf("inherited result = %+v", result)
		}
		if bound := h.record.boundSession(); bound != result.SessionID {
			t.Fatalf("binding = %q, want the new conversation %q", bound, result.SessionID)
		}
		if changed := h.changedBindings(); len(changed) != 1 || changed[0] != result.SessionID {
			t.Fatalf("live.session_changed = %v, want the new conversation once", changed)
		}
		// The host note is what makes the voice model stop naming the session it
		// left, so it has to name the one that replaced it.
		note := h.audio.lastAppendedText()
		if !strings.Contains(note, fmt.Sprintf("#%d", result.SessionNumber)) || !strings.Contains(note, result.SessionID) {
			t.Fatalf("host note = %q", note)
		}
		if created := h.createdSessions(); len(created) != 1 || created[0] != result.SessionID {
			t.Fatalf("session.created = %v, want the new conversation once", created)
		}
		// The conversation the call left is not the one that was changed.
		if after := h.messageCount(t, source.ID); after != before {
			t.Fatalf("the conversation left behind was written to: %d messages, want %d", after, before)
		}

		row, err := h.store.Get(context.Background(), result.SessionID)
		if err != nil || row == nil {
			t.Fatalf("created row = %#v, %v", row, err)
		}
		if row.Provider == "" || row.Model == "" || row.Mode != session.ModeChat ||
			row.Origin != session.OriginWeb || row.Status != session.StatusActive || row.MessageCount != 0 {
			t.Fatalf("created row is not a blank web conversation: %+v", row)
		}
		if row.ProjectID != registered.ID || row.ProjectName != registered.Name || !sameServePath(row.CWD, registered.CanonicalDir) {
			t.Fatalf("created row workspace = %+v, want %s", row, registered.ID)
		}
		if row.Agent != "reviewer" {
			t.Fatalf("created row agent = %q", row.Agent)
		}
	})

	t.Run("outside every project", func(t *testing.T) {
		// A conversation outside every project keeps the new one outside them too, in
		// the default workspace, rather than being refused for the missing project the
		// browser is asked to choose.
		h := newVoiceNewSessionHarness(t, "voice-unprojected")
		h.conversation(t, &session.Session{ID: "voice-unprojected", GeneratedShortTitle: "Loose work"})
		content, err := h.start(t, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			SessionID string `json:"session_id"`
			Project   string `json:"project"`
		}
		if err := json.Unmarshal([]byte(content), &result); err != nil {
			t.Fatal(err)
		}
		row, err := h.store.Get(context.Background(), result.SessionID)
		if err != nil || row == nil {
			t.Fatalf("created row = %#v, %v", row, err)
		}
		if row.ProjectID != "" || result.Project != "" {
			t.Fatalf("the new conversation was given a project: %+v", row)
		}
		if !sameServePath(row.CWD, h.server.startupDir) {
			t.Fatalf("no-project workspace = %q, want the default workspace %q", row.CWD, h.server.startupDir)
		}
		if h.record.boundSession() != result.SessionID {
			t.Fatalf("binding = %q", h.record.boundSession())
		}
	})
}

// TestLiveNewSessionResolvesNamedProjectsAndAgents covers the arguments the voice
// model passes through from speech: a project named loosely, an agent named in the
// wrong case. Both are resolved to the registry's own identity, and an exact name
// wins over another project that merely contains it.
func TestLiveNewSessionResolvesNamedProjectsAndAgents(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        string
		wantProject string
		wantAgent   string
	}{
		{name: "exact name", args: `{"project":"Reflow work"}`, wantProject: "Reflow work"},
		{name: "case-insensitive name", args: `{"project":"eLsEwHeRe"}`, wantProject: "Elsewhere"},
		{name: "unique substring", args: `{"project":"work"}`, wantProject: "Reflow work"},
		{name: "exact name beats a superset", args: `{"project":"Reflow"}`, wantProject: "Reflow"},
		{name: "agent in the wrong case", args: `{"agent":"REVIEWER"}`, wantProject: "Reflow", wantAgent: "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceNewSessionHarness(t, "voice-named", "reviewer", "researcher")
			exact := h.project(t, "Reflow")
			superset := h.project(t, "Reflow work")
			h.project(t, "Elsewhere")
			h.conversation(t, &session.Session{
				ID: "voice-named", GeneratedShortTitle: "Voice work", Agent: "researcher",
				ProjectID: exact.ID, ProjectName: exact.Name, CWD: exact.CanonicalDir,
			})

			content, err := h.start(t, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				SessionID string `json:"session_id"`
				Project   string `json:"project"`
				Agent     string `json:"agent"`
			}
			if err := json.Unmarshal([]byte(content), &result); err != nil {
				t.Fatal(err)
			}
			if result.Project != tc.wantProject {
				t.Fatalf("project = %q, want %q", result.Project, tc.wantProject)
			}
			row, err := h.store.Get(context.Background(), result.SessionID)
			if err != nil || row == nil {
				t.Fatalf("created row = %#v, %v", row, err)
			}
			if row.ProjectName != tc.wantProject {
				t.Fatalf("persisted project = %q, want %q", row.ProjectName, tc.wantProject)
			}
			if tc.wantProject == "Reflow work" && row.ProjectID != superset.ID {
				t.Fatalf("substring resolved to the wrong project: %+v", row)
			}
			// An omitted agent inherits the conversation being left; a named one is
			// answered with the registry's spelling, which is what the row carries.
			wantAgent := tc.wantAgent
			if wantAgent == "" {
				wantAgent = "researcher"
			}
			if result.Agent != wantAgent || row.Agent != wantAgent {
				t.Fatalf("agent = %q / row %q, want %q", result.Agent, row.Agent, wantAgent)
			}
		})
	}
}

// TestLiveNewSessionRefusesUnresolvableNames is the "never guess" rule. A name the
// host cannot resolve to exactly one project, or to an agent at all, is refused with
// what it could have meant, and nothing is created or moved.
func TestLiveNewSessionRefusesUnresolvableNames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   string
		pinned string
		want   []string
	}{
		{
			name: "unknown project", args: `{"project":"Reflows"}`,
			want: []string{`no "Reflows" project`, "Reflow work (", "Elsewhere ("},
		},
		{
			name: "ambiguous project", args: `{"project":"reflow"}`,
			want: []string{"matches more than one project", "Reflow work (", "Reflow decisions ("},
		},
		{
			name: "unknown agent", args: `{"agent":"reviewerr"}`,
			want: []string{`no "reviewerr" agent`, "researcher", "reviewer"},
		},
		{
			name: "pinned agent conflict", args: `{"agent":"researcher"}`, pinned: "reviewer",
			want: []string{`runs every conversation with the "reviewer" agent`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceNewSessionHarness(t, "voice-refused", "reviewer", "researcher")
			reflow := h.project(t, "Reflow work")
			h.project(t, "Reflow decisions")
			h.project(t, "Elsewhere")
			h.server.cfg.agentName = tc.pinned
			source := h.conversation(t, &session.Session{
				ID: "voice-refused", GeneratedShortTitle: "Voice work", Agent: "researcher",
				ProjectID: reflow.ID, ProjectName: reflow.Name, CWD: reflow.CanonicalDir,
			})

			_, err := h.start(t, tc.args)
			if err == nil {
				t.Fatal("an unresolvable name was accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing %q", err, want)
				}
			}
			if bound := h.record.boundSession(); bound != source.ID {
				t.Fatalf("the call moved to %q on a refused request", bound)
			}
			if ids := durableSessionIDs(t, h.server); len(ids) != 1 || ids[0] != source.ID {
				t.Fatalf("a refused request created conversations: %v", ids)
			}
		})
	}
}

// TestLiveNewSessionReportsAConversationItCouldNotBind is the failure the user has
// to hear about: the conversation exists and the call did not move. Naming it is
// what lets them ask again, so the message has to carry the durable number.
func TestLiveNewSessionReportsAConversationItCouldNotBind(t *testing.T) {
	h := newVoiceNewSessionHarness(t, "voice-orphaned")
	h.conversation(t, &session.Session{ID: "voice-orphaned", GeneratedShortTitle: "Voice work"})
	// The call ends while the conversation is being created. Nothing can stop that
	// racing in production, and the conversation that was made must survive it.
	h.server.stopLiveSession(context.Background(), h.record.id, "test")

	_, err := h.start(t, `{}`)
	if err == nil {
		t.Fatal("a switch on an ended call succeeded")
	}
	for _, want := range []string{"was created but the call did not move", "live_switch_session", errLiveCallEnded.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	match := regexp.MustCompile(`session #(\d+)`).FindStringSubmatch(err.Error())
	if match == nil {
		t.Fatalf("error does not name the created conversation: %q", err)
	}
	var number int64
	if _, scanErr := fmt.Sscanf(match[1], "%d", &number); scanErr != nil {
		t.Fatal(scanErr)
	}
	created, getErr := h.store.GetByNumber(context.Background(), number)
	if getErr != nil || created == nil {
		t.Fatalf("the conversation the error names does not exist: %#v, %v", created, getErr)
	}
	if created.MessageCount != 0 {
		t.Fatalf("the orphaned conversation is not empty: %+v", created)
	}
}

// TestLiveRouterStartsANewConversationEndToEnd drives the fifth tool the way the
// voice model reaches it: a real request through the production router, a scripted
// tool call, and the session lane asserted untouched. Starting a conversation is
// session management, so no part of it may become a chat turn.
func TestLiveRouterStartsANewConversationEndToEnd(t *testing.T) {
	h := newControlLaneHarness(t, "control-new")
	before := h.messageCount(t, "control-new")
	h.agent.
		AddToolCall("call_new", tools.LiveNewSessionToolName, map[string]any{}).
		AddTextResponse("Started a new conversation.")

	h.provider.events <- live.Event{
		Kind:         live.EventDelegationCreated,
		DelegationID: "control-new-session",
		Text:         "start a new conversation",
	}
	answer := h.commentary(t)

	if !strings.Contains(answer, "new conversation") {
		t.Fatalf("answer = %q", answer)
	}
	created := h.record.boundSession()
	if created == "" || created == "control-new" {
		t.Fatalf("binding = %q, want a new conversation", created)
	}
	h.delegator.assertSessionLaneUntouched(t)
	if after := h.messageCount(t, "control-new"); after != before {
		t.Fatalf("the request was written into the conversation it left: %d messages, want %d", after, before)
	}
	if row, err := h.store.Get(context.Background(), created); err != nil || row == nil || row.MessageCount != 0 {
		t.Fatalf("created row = %+v, %v", row, err)
	}
	// The router was told what it did, in the terms it has to say out loud.
	requests := h.agent.RecordedRequests()
	if len(requests) < 2 {
		t.Fatalf("routing turns = %d, want a tool call and its answer", len(requests))
	}
	result := toolResultText(requests[1], tools.LiveNewSessionToolName)
	for _, want := range []string{created, "session_number", "bound to this new conversation"} {
		if !strings.Contains(result, want) {
			t.Fatalf("tool result %q missing %q", result, want)
		}
	}
	// Exactly one conversation was added, and none of them is a chat turn's.
	ids := durableSessionIDs(t, h.server)
	if len(ids) != 2 {
		t.Fatalf("router created unexpected conversations: %v", ids)
	}
}

// TestHandleCreateWebSessionStillAnswersAsItAlwaysDid pins the endpoint half of the
// shared creation path. The handler now delegates creation and maps the result back,
// so every refusal it always produced is asserted here by status and error code.
func TestHandleCreateWebSessionStillAnswersAsItAlwaysDid(t *testing.T) {
	newServer := func(t *testing.T) (*serveServer, *session.SQLiteStore) {
		t.Helper()
		manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
			provider := llm.NewMockProvider("mock")
			runtime := &serveRuntime{provider: provider, providerKey: "mock", defaultModel: "default"}
			runtime.Touch()
			return runtime, nil
		})
		t.Cleanup(manager.Close)
		store := newSessionDirectoryTestStore(t)
		return &serveServer{cfg: serveServerConfig{ui: true}, store: store, sessionMgr: manager, startupDir: t.TempDir(), projectsEnabled: true}, store
	}
	for _, tc := range []struct {
		name       string
		body       string
		firstParty bool
		status     int
		code       string
	}{
		{name: "no project chosen", body: `{}`, firstParty: true, status: http.StatusBadRequest, code: "project_required"},
		{name: "project mode off", body: `{"project_id":"prj_missing"}`, firstParty: true, status: http.StatusNotFound, code: "project_not_found"},
		{name: "third party", body: `{}`, status: http.StatusForbidden, code: "invalid_origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newServer(t)
			request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader([]byte(tc.body)))
			request.Header.Set("Content-Type", "application/json")
			if tc.firstParty {
				request.Header.Set("X-Term-LLM-UI-Version", "test")
			}
			response := httptest.NewRecorder()
			srv.handleCreateWebSession(response, request)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			var payload struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body = %s", response.Body.String())
			}
			// Workspace refusals carry a code and protocol refusals a type; both are
			// the caller-visible name of the refusal.
			got := payload.Error.Code
			if got == "" {
				got = payload.Error.Type
			}
			if got != tc.code {
				t.Fatalf("error name = %q, want %q", got, tc.code)
			}
		})
	}

	t.Run("missing session persistence", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader([]byte(`{}`)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Term-LLM-UI-Version", "test")
		response := httptest.NewRecorder()
		(&serveServer{}).handleCreateWebSession(response, request)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "session persistence is unavailable") {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
	})

	t.Run("created", func(t *testing.T) {
		srv, store := newServer(t)
		root := t.TempDir()
		project := session.Project{Name: "Usable", CanonicalDir: root}
		if err := store.CreateProject(context.Background(), &project); err != nil {
			t.Fatal(err)
		}
		subscription, err := srv.ensureEventBroker().Subscribe(nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(createWebSessionRequest{ProjectID: project.ID, Model: "chosen-model"})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Term-LLM-UI-Version", "test")
		response := httptest.NewRecorder()
		srv.handleCreateWebSession(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
		var payload struct {
			Session webSessionEntry `json:"session"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		persisted, err := store.Get(context.Background(), payload.Session.ID)
		if err != nil || persisted == nil || persisted.ProjectID != project.ID || persisted.Model != "chosen-model" {
			t.Fatalf("persisted = %+v, %v", persisted, err)
		}
		// The sidebar is told by the same publication the voice lane relies on.
		published := false
		for draining := true; draining; {
			select {
			case event := <-subscription.Events:
				published = published || (event.Type == serveEventSessionCreated && event.SessionID == payload.Session.ID)
			default:
				draining = false
			}
		}
		if !published {
			t.Fatalf("created conversation was not published: %s", payload.Session.ID)
		}
	})
}

// TestCreateWebSessionClassifiesFailuresForBothCallers is the voice lane's half of
// the same contract: a failure carries either the workspace classification the HTTP
// endpoint renders as a workspace error, or a status the endpoint can send as-is.
func TestCreateWebSessionClassifiesFailuresForBothCallers(t *testing.T) {
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	srv := newTestServeServer()
	srv.store = store
	srv.projectsEnabled = true
	srv.startupDir = t.TempDir()

	if _, err := srv.createWebSession(ctx, createWebSessionRequest{}); err == nil {
		t.Fatal("a first-party conversation with no project succeeded")
	} else {
		var workspaceErr *newConversationWorkspaceError
		if !errors.As(err, &workspaceErr) {
			t.Fatalf("error = %v, want a workspace classification", err)
		}
	}
	// A registered project whose directory has vanished is refused before anything
	// is written, as the workspace error the browser already renders.
	gone := t.TempDir()
	project := session.Project{Name: "Vanished", CanonicalDir: gone}
	if err := store.CreateProject(ctx, &project); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.createWebSession(ctx, createWebSessionRequest{ProjectID: project.ID}); err == nil {
		t.Fatal("a vanished project started a conversation")
	} else {
		var workspaceErr *serveWorkspaceError
		if !errors.As(err, &workspaceErr) {
			t.Fatalf("error = %v, want a workspace error", err)
		}
	}
	// A missing store is a service-unavailable the endpoint can send unchanged,
	// rather than a panic or an anonymous 500.
	broken := &serveServer{}
	if _, err := broken.createWebSession(ctx, createWebSessionRequest{NoProject: true}); err == nil {
		t.Fatal("creation without a store succeeded")
	} else {
		var createErr *serveSessionCreateError
		if !errors.As(err, &createErr) || createErr.Status != http.StatusServiceUnavailable || createErr.Code != "session_unavailable" {
			t.Fatalf("error = %#v", err)
		}
	}
}

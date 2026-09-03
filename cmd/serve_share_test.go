package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/share"
)

type serveSharePublisherMock struct {
	visibility  share.Visibility
	description string
	files       map[string]string
	err         error
	capErr      error
	caps        share.Capabilities
	result      share.Result
	capCalls    int
	createCalls int
}

func (m *serveSharePublisherMock) Capabilities(context.Context) (share.Capabilities, error) {
	m.capCalls++
	if m.capErr != nil {
		return share.Capabilities{}, m.capErr
	}
	if m.caps.Protocol == "" {
		m.caps = share.Capabilities{
			Protocol: share.Protocol, Version: share.Version,
			Provider:          share.Provider{ID: "github", Name: "GitHub Gist"},
			Operations:        []share.Operation{share.OperationCreate, share.OperationUpdate},
			Visibilities:      []share.Visibility{share.VisibilityUnlisted, share.VisibilityPublic},
			DefaultVisibility: share.VisibilityUnlisted,
		}
	}
	return m.caps, nil
}

func (m *serveSharePublisherMock) Create(_ context.Context, req share.Request) (share.Result, error) {
	m.createCalls++
	m.description, m.visibility = req.Description, req.Visibility
	m.files = make(map[string]string, len(req.Files))
	for _, file := range req.Files {
		m.files[file.Name] = string(file.Content)
	}
	if m.err != nil {
		return share.Result{}, m.err
	}
	if m.result.ID != "" {
		return m.result, nil
	}
	return share.Result{Provider: "github", ID: "abc123", URL: "https://gisthost.github.io/?abc123/index.html", SourceURL: "https://gist.github.com/test/abc123", Visibility: req.Visibility, Ready: true}, nil
}

func (m *serveSharePublisherMock) Update(context.Context, string, share.Request) (share.Result, error) {
	return share.Result{}, errors.New("unexpected update")
}

func newServeShareFixture(t *testing.T) (*serveServer, int64) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	sess := &session.Session{
		ID: "share-session", Name: "Share test", Provider: "mock", Model: "model",
		Mode: session.ModeChat, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now,
		UserTurns: 4, LLMTurns: 4, ToolCalls: 8, InputTokens: 100, OutputTokens: 50,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		llm.UserText("private prompt"),
		llm.AssistantText("shareable answer"),
		llm.UserText("later prompt"),
	} {
		if err := store.AddMessage(context.Background(), sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &serveServer{store: store}, messages[1].ID
}

func serveShareRequest(t *testing.T, server *serveServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/share-session/shares", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.handleSessionByID(rr, req)
	return rr
}

func TestCreateSessionShareResponseUsesTextOnlyExport(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	mock := &serveSharePublisherMock{}
	server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"response","public":true}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if mock.visibility != share.VisibilityPublic || !strings.Contains(mock.description, "assistant response") {
		t.Fatalf("share request: visibility=%v description=%q", mock.visibility, mock.description)
	}
	for name, content := range mock.files {
		if !strings.Contains(content, "shareable answer") {
			t.Fatalf("%s omitted response", name)
		}
		if strings.Contains(content, "private prompt") || strings.Contains(content, "later prompt") {
			t.Fatalf("%s leaked prompt: %s", name, content)
		}
		if strings.Contains(content, "## Metrics") || strings.Contains(content, ">Activity<") {
			t.Fatalf("%s included whole-session metrics", name)
		}
	}
	for _, want := range []string{
		`"provider":"github"`, `"id":"abc123"`, `"url":"https://gisthost.github.io/?abc123/index.html"`,
		`"visibility":"public"`, `"ready":true`, `"scope":"response"`,
		`"gist_id":"abc123"`, `"preview_url":"https://gisthost.github.io/?abc123/index.html"`, `"public":true`,
	} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("response missing %s: %s", want, rr.Body.String())
		}
	}
}

func TestCreateSessionShareExplainsGitHubCLIDependency(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	server.sharePublisherFactory = func() (share.Publisher, error) {
		return nil, share.NewError(share.ErrorDependencyMissing, "GitHub sharing requires the gh CLI; install it from https://cli.github.com")
	}
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"conversation"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "requires the gh CLI") || !strings.Contains(rr.Body.String(), "cli.github.com") {
		t.Fatalf("dependency guidance missing: %s", rr.Body.String())
	}
}

func TestGenericCapabilitiesDoesNotInvokeSharingHelper(t *testing.T) {
	server, _ := newServeShareFixture(t)
	called := false
	server.sharePublisherFactory = func() (share.Publisher, error) {
		called = true
		return nil, errors.New("must not run")
	}
	rr := httptest.NewRecorder()
	server.handleCapabilities(rr, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if rr.Code != http.StatusOK || called || strings.Contains(rr.Body.String(), `"sharing"`) {
		t.Fatalf("status=%d helperCalled=%v body=%s", rr.Code, called, rr.Body.String())
	}
}

func TestSharingCapabilitiesAreAuthenticatedCuratedAndNoStore(t *testing.T) {
	server, _ := newServeShareFixture(t)
	caps := share.Capabilities{
		Protocol: share.Protocol, Version: share.Version,
		Provider:     share.Provider{ID: "acme", Name: "Acme Vault", Help: "Sign in to Acme."},
		Operations:   []share.Operation{share.OperationCreate},
		Visibilities: []share.Visibility{share.VisibilityPrivate}, DefaultVisibility: share.VisibilityPrivate,
		Notes: []string{"Private links expire."},
	}
	server.sharePublisherFactory = func() (share.Publisher, error) { return &serveSharePublisherMock{caps: caps}, nil }
	server.cfg.requireAuth, server.cfg.token = true, "secret"
	handler := server.auth(server.handleSharingCapabilities)

	unauthorized := httptest.NewRecorder()
	handler(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/sharing/capabilities", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sharing/capabilities", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", rr.Code, rr.Header().Get("Cache-Control"), rr.Body.String())
	}
	var response sharingCapabilitiesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Enabled || response.Provider.ID != "acme" || response.DefaultVisibility != share.VisibilityPrivate || response.Help != "Sign in to Acme." {
		t.Fatalf("response=%+v", response)
	}
}

func TestSharingProviderCachesCapabilitiesAndInvalidatesWhenConfigChanges(t *testing.T) {
	server, _ := newServeShareFixture(t)
	server.cfgRef = &config.Config{Share: config.ShareConfig{Provider: "command", Command: []string{"first"}}}
	first := &serveSharePublisherMock{caps: share.Capabilities{
		Protocol: share.Protocol, Version: share.Version, Provider: share.Provider{ID: "first", Name: "First"},
		Operations: []share.Operation{share.OperationCreate}, Visibilities: []share.Visibility{share.VisibilityPrivate}, DefaultVisibility: share.VisibilityPrivate,
	}}
	second := &serveSharePublisherMock{caps: share.Capabilities{
		Protocol: share.Protocol, Version: share.Version, Provider: share.Provider{ID: "second", Name: "Second"},
		Operations: []share.Operation{share.OperationCreate}, Visibilities: []share.Visibility{share.VisibilityPrivate}, DefaultVisibility: share.VisibilityPrivate,
	}}
	factoryCalls := 0
	server.sharePublisherFactory = func() (share.Publisher, error) {
		factoryCalls++
		if server.cfgRef.Share.Command[0] == "first" {
			return first, nil
		}
		return second, nil
	}
	request := func() string {
		rr := httptest.NewRecorder()
		server.handleSharingCapabilities(rr, httptest.NewRequest(http.MethodGet, "/v1/sharing/capabilities", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	if body := request(); !strings.Contains(body, `"id":"first"`) {
		t.Fatalf("first body=%s", body)
	}
	_ = request()
	if factoryCalls != 1 || first.capCalls != 1 {
		t.Fatalf("cache calls factory=%d capabilities=%d", factoryCalls, first.capCalls)
	}
	server.cfgRef.Share.Command[0] = "second"
	if body := request(); !strings.Contains(body, `"id":"second"`) {
		t.Fatalf("second body=%s", body)
	}
	if factoryCalls != 2 || second.capCalls != 1 {
		t.Fatalf("rebuild calls factory=%d capabilities=%d", factoryCalls, second.capCalls)
	}
}

type blockingServeSharePublisher struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (p *blockingServeSharePublisher) Capabilities(context.Context) (share.Capabilities, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
	}
	<-p.release
	return share.Capabilities{
		Protocol: share.Protocol, Version: share.Version, Provider: share.Provider{ID: "blocking", Name: "Blocking"},
		Operations: []share.Operation{share.OperationCreate}, Visibilities: []share.Visibility{share.VisibilityPrivate}, DefaultVisibility: share.VisibilityPrivate,
	}, nil
}

func (p *blockingServeSharePublisher) Create(context.Context, share.Request) (share.Result, error) {
	return share.Result{}, errors.New("not used")
}

func TestSharingProviderSingleflightsConcurrentCapabilities(t *testing.T) {
	server, _ := newServeShareFixture(t)
	publisher := &blockingServeSharePublisher{started: make(chan struct{}), release: make(chan struct{})}
	var factoryCalls atomic.Int32
	server.sharePublisherFactory = func() (share.Publisher, error) {
		factoryCalls.Add(1)
		return publisher, nil
	}
	const callers = 8
	errorsCh := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, _, err := server.sharingProvider(context.Background())
			errorsCh <- err
		}()
	}
	<-publisher.started
	close(publisher.release)
	for i := 0; i < callers; i++ {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if factoryCalls.Load() != 1 || publisher.calls.Load() != 1 {
		t.Fatalf("factory calls=%d capabilities calls=%d", factoryCalls.Load(), publisher.calls.Load())
	}
}

func TestShareInvocationConcurrencyIsBounded(t *testing.T) {
	server := &serveServer{}
	var active atomic.Int32
	var maximum atomic.Int32
	start := make(chan struct{})
	entered := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.withShareInvocation(context.Background(), func() error {
				current := active.Add(1)
				for {
					old := maximum.Load()
					if current <= old || maximum.CompareAndSwap(old, current) {
						break
					}
				}
				entered <- struct{}{}
				<-start
				active.Add(-1)
				return nil
			}); err != nil {
				t.Errorf("withShareInvocation: %v", err)
			}
		}()
	}
	<-entered
	close(start)
	wg.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent invocations=%d, want 1", maximum.Load())
	}
}

func TestCreateSessionShareUsesCustomProviderWithoutPersistenceOrGistFields(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	caps := share.Capabilities{
		Protocol: share.Protocol, Version: share.Version,
		Provider:     share.Provider{ID: "acme", Name: "Acme Vault"},
		Operations:   []share.Operation{share.OperationCreate},
		Visibilities: []share.Visibility{share.VisibilityPrivate}, DefaultVisibility: share.VisibilityPrivate,
	}
	mock := &serveSharePublisherMock{caps: caps, result: share.Result{
		Provider: "acme", ID: "opaque", URL: "https://share.example/opaque",
		Visibility: share.VisibilityPrivate, Ready: true,
	}}
	server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"conversation","visibility":"private"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"gist_id", "gist_url", "preview_url", `"public"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("custom response exposed legacy field %q: %s", forbidden, body)
		}
	}
	persisted, err := server.store.Get(context.Background(), "share-session")
	if err != nil || persisted.Share != nil {
		t.Fatalf("point-in-time share was persisted: share=%+v err=%v", persisted.Share, err)
	}
}

func TestCreateSessionShareGitHubPathIsAlsoNotPersisted(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	mock := &serveSharePublisherMock{}
	server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"response"}`)
	if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"provider":"github"`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	persisted, err := server.store.Get(context.Background(), "share-session")
	if err != nil || persisted.Share != nil {
		t.Fatalf("GitHub point-in-time share was persisted: share=%+v err=%v", persisted.Share, err)
	}
}

func TestCreateSessionShareRejectsUnsupportedVisibilityBeforeCreate(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	caps := share.Capabilities{
		Protocol: share.Protocol, Version: share.Version,
		Provider:     share.Provider{ID: "acme", Name: "Acme Vault"},
		Operations:   []share.Operation{share.OperationCreate},
		Visibilities: []share.Visibility{share.VisibilityPrivate}, DefaultVisibility: share.VisibilityPrivate,
	}
	mock := &serveSharePublisherMock{caps: caps}
	server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"response","visibility":"public"}`)
	if rr.Code != http.StatusBadRequest || mock.createCalls != 0 || !strings.Contains(rr.Body.String(), string(share.ErrorUnsupportedVisibility)) {
		t.Fatalf("status=%d calls=%d body=%s", rr.Code, mock.createCalls, rr.Body.String())
	}
}

func TestCreateSessionShareCuratesProviderError(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	mock := &serveSharePublisherMock{err: share.NewError(share.ErrorProvider, "sharing provider rejected the request")}
	server.sharePublisherFactory = func() (share.Publisher, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"response"}`)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "sharing provider rejected") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateSessionShareRejectsInvalidAnchorAndMethodBeforeStartingProvider(t *testing.T) {
	server, _ := newServeShareFixture(t)
	called := false
	server.sharePublisherFactory = func() (share.Publisher, error) {
		called = true
		return nil, errors.New("must not run")
	}
	rr := serveShareRequest(t, server, `{"anchor_message_id":999999,"scope":"response"}`)
	if rr.Code != http.StatusBadRequest || called {
		t.Fatalf("anchor status=%d helperCalled=%v body=%s", rr.Code, called, rr.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/share-session/shares", nil)
	rr = httptest.NewRecorder()
	server.handleSessionByID(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("method status=%d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/serve/sessionactivity"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionActivityRouteReusesDirectChildProjection(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, entry := range []*session.Session{
		{ID: "parent", Provider: "debug", Model: "test", Mode: session.ModeChat},
		{ID: "child", ParentID: "parent", Provider: "debug", Model: "actual-model", Mode: session.ModeChat, Status: session.StatusComplete},
		{ID: "nested", ParentID: "child", Provider: "debug", Model: "nested-model", Mode: session.ModeChat, Status: session.StatusComplete},
		{ID: "unrelated", Provider: "debug", Model: "test", Mode: session.ModeChat},
	} {
		if err := store.Create(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpdateMetrics(ctx, "nested", 0, 0, 10, 20, 30, 40); err != nil {
		t.Fatal(err)
	}
	runs := newServeResponseRunManager()
	defer runs.Close()
	server := &serveServer{store: store, responseRuns: runs}
	request := linkeddevices.WithDevicePrincipal(httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil), linkeddevices.DevicePrincipal{DeviceID: "phone", Scope: linkeddevices.ScopeWebV1})
	recorder := httptest.NewRecorder()
	server.handleSessionByID(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("route status %d: %s", recorder.Code, recorder.Body.String())
	}
	var activity sessionactivity.SessionActivityResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &activity); err != nil {
		t.Fatal(err)
	}
	if len(activity.Agents) != 2 || activity.JobsAvailability != "restricted" {
		t.Fatalf("activity = %+v", activity)
	}
	nested := activity.Agents[1]
	if nested.SessionID != "nested" || nested.Model != "nested-model" || nested.InputTokens != 10 || nested.OutputTokens != 20 || nested.CachedInputTokens != 30 || nested.CacheWriteTokens != 40 {
		t.Fatalf("nested projection: %+v", nested)
	}
	direct := httptest.NewRecorder()
	server.handleSessionByID(direct, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/children", nil))
	var children struct {
		Children []childRunProjection `json:"children"`
	}
	if err := json.Unmarshal(direct.Body.Bytes(), &children); err != nil {
		t.Fatal(err)
	}
	if len(children.Children) != 1 || children.Children[0].SessionID != "child" {
		t.Fatalf("direct children semantics changed: %+v", children)
	}
}

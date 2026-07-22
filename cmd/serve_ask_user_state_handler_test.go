package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	planpkg "github.com/samsaffron/term-llm/internal/plan"
	"github.com/samsaffron/term-llm/internal/session"
)

type sessionStatePlanStore struct {
	session.NoopStore
	snapshots map[string]planpkg.Snapshot
	versions  map[string]int64
	messages  map[string][]session.Message
	loadErr   error
}

func newSessionStatePlanStore() *sessionStatePlanStore {
	return &sessionStatePlanStore{
		snapshots: make(map[string]planpkg.Snapshot),
		versions:  make(map[string]int64),
		messages:  make(map[string][]session.Message),
	}
}

func (s *sessionStatePlanStore) LoadPlanSnapshot(_ context.Context, sessionID string) (planpkg.Snapshot, int64, error) {
	if s.loadErr != nil {
		return planpkg.Snapshot{}, 0, s.loadErr
	}
	return s.snapshots[sessionID], s.versions[sessionID], nil
}

func (s *sessionStatePlanStore) GetMessages(_ context.Context, sessionID string, _, _ int) ([]session.Message, error) {
	return append([]session.Message(nil), s.messages[sessionID]...), nil
}

func (s *sessionStatePlanStore) SavePlanSnapshot(_ context.Context, sessionID string, snapshot planpkg.Snapshot) (int64, error) {
	s.versions[sessionID]++
	s.snapshots[sessionID] = snapshot
	return s.versions[sessionID], nil
}

func (s *sessionStatePlanStore) DeletePlanSnapshot(_ context.Context, sessionID string) error {
	delete(s.snapshots, sessionID)
	delete(s.versions, sessionID)
	return nil
}

func getSessionStateResponse(t *testing.T, store session.Store, sessionID string) map[string]json.RawMessage {
	t.Helper()
	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID+"/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

type currentPlanResponse struct {
	Version     int64          `json:"version"`
	Steps       []planpkg.Step `json:"steps"`
	Explanation string         `json:"explanation,omitempty"`
}

func TestHandleSessionStateReturnsLatestAuthoritativePlan(t *testing.T) {
	store := newSessionStatePlanStore()
	first := planpkg.Snapshot{Plan: []planpkg.Step{
		{Step: "Inspect flow", Status: planpkg.StatusInProgress},
		{Step: "Add tests", Status: planpkg.StatusPending},
	}}
	latest := planpkg.Snapshot{Explanation: "  Ready to implement.  ", Plan: []planpkg.Step{
		{Step: "  Inspect flow  ", Status: planpkg.StatusCompleted},
		{Step: "Add tests", Status: planpkg.StatusInProgress},
	}}
	if _, err := store.SavePlanSnapshot(context.Background(), "session-a", first); err != nil {
		t.Fatalf("save first plan: %v", err)
	}
	version, err := store.SavePlanSnapshot(context.Background(), "session-a", latest)
	if err != nil {
		t.Fatalf("save latest plan: %v", err)
	}

	response := getSessionStateResponse(t, store, "session-a")
	raw, ok := response["current_plan"]
	if !ok {
		t.Fatal("current_plan field omitted for capable store")
	}
	var got currentPlanResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode current_plan: %v", err)
	}
	want := currentPlanResponse{
		Version: version,
		Steps: []planpkg.Step{
			{Step: "Inspect flow", Status: planpkg.StatusCompleted},
			{Step: "Add tests", Status: planpkg.StatusInProgress},
		},
		Explanation: "Ready to implement.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("current_plan = %#v, want %#v", got, want)
	}
}

func TestHandleSessionStatePlanCapabilitySemantics(t *testing.T) {
	t.Run("capable absent and cleared are null", func(t *testing.T) {
		store := newSessionStatePlanStore()
		response := getSessionStateResponse(t, store, "session-a")
		if raw, ok := response["current_plan"]; !ok || string(raw) != "null" {
			t.Fatalf("current_plan = %q, present=%v; want explicit null", raw, ok)
		}

		if _, err := store.SavePlanSnapshot(context.Background(), "session-a", planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Work", Status: planpkg.StatusCompleted}}}); err != nil {
			t.Fatalf("save plan: %v", err)
		}
		if err := store.DeletePlanSnapshot(context.Background(), "session-a"); err != nil {
			t.Fatalf("clear plan: %v", err)
		}
		response = getSessionStateResponse(t, store, "session-a")
		if raw, ok := response["current_plan"]; !ok || string(raw) != "null" {
			t.Fatalf("current_plan after clear = %q, present=%v; want explicit null", raw, ok)
		}
	})

	t.Run("unsupported store omits field", func(t *testing.T) {
		response := getSessionStateResponse(t, &session.NoopStore{}, "session-a")
		if raw, ok := response["current_plan"]; ok {
			t.Fatalf("current_plan = %s; want field omitted", raw)
		}
	})

	t.Run("capable logged store returns plan", func(t *testing.T) {
		base := newSessionStatePlanStore()
		version, err := base.SavePlanSnapshot(context.Background(), "session-a", planpkg.Snapshot{
			Plan: []planpkg.Step{{Step: "Wrapped plan", Status: planpkg.StatusInProgress}},
		})
		if err != nil {
			t.Fatalf("save plan: %v", err)
		}
		response := getSessionStateResponse(t, session.NewLoggingStore(base, nil), "session-a")
		var got currentPlanResponse
		if err := json.Unmarshal(response["current_plan"], &got); err != nil {
			t.Fatalf("decode current_plan: %v", err)
		}
		if got.Version != version || len(got.Steps) != 1 || got.Steps[0].Step != "Wrapped plan" {
			t.Fatalf("current_plan = %#v, want delegated wrapped plan at version %d", got, version)
		}
	})

	t.Run("unsupported logged store omits field", func(t *testing.T) {
		store := session.NewLoggingStore(&session.NoopStore{}, nil)
		response := getSessionStateResponse(t, store, "session-a")
		if raw, ok := response["current_plan"]; ok {
			t.Fatalf("current_plan = %s; want field omitted", raw)
		}
	})

	t.Run("load failure is not an authoritative clear", func(t *testing.T) {
		store := newSessionStatePlanStore()
		store.loadErr = errors.New("read failed")
		response := getSessionStateResponse(t, store, "session-a")
		if raw, ok := response["current_plan"]; ok {
			t.Fatalf("current_plan = %s; want field omitted on read failure", raw)
		}
	})
}

func TestHandleSessionStateReturnsPlanWhenRestoredTranscriptPageOmitsCompactedToolCall(t *testing.T) {
	store := newSessionStatePlanStore()
	if _, err := store.SavePlanSnapshot(context.Background(), "session-restored", planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Survives compaction", Status: planpkg.StatusInProgress}}}); err != nil {
		t.Fatalf("save restored plan: %v", err)
	}
	// Simulate a freshly restored server whose loaded transcript page contains
	// only a compaction tail marker, not the historical update_plan call.
	store.messages["session-restored"] = []session.Message{{
		ID:             99,
		Sequence:       400,
		Role:           "assistant",
		CompactionTail: true,
	}}

	response := getSessionStateResponse(t, store, "session-restored")
	var got currentPlanResponse
	if err := json.Unmarshal(response["current_plan"], &got); err != nil {
		t.Fatalf("decode current_plan: %v", err)
	}
	if got.Version != 1 || len(got.Steps) != 1 || got.Steps[0].Step != "Survives compaction" {
		t.Fatalf("current_plan = %#v, want restored authoritative snapshot", got)
	}
}

func TestHandleSessionStateKeepsPlansIsolatedFromTranscriptAndSessions(t *testing.T) {
	store := newSessionStatePlanStore()
	if _, err := store.SavePlanSnapshot(context.Background(), "session-a", planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Only A", Status: planpkg.StatusCompleted}}}); err != nil {
		t.Fatalf("save session-a plan: %v", err)
	}
	if _, err := store.SavePlanSnapshot(context.Background(), "session-b", planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Only B", Status: planpkg.StatusInProgress}}}); err != nil {
		t.Fatalf("save session-b plan: %v", err)
	}

	for _, tc := range []struct {
		sessionID string
		wantStep  string
	}{
		{sessionID: "session-a", wantStep: "Only A"},
		{sessionID: "session-b", wantStep: "Only B"},
	} {
		t.Run(tc.sessionID, func(t *testing.T) {
			response := getSessionStateResponse(t, store, tc.sessionID)
			var got currentPlanResponse
			if err := json.Unmarshal(response["current_plan"], &got); err != nil {
				t.Fatalf("decode current_plan: %v", err)
			}
			if len(got.Steps) != 1 || got.Steps[0].Step != tc.wantStep {
				t.Fatalf("steps = %#v, want only %q", got.Steps, tc.wantStep)
			}
		})
	}
}

package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestServeUndoRedoFailedTurn(t *testing.T) {
	srv, store, _, _, sessionID := newServeUndoRedoTest(t)
	addServeUndoRedoMessage(t, store, sessionID, core.UserText("first"))
	answer := addServeUndoRedoMessage(t, store, sessionID, core.AssistantText("answer"))
	addServeUndoRedoMessage(t, store, sessionID, core.UserText("restore failed prompt"))
	failure := addServeUndoRedoMessage(t, store, sessionID, llm.RunErrorEventMessage(llm.RunErrorMarker{ResponseID: "resp_failed", ErrorType: "invalid_request_error", Message: "provider rejected request"}))
	state, err := store.TranscriptMutationState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.HeadID != failure.ID {
		t.Fatalf("head = %d, want error row %d", state.HeadID, failure.ID)
	}
	stale := state
	stale.HeadID = failure.ID - 1
	if rr := requestServeUndoRedo(t, srv, sessionID, "undo", stale); rr.Code != http.StatusConflict {
		t.Fatalf("stale head accepted: %d %s", rr.Code, rr.Body.String())
	}
	rr := requestServeUndoRedo(t, srv, sessionID, "undo", state)
	if rr.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Rev      int64  `json:"rev"`
		HeadID   int64  `json:"head_id"`
		UserText string `json:"user_text"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.HeadID != answer.ID || payload.UserText != "restore failed prompt" {
		t.Fatalf("undo payload = %#v", payload)
	}
	rr = requestServeUndoRedo(t, srv, sessionID, "redo", session.TranscriptMutationState{Rev: payload.Rev, HeadID: payload.HeadID})
	if rr.Code != http.StatusOK {
		t.Fatalf("redo: %d %s", rr.Code, rr.Body.String())
	}
	messages, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 || messages[3].ID != failure.ID {
		t.Fatalf("redo failed to restore error row: %#v", messages)
	}
}

package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

func TestNotificationClientHandoffUsesExactDurableCursor(t *testing.T) {
	provider := mockprovider.NewMockProvider("debug").
		AddToolCall("page-call", "webmcp__ping", map[string]any{"message": "hi"}).
		AddTextResponse("page result incorporated")
	s, store, _, origin := newNotificationNativeFixture(t, provider, "window_handoff")
	if w := postNotificationNativePresence(t, s, origin); w.Code != http.StatusOK {
		t.Fatalf("presence=%d %s", w.Code, w.Body.String())
	}
	tools := `"tools":[{"type":"function","name":"webmcp__ping","description":"ping","parameters":{"type":"object"}}]`
	first := notificationNativeRequest(origin, "/v1/responses", `{"input":"ping the page","stream":true,"client_message_id":"handoff-human",`+tools+`}`)
	w := httptest.NewRecorder()
	s.handleResponses(w, first)
	if w.Code != http.StatusOK {
		t.Fatalf("handoff=%d %s", w.Code, w.Body.String())
	}
	run := waitNotificationNativeRun(t, s, w.Header().Get("x-response-id"))
	snapshot := run.snapshot()
	cursor, _ := snapshot["continuation_response_id"].(string)
	messageID, durable := parseDurableResponseMessageID(cursor)
	if !durable || cursor == run.id {
		t.Fatalf("handoff must expose distinct stable run and durable cursor: %+v", snapshot)
	}
	message, err := store.GetMessageByID(context.Background(), messageID)
	if err != nil || message.ResponseID != run.id {
		t.Fatalf("durable cursor lost exact response lineage: %+v %v", message, err)
	}
	terminalAt := strings.LastIndex(w.Body.String(), "event: response.completed\n")
	if terminalAt < 0 || !strings.Contains(w.Body.String()[terminalAt:], `"id":"`+cursor+`"`) || !strings.Contains(w.Body.String()[terminalAt:], `"pending_client_calls"`) {
		t.Fatalf("terminal stream omitted authoritative handoff cursor: %s", w.Body.String())
	}
	rows, err := store.ClaimCompletionNotifications(context.Background(), "intermediate", time.Now().Add(4*time.Second), time.Minute, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("handoff produced premature candidate: %+v %v", rows, err)
	}
	post := func(previous, callID, clientID string) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(`{"stream":true,"previous_response_id":%q,"client_message_id":%q,"input":[{"type":"function_call_output","call_id":%q,"output":"pong"}],%s}`, previous, clientID, callID, tools)
		r := notificationNativeRequest(origin, "/v1/responses", body)
		// Actual tool continuations carry no new human destination. The durable
		// cursor/result set alone must inherit the previously captured owner.
		for _, header := range []string{requestNotificationVersionHeader, requestNotificationWindowHeader, requestNotificationDocumentHeader, requestNotificationSourceHeader, requestPushSubscriptionHeader} {
			r.Header.Del(header)
		}
		result := httptest.NewRecorder()
		s.handleResponses(result, r)
		return result
	}
	// A stable stream ID is not the durable cursor emitted at handoff. Keep the
	// stale-cursor fence rather than weakening it to accommodate a wrong client.
	if rejected := post(run.id, "page-call", "wrong-cursor"); rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), "is stale") {
		t.Fatalf("live ID unexpectedly substituted for durable handoff: %d %s", rejected.Code, rejected.Body.String())
	}
	if rejected := post(cursor, "unrelated-call", "wrong-result"); rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), "do not match the current handoff") {
		t.Fatalf("exact pending result validation weakened: %d %s", rejected.Code, rejected.Body.String())
	}
	if provider.CurrentTurn() != 1 {
		t.Fatal("rejected continuation invoked provider")
	}
	accepted := post(cursor, "page-call", "exact-result")
	if accepted.Code != http.StatusOK {
		t.Fatalf("exact durable continuation=%d %s", accepted.Code, accepted.Body.String())
	}
	final := waitNotificationNativeRun(t, s, accepted.Header().Get("x-response-id"))
	payload := assertNotificationNativeCandidate(t, s, store, origin, final)
	var completion completionPushPayload
	if err := json.Unmarshal(payload, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.ResponseID == run.id || completion.WindowID != origin.WindowID || provider.CurrentTurn() != 2 {
		t.Fatalf("continuation did not finish the captured human turn: %s", payload)
	}
	if rejected := post(cursor, "page-call", "stale-replay"); rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), "is stale") {
		t.Fatalf("old handoff replay admitted after final synthesis: %d %s", rejected.Code, rejected.Body.String())
	}
}

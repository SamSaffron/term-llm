package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
)

// recordingLiveProvider captures the SessionOptions a start produced, so a test
// can assert what the voice model was actually told.
type recordingLiveProvider struct {
	session *stubLiveSession
	options chan live.SessionOptions
}

func (p *recordingLiveProvider) Name() string                { return "stub" }
func (p *recordingLiveProvider) Ready(context.Context) error { return nil }
func (p *recordingLiveProvider) Start(_ context.Context, _ string, opts live.SessionOptions) (live.Session, error) {
	select {
	case p.options <- opts:
	default:
	}
	return p.session, nil
}

type clientDelegationHarness struct {
	srv     *serveServer
	session *stubLiveSession
	options chan live.SessionOptions
}

func newClientDelegationHarness(t *testing.T) *clientDelegationHarness {
	t.Helper()
	session := &stubLiveSession{events: make(chan live.Event, 16), closed: make(chan struct{})}
	harness := &clientDelegationHarness{
		srv: newTestServeServer(), session: session,
		options: make(chan live.SessionOptions, 4),
	}
	harness.srv.cfg.ui = true
	harness.srv.shutdownCh = make(chan struct{})
	harness.srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderChatGPT}}
	harness.srv.liveProviderFactory = func(config.LiveConfig) (live.Provider, error) {
		return &recordingLiveProvider{session: session, options: harness.options}, nil
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		harness.srv.closeLiveSessions(ctx)
	})
	return harness
}

// start posts one start request built from body and returns the decoded response.
func (h *clientDelegationHarness) start(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	if _, ok := body["sdp"]; !ok {
		body["sdp"] = browserOffer
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions", strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.srv.handleLiveSessions(recorder, request)
	var decoded map[string]any
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	}
	return recorder.Code, decoded
}

// startClient starts one client-mode call and returns its live id and record.
func (h *clientDelegationHarness) startClient(t *testing.T, sessionID, hint string) (string, *liveSession) {
	t.Helper()
	body := map[string]any{"session_id": sessionID, "delegation_mode": liveDelegationModeClient}
	if hint != "" {
		body["delegation_context"] = hint
	}
	status, decoded := h.start(t, body)
	if status != http.StatusOK {
		t.Fatalf("start status = %d, body = %v", status, decoded)
	}
	if decoded["delegation_mode"] != liveDelegationModeClient {
		t.Fatalf("echoed delegation_mode = %v", decoded["delegation_mode"])
	}
	liveID, _ := decoded["live_id"].(string)
	record, ok := h.srv.lookupLiveSession(liveID)
	if !ok {
		t.Fatalf("live session %q was not registered", liveID)
	}
	return liveID, record
}

// postResult submits one delegation result through the real route.
func (h *clientDelegationHarness) postResult(t *testing.T, liveID, delegationID, body string) (int, map[string]any) {
	t.Helper()
	return postLiveDelegationResult(t, h.srv, liveID, delegationID, body)
}

func postLiveDelegationResult(t *testing.T, srv *serveServer, liveID, delegationID, body string) (int, map[string]any) {
	t.Helper()
	path := "/v1/live/sessions/" + liveID + "/delegations/" + delegationID + "/result"
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.handleLiveSessionByID(recorder, request)
	var decoded map[string]any
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	}
	return recorder.Code, decoded
}

// requestedDelegations returns every live.delegation_requested published so far,
// in publish order.
func requestedDelegations(record *liveSession) []liveSessionEvent {
	var requested []liveSessionEvent
	for _, event := range liveEventSnapshot(record) {
		if event.Type == liveEventDelegationRequested {
			requested = append(requested, event)
		}
	}
	return requested
}

func waitForRequestedDelegations(t *testing.T, record *liveSession, count int) []liveSessionEvent {
	t.Helper()
	var requested []liveSessionEvent
	waitForLiveCondition(t, fmt.Sprintf("%d live.delegation_requested events", count), func() bool {
		requested = requestedDelegations(record)
		return len(requested) >= count
	})
	return requested
}

func waitForDelegationState(t *testing.T, record *liveSession, delegationID, state string) liveSessionEvent {
	t.Helper()
	var match liveSessionEvent
	waitForLiveCondition(t, "live.delegation "+state, func() bool {
		for _, event := range liveEventSnapshot(record) {
			if event.Type == liveEventDelegation && event.Data["delegation_id"] == delegationID && event.Data["state"] == state {
				match = event
				return true
			}
		}
		return false
	})
	return match
}

func TestLiveStartValidatesDelegationMode(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mode   any
		status int
		want   string
	}{
		{name: "omitted", status: http.StatusOK, want: liveDelegationModeServer},
		{name: "explicit server", mode: liveDelegationModeServer, status: http.StatusOK, want: liveDelegationModeServer},
		{name: "client", mode: liveDelegationModeClient, status: http.StatusOK, want: liveDelegationModeClient},
		{name: "unknown is never defaulted", mode: "device", status: http.StatusBadRequest},
		{name: "empty string is not a mode name", mode: "   ", status: http.StatusOK, want: liveDelegationModeServer},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newClientDelegationHarness(t)
			body := map[string]any{"session_id": "mode-" + strings.ReplaceAll(testCase.name, " ", "-")}
			if testCase.mode != nil {
				body["delegation_mode"] = testCase.mode
			}
			status, decoded := harness.start(t, body)
			if status != testCase.status {
				t.Fatalf("status = %d, want %d (body %v)", status, testCase.status, decoded)
			}
			if testCase.status != http.StatusOK {
				return
			}
			if decoded["delegation_mode"] != testCase.want {
				t.Fatalf("echoed delegation_mode = %v, want %q", decoded["delegation_mode"], testCase.want)
			}
		})
	}
}

func TestLiveCapabilityAdvertisesDelegationModesWithoutBumpingVersion(t *testing.T) {
	harness := newClientDelegationHarness(t)
	capability := harness.srv.liveCapability(context.Background())
	modes, ok := capability["delegation_modes"].([]string)
	if !ok || len(modes) != 2 || modes[0] != liveDelegationModeServer || modes[1] != liveDelegationModeClient {
		t.Fatalf("delegation_modes = %#v", capability["delegation_modes"])
	}
	// The web UI reads the version and gains nothing from client-owned delegation,
	// so this addition is deliberately additive.
	if capability["version"] != 3 {
		t.Fatalf("version = %v, want an unchanged 3", capability["version"])
	}
}

func TestLiveStartValidatesDelegationContext(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mode   string
		hint   string
		status int
	}{
		{name: "client mode accepts a hint", mode: liveDelegationModeClient, hint: "Can control Apple Music.", status: http.StatusOK},
		{name: "server mode refuses a hint", mode: liveDelegationModeServer, hint: "Can control Apple Music.", status: http.StatusBadRequest},
		{name: "default mode refuses a hint", hint: "Can control Apple Music.", status: http.StatusBadRequest},
		{name: "oversized", mode: liveDelegationModeClient, hint: strings.Repeat("a", liveDelegationContextLimitBytes+1), status: http.StatusBadRequest},
		{name: "control characters", mode: liveDelegationModeClient, hint: "Apple Music\x07 control", status: http.StatusBadRequest},
		{name: "newlines are allowed", mode: liveDelegationModeClient, hint: "Apple Music.\nPlaylists.", status: http.StatusOK},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newClientDelegationHarness(t)
			body := map[string]any{"session_id": "ctx-" + strings.ReplaceAll(testCase.name, " ", "-"), "delegation_context": testCase.hint}
			if testCase.mode != "" {
				body["delegation_mode"] = testCase.mode
			}
			if status, decoded := harness.start(t, body); status != testCase.status {
				t.Fatalf("status = %d, want %d (body %v)", status, testCase.status, decoded)
			}
		})
	}
}

func TestLiveClientDelegationContextReachesTheVoiceModel(t *testing.T) {
	harness := newClientDelegationHarness(t)
	harness.startClient(t, "hint-session", "Can control Apple Music: play, pause, skip.")

	var opts live.SessionOptions
	select {
	case opts = <-harness.options:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider never received session options")
	}
	if !strings.Contains(opts.Context, "Can control Apple Music: play, pause, skip.") {
		t.Fatalf("the device hint never reached the voice model: %q", opts.Context)
	}
	if !strings.Contains(opts.Context, "executed by the user's device") {
		t.Fatalf("the hint was not wrapped in the host preamble: %q", opts.Context)
	}
	capabilities := strings.Index(opts.Context, live.CapabilityContext(live.ConfigCapabilities(harness.srv.liveConfig())))
	if capabilities < 0 || capabilities > strings.Index(opts.Context, "executed by the user's device") {
		t.Fatalf("the hint displaced the host capability context: %q", opts.Context)
	}
}

// TestLiveClientDelegationPublishesRequestAndSpeaksTheDeviceAnswer covers the
// whole client-mode round trip through the real controller: the provider's
// delegation event becomes a request event the device can answer, and the
// device's answer is what the voice model hears.
func TestLiveClientDelegationPublishesRequestAndSpeaksTheDeviceAnswer(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, record := harness.startClient(t, "jazz-session", "Can control Apple Music.")

	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_1", Text: "play some jazz"}
	requested := waitForRequestedDelegations(t, record, 1)
	data := requested[0].Data
	if data["delegation_id"] != "item_1" || data["input"] != "play some jazz" {
		t.Fatalf("requested payload = %+v", data)
	}
	if data["session_id"] != "jazz-session" {
		t.Fatalf("requested event lost the binding: %+v", data)
	}
	if data["resend"] != false {
		t.Fatalf("first publication was marked as a resend: %+v", data)
	}
	if !pendingClientDelegation(record, "item_1") {
		t.Fatal("the delegation was published without a pending entry")
	}

	status, body := harness.postResult(t, liveID, "item_1", `{"output":"Playing Kind of Blue.","response_id":"resp_1"}`)
	if status != http.StatusOK || body["ok"] != true || body["duplicate"] != nil {
		t.Fatalf("result status = %d, body = %v", status, body)
	}

	waitForDelegationState(t, record, "item_1", string(live.DelegationDone))
	waitForLiveCondition(t, "the spoken answer", func() bool {
		harness.session.mu.Lock()
		defer harness.session.mu.Unlock()
		for _, chunk := range harness.session.delegated {
			if chunk.Channel == live.ChannelSpeakable && strings.Contains(chunk.Text, "Playing Kind of Blue.") {
				return true
			}
		}
		return false
	})
	waitForLiveCondition(t, "the pending entry to be retired", func() bool {
		return !pendingClientDelegation(record, "item_1")
	})
}

func TestLiveClientDelegationRequestReplaysAfterACursor(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, record := harness.startClient(t, "replay-session", "Can control Apple Music.")
	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_replay", Text: "skip this track"}
	requested := waitForRequestedDelegations(t, record, 1)

	stream := httptest.NewServer(http.HandlerFunc(harness.srv.handleLiveSessionByID))
	defer stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	after := strconv.Itoa(requested[0].Sequence - 1)
	url := stream.URL + "/v1/live/sessions/" + liveID + "/events?after=" + after
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	seen := false
	for !seen {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"delegation_id":"item_replay"`) &&
			strings.Contains(line, `"input":"skip this track"`) {
			seen = true
		}
	}
}

func TestLiveClientDelegationResendsWhilePending(t *testing.T) {
	srv, record := newClientDelegationUnit(t, "resend-session")
	delegator := &serveLiveClientDelegator{
		server: srv, live: record,
		resendInterval: 5 * time.Millisecond, timeout: 5 * time.Second,
	}
	done := runClientDelegation(t, delegator, live.DelegationRequest{ID: "item_resend", Input: "keep asking"})

	requested := waitForRequestedDelegations(t, record, 3)
	if requested[0].Data["resend"] != false {
		t.Fatalf("first publication = %+v", requested[0].Data)
	}
	for _, event := range requested[1:] {
		if event.Data["resend"] != true {
			t.Fatalf("repeat was not marked as a resend: %+v", event.Data)
		}
		if event.Data["delegation_id"] != "item_resend" || event.Data["input"] != "keep asking" {
			t.Fatalf("resend payload drifted: %+v", event.Data)
		}
	}

	if outcome := record.completeClientDelegation("item_resend", "", liveClientDelegationResult{Output: "done"}); outcome != liveDelegationResultAccepted {
		t.Fatalf("result outcome = %v", outcome)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	// A retired delegation stops being republished; otherwise a finished request
	// would keep arriving on the device forever.
	before := len(requestedDelegations(record))
	record.resendClientDelegation("item_resend")
	if after := len(requestedDelegations(record)); after != before {
		t.Fatalf("a retired delegation was republished: %d then %d", before, after)
	}
}

func TestLiveClientDelegationFailurePathsRetireThePendingEntry(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		srv, record := newClientDelegationUnit(t, "timeout-session")
		delegator := &serveLiveClientDelegator{server: srv, live: record, resendInterval: time.Hour, timeout: 20 * time.Millisecond}
		done := runClientDelegation(t, delegator, live.DelegationRequest{ID: "item_timeout", Input: "play jazz"})
		err := <-done
		if err == nil || !strings.Contains(err.Error(), "did not answer") {
			t.Fatalf("Run = %v, want a user-safe timeout", err)
		}
		if pendingClientDelegation(record, "item_timeout") {
			t.Fatal("the pending entry survived a timeout")
		}
		// A device answering after the host gave up must not be absorbed silently.
		if outcome := record.completeClientDelegation("item_timeout", "", liveClientDelegationResult{Output: "late"}); outcome != liveDelegationResultConflict {
			t.Fatalf("late result outcome = %v, want a conflict", outcome)
		}
	})

	t.Run("controller cancellation", func(t *testing.T) {
		srv, record := newClientDelegationUnit(t, "cancel-session")
		delegator := &serveLiveClientDelegator{server: srv, live: record, resendInterval: time.Hour, timeout: time.Hour}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- delegator.Run(ctx, live.DelegationRequest{ID: "item_cancel", Input: "play jazz"}, func(live.DelegationChunk) {})
		}()
		waitForRequestedDelegations(t, record, 1)
		cancel()
		if err := <-done; err == nil {
			t.Fatal("Run returned nil after cancellation")
		}
		if pendingClientDelegation(record, "item_cancel") {
			t.Fatal("the pending entry survived cancellation")
		}
	})

	t.Run("call ended", func(t *testing.T) {
		srv, record := newClientDelegationUnit(t, "ended-session")
		delegator := &serveLiveClientDelegator{server: srv, live: record, resendInterval: time.Hour, timeout: time.Hour}
		done := runClientDelegation(t, delegator, live.DelegationRequest{ID: "item_ended", Input: "play jazz"})
		waitForRequestedDelegations(t, record, 1)
		record.closeSubscribers()
		err := <-done
		if err == nil || !strings.Contains(err.Error(), "ended") {
			t.Fatalf("Run = %v, want the call-ended reason", err)
		}
		if pendingClientDelegation(record, "item_ended") {
			t.Fatal("the pending entry survived shutdown")
		}
		if outcome := record.completeClientDelegation("item_ended", "", liveClientDelegationResult{Output: "late"}); outcome != liveDelegationResultConflict {
			t.Fatalf("post-shutdown result outcome = %v, want a conflict", outcome)
		}
	})
}

func TestLiveClientDelegationStaysBoundToTheSessionItStartedIn(t *testing.T) {
	srv, record := newClientDelegationUnit(t, "switch-source")
	delegator := &serveLiveClientDelegator{server: srv, live: record, resendInterval: time.Hour, timeout: 5 * time.Second}
	done := runClientDelegation(t, delegator, live.DelegationRequest{ID: "item_bound", Input: "play jazz"})
	requested := waitForRequestedDelegations(t, record, 1)
	if requested[0].Data["session_id"] != "switch-source" {
		t.Fatalf("requested session = %v", requested[0].Data["session_id"])
	}

	if _, err := srv.rebindLiveSession(record, liveSwitchTarget{SessionID: "switch-target", Number: 7, Title: "Target"}); err != nil {
		t.Fatal(err)
	}
	// The in-flight request keeps the session it was published with, so a client
	// that answers it with that id is accepted rather than refused as inconsistent.
	if outcome := record.completeClientDelegation("item_bound", "switch-source", liveClientDelegationResult{Output: "ok"}); outcome != liveDelegationResultAccepted {
		t.Fatalf("in-flight result outcome = %v", outcome)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}

	next := runClientDelegation(t, delegator, live.DelegationRequest{ID: "item_after", Input: "play blues"})
	requested = waitForRequestedDelegations(t, record, 2)
	if got := requested[1].Data["session_id"]; got != "switch-target" {
		t.Fatalf("the next request used session %v, want the new binding", got)
	}
	if outcome := record.completeClientDelegation("item_after", "", liveClientDelegationResult{Output: "ok"}); outcome != liveDelegationResultAccepted {
		t.Fatalf("second result outcome = %v", outcome)
	}
	if err := <-next; err != nil {
		t.Fatalf("second Run = %v", err)
	}
}

func TestLiveDelegationResultIsIdempotentAndFirstWriterWins(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, record := harness.startClient(t, "idempotent-session", "Can control Apple Music.")
	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_once", Text: "play jazz"}
	waitForRequestedDelegations(t, record, 1)

	accepted := `{"output":"  Playing jazz.  ","response_id":"resp_a"}`
	if status, body := harness.postResult(t, liveID, "item_once", accepted); status != http.StatusOK || body["ok"] != true {
		t.Fatalf("first result status = %d, body = %v", status, body)
	}
	// Trimming is part of the accepted value, so a client retry that differs only
	// in whitespace is the same answer, not a contradiction.
	// response_id is correlation metadata, so a retry reporting a different one is
	// still the same answer.
	status, body := harness.postResult(t, liveID, "item_once", `{"output":"Playing jazz.","response_id":"resp_b"}`)
	if status != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("identical replay status = %d, body = %v", status, body)
	}
	if status, body := harness.postResult(t, liveID, "item_once", `{"output":"Playing something else."}`); status != http.StatusConflict {
		t.Fatalf("differing replay status = %d, body = %v", status, body)
	}
	if status, _ := harness.postResult(t, liveID, "item_missing", `{"output":"ok"}`); status != http.StatusNotFound {
		t.Fatalf("unknown delegation status = %d, want 404", status)
	}
	if status, _ := postLiveDelegationResult(t, harness.srv, "live_missing", "item_once", `{"output":"ok"}`); status != http.StatusNotFound {
		t.Fatalf("unknown live id status = %d, want 404", status)
	}
	if status, _ := harness.postResult(t, liveID, "item_once", `{"session_id":"other","output":"Playing jazz."}`); status != http.StatusConflict {
		t.Fatalf("mismatched session_id status = %d, want 409", status)
	}
}

func TestLiveDelegationResultValidatesItsBody(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, record := harness.startClient(t, "body-session", "Can control Apple Music.")
	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_body", Text: "play jazz"}
	waitForRequestedDelegations(t, record, 1)

	for _, testCase := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "neither", body: `{}`, status: http.StatusBadRequest},
		{name: "both", body: `{"output":"ok","error":"nope"}`, status: http.StatusBadRequest},
		{name: "blank output", body: `{"output":"   "}`, status: http.StatusBadRequest},
		{name: "blank error", body: `{"error":"  "}`, status: http.StatusBadRequest},
		{name: "invalid json", body: `{`, status: http.StatusBadRequest},
		{name: "oversize", body: `{"output":"` + strings.Repeat("a", liveDelegationResultLimitBytes+1) + `"}`, status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if status, body := harness.postResult(t, liveID, "item_body", testCase.body); status != testCase.status {
				t.Fatalf("status = %d, want %d (body %v)", status, testCase.status, body)
			}
		})
	}
	if !pendingClientDelegation(record, "item_body") {
		t.Fatal("a rejected body consumed the pending delegation")
	}
}

func TestLiveDelegationResultRejectsNonPostAndUnknownSubpaths(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, _ := harness.startClient(t, "method-session", "")

	request := httptest.NewRequest(http.MethodGet, "/v1/live/sessions/"+liveID+"/delegations/item_1/result", nil)
	recorder := httptest.NewRecorder()
	harness.srv.handleLiveSessionByID(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", recorder.Code)
	}

	for _, path := range []string{"/delegations/item_1", "/delegations/", "/delegations/item_1/results"} {
		request = httptest.NewRequest(http.MethodPost, "/v1/live/sessions/"+liveID+path, strings.NewReader(`{"output":"ok"}`))
		recorder = httptest.NewRecorder()
		harness.srv.handleLiveSessionByID(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, recorder.Code)
		}
	}
}

// TestLiveServerModeNeverPublishesADelegationRequest guards the default: a call
// that did not ask for client mode must keep running delegations as chat turns,
// with nothing published for a device to pick up.
func TestLiveServerModeNeverPublishesADelegationRequest(t *testing.T) {
	harness := newClientDelegationHarness(t)
	status, decoded := harness.start(t, map[string]any{"session_id": "server-mode-session"})
	if status != http.StatusOK {
		t.Fatalf("start status = %d, body = %v", status, decoded)
	}
	liveID, _ := decoded["live_id"].(string)
	record, ok := harness.srv.lookupLiveSession(liveID)
	if !ok {
		t.Fatal("live session was not registered")
	}
	select {
	case opts := <-harness.options:
		if strings.Contains(opts.Context, "executed by the user's device") {
			t.Fatalf("server mode told the voice model about device tools: %q", opts.Context)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the provider never received session options")
	}
	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_server", Text: "list the go files"}
	waitForDelegationState(t, record, "item_server", string(live.DelegationRunning))
	if requested := requestedDelegations(record); len(requested) != 0 {
		t.Fatalf("server mode published %d delegation requests", len(requested))
	}
	if status, _ := harness.postResult(t, liveID, "item_server", `{"output":"three files"}`); status != http.StatusNotFound {
		t.Fatalf("server-mode result status = %d, want 404", status)
	}
}

// pendingClientDelegation reports whether delegationID still has a waiter. This
// is internal bookkeeping, asserted only to prove every exit path retires it.
func pendingClientDelegation(record *liveSession, delegationID string) bool {
	record.mu.Lock()
	defer record.mu.Unlock()
	entry := record.clientDelegationLocked(delegationID)
	return entry != nil && entry.done != nil
}

// newClientDelegationUnit builds a registered call for tests that drive the
// delegator directly, so resend, timeout, and cancellation do not have to wait
// out the production intervals.
func newClientDelegationUnit(t *testing.T, sessionID string) (*serveServer, *liveSession) {
	t.Helper()
	srv := newTestServeServer()
	record := newLiveSession("live_"+sessionID, sessionID)
	record.delegationMode = liveDelegationModeClient
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	return srv, record
}

// runClientDelegation starts one Run and returns its error channel.
func runClientDelegation(t *testing.T, delegator *serveLiveClientDelegator, request live.DelegationRequest) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- delegator.Run(context.Background(), request, func(live.DelegationChunk) {})
	}()
	return done
}

// TestLiveClientDelegationIsAbandonedWhenTheProviderEndsTheCall covers the end a
// host teardown never runs for: the provider stream finishes on its own. The
// waiter must be released then rather than held to the delegation timeout, and a
// result posted afterwards must be refused with a conflict rather than accepted
// for a call that can no longer speak it.
func TestLiveClientDelegationIsAbandonedWhenTheProviderEndsTheCall(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, record := harness.startClient(t, "provider-ended-session", "Can control Apple Music.")
	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_hangup", Text: "play jazz"}
	waitForRequestedDelegations(t, record, 1)

	harness.session.events <- live.Event{Kind: live.EventEnded}
	waitForLiveCondition(t, "the waiter to be released", func() bool {
		return !pendingClientDelegation(record, "item_hangup")
	})
	// The call is ended but still registered, so this is the ended-call conflict
	// rather than an unknown id.
	if status, body := harness.postResult(t, liveID, "item_hangup", `{"output":"Playing jazz."}`); status != http.StatusConflict {
		t.Fatalf("post-hangup result status = %d, body = %v", status, body)
	}
}

// TestLiveClientDelegationFailureIsSpokenAndPublished checks the controller still
// owns the lifecycle in client mode: a device failure becomes the failed state,
// carrying the device's own user-safe text.
func TestLiveClientDelegationFailureIsSpokenAndPublished(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, record := harness.startClient(t, "device-failure-session", "Can control Apple Music.")
	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_fail", Text: "play jazz"}
	waitForRequestedDelegations(t, record, 1)

	if status, _ := harness.postResult(t, liveID, "item_fail", `{"error":"Apple Music is not authorized."}`); status != http.StatusOK {
		t.Fatalf("failure result status = %d", status)
	}
	failed := waitForDelegationState(t, record, "item_fail", string(live.DelegationFailed))
	if text, _ := failed.Data["text"].(string); !strings.Contains(text, "Apple Music is not authorized.") {
		t.Fatalf("failed event text = %q", text)
	}
	waitForLiveCondition(t, "the spoken failure", func() bool {
		harness.session.mu.Lock()
		defer harness.session.mu.Unlock()
		for _, chunk := range harness.session.delegated {
			if strings.Contains(chunk.Text, "Apple Music is not authorized.") {
				return true
			}
		}
		return false
	})
	if pendingClientDelegation(record, "item_fail") {
		t.Fatal("the pending entry survived a device failure")
	}
}

// TestLiveClientDelegationWaitsForABusySessionThroughTheController asserts the
// whole point of keeping the busy check on the host: the voice model hears the
// same busy notice as in server mode and nothing is published to the device until
// the session is free, rather than the device discovering a 409 for itself.
func TestLiveClientDelegationWaitsForABusySessionThroughTheController(t *testing.T) {
	harness := newClientDelegationHarness(t)
	liveID, record := harness.startClient(t, "busy-controller-session", "Can control Apple Music.")
	runs := harness.srv.ensureResponseRuns()
	runs.setActiveRun("busy-controller-session", "run_busy")

	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_wait", Text: "play jazz"}
	waitForLiveCondition(t, "the busy notice", func() bool {
		harness.session.mu.Lock()
		defer harness.session.mu.Unlock()
		for _, chunk := range harness.session.delegated {
			if chunk.Channel == live.ChannelQuiet && strings.Contains(chunk.Text, "Still finishing") {
				return true
			}
		}
		return false
	})
	if requested := requestedDelegations(record); len(requested) != 0 {
		t.Fatalf("a busy session published %d requests to the device", len(requested))
	}

	runs.clearActiveRun("busy-controller-session", "run_busy")
	waitForRequestedDelegations(t, record, 1)
	if status, _ := harness.postResult(t, liveID, "item_wait", `{"output":"Playing jazz."}`); status != http.StatusOK {
		t.Fatalf("result after the retry status = %d", status)
	}
	waitForDelegationState(t, record, "item_wait", string(live.DelegationDone))
}

// TestLiveClientDelegationRefusesARebindBetweenTheBusyCheckAndThePublish covers
// the one-snapshot rule: the session checked for a running turn, the session
// published to the device, and the session a client's consistency check compares
// against must all be the same value.
func TestLiveClientDelegationRefusesARebindBetweenTheBusyCheckAndThePublish(t *testing.T) {
	srv, record := newClientDelegationUnit(t, "snapshot-source")
	if _, err := srv.rebindLiveSession(record, liveSwitchTarget{SessionID: "snapshot-target", Number: 3, Title: "Target"}); err != nil {
		t.Fatal(err)
	}
	// "snapshot-source" is the stale snapshot a Run would be holding.
	if _, err := record.beginClientDelegation("item_stale", "snapshot-source", "play jazz"); err == nil {
		t.Fatal("a stale binding snapshot was published to the device")
	}
	if requested := requestedDelegations(record); len(requested) != 0 {
		t.Fatalf("the refused delegation published %d requests", len(requested))
	}
	if pendingClientDelegation(record, "item_stale") {
		t.Fatal("the refused delegation left a pending entry")
	}
}

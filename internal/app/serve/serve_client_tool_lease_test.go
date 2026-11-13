package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	codemodetool "github.com/samsaffron/term-llm/internal/codemode/tool"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
)

func typedLeaseFixture(t *testing.T, ctx context.Context) (context.Context, *serveServer, *responseRun, *typedClientToolRunner, func()) {
	t.Helper()
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	t.Cleanup(srv.responseRuns.Close)
	run := newResponseRun("resp_foreground", "session_foreground", "", "mock", time.Now().Unix(), nil)
	if err := srv.responseRuns.create(run); err != nil {
		t.Fatal(err)
	}
	specs := liveClientToolSpecs([]json.RawMessage{pageToolDefinition("webmcp__ping")})
	scopedCtx, factory, closeForeground := srv.typedForegroundClientToolRunner(ctx, nil, specs, true)
	runner := factory(run.id).(*typedClientToolRunner)
	run.clientToolLease = runner.ClientToolLease()
	if run.clientToolLease == nil {
		t.Fatal("typed original foreground has no lease")
	}
	return scopedCtx, srv, run, runner, closeForeground
}

func TestTypedClientForegroundLeaseOriginalStreamOnly(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream_return", true: "disconnect"}[disconnect], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, _, run, runner, closeForeground := typedLeaseFixture(t, ctx)
			defer closeForeground()
			lease := runner.ClientToolLease()
			if disconnect {
				cancel()
			} else {
				closeForeground()
			}
			select {
			case <-lease.Done():
			case <-time.After(time.Second):
				t.Fatal("original foreground did not revoke")
			}
			// A recovery subscriber does not run the original factory or rebind.
			subscriber := run.subscribe(0)
			if subscriber.ch != nil {
				defer run.unsubscribe(subscriber.ch)
			}
			if runner.ClientToolLease() != lease || lease.Check() == nil {
				t.Fatal("recovery renewed original foreground capability")
			}
		})
	}
}

func TestTypedClientForegroundResponseTerminalAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "cancel"}[cancelled], func(t *testing.T) {
			_, _, run, runner, closeForeground := typedLeaseFixture(t, context.Background())
			defer closeForeground()
			if cancelled {
				run.mu.Lock()
				run.cancel = func() {}
				run.mu.Unlock()
				run.requestCancel()
			} else {
				run.mu.Lock()
				err := run.appendEventLocked("response.completed", nil, true)
				run.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			if runner.ClientToolLease().Check() == nil {
				t.Fatal("response terminal/cancel did not revoke synchronously")
			}
		})
	}
}

// Match production: a delegated foreground factory is created after the
// originating live event connection is attached, not before it subscribes.
func pinLiveForegroundFactory(h *inlineRunnerHarness) {
	old := h.factory("unadmitted").(inlinePageToolRun)
	h.factory = old.d.inlinePageToolRunner(old.runtime, old.sessionID, h.record.declaredClientTools())
}

func TestLiveClientForegroundLeaseLifecycle(t *testing.T) {
	for _, end := range []string{"disconnect", "overflow", "generation", "end", "response"} {
		t.Run(end, func(t *testing.T) {
			h := newInlineRunnerHarness(t, "lease-"+end)
			_, owner, _, terminal := h.record.subscribe(0)
			if terminal {
				t.Fatal("unexpected terminal live session")
			}
			defer h.record.unsubscribe(owner)
			pinLiveForegroundFactory(h)
			runner := h.factory("resp_original").(inlinePageToolRun)
			lease := runner.ClientToolLease()
			if lease == nil {
				t.Fatal("unique foreground subscriber was not admitted")
			}
			switch end {
			case "disconnect":
				h.record.unsubscribe(owner)
			case "overflow":
				h.record.mu.Lock()
				for range liveSubscriberBuffer + 1 {
					h.record.appendEventLocked("test.fill", nil)
				}
				h.record.mu.Unlock()
			case "generation":
				if err := h.record.setClientTools(h.record.declaredClientTools()); err != nil {
					t.Fatal(err)
				}
			case "end":
				h.record.closeSubscribers()
			case "response":
				runner.ResponseClientToolsDone()
			}
			if lease.Check() == nil {
				t.Fatal("lifecycle hook did not revoke synchronously")
			}
			// Reconnecting and resuming must not renew the captured runner.
			_, next, _, _ := h.record.subscribe(0)
			defer h.record.unsubscribe(next)
			if runner.ClientToolLease() != lease || lease.Check() == nil {
				t.Fatal("reconnect rebound old client capability")
			}
		})
	}
}

func TestLiveClientForegroundRequiresUniqueCurrentOwner(t *testing.T) {
	h := newInlineRunnerHarness(t, "lease-unsupported")
	if h.factory("resp_no_stream").(inlinePageToolRun).ClientToolLease() != nil {
		t.Fatal("no foreground stream still admitted client lease")
	}
	_, first, _, _ := h.record.subscribe(0)
	defer h.record.unsubscribe(first)
	_, second, _, _ := h.record.subscribe(0)
	defer h.record.unsubscribe(second)
	if h.factory("resp_ambiguous").(inlinePageToolRun).ClientToolLease() != nil {
		t.Fatal("ambiguous foreground owner was admitted")
	}
}

func TestLiveClientForegroundResultAndExpiryWhileQueued(t *testing.T) {
	h := newInlineRunnerHarness(t, "lease-live-result")
	_, owner, _, _ := h.record.subscribe(0)
	defer h.record.unsubscribe(owner)
	pinLiveForegroundFactory(h)
	runner := h.factory("resp_original").(inlinePageToolRun)
	lease := runner.ClientToolLease()
	done := make(chan inlineRunnerResult, 1)
	go func() {
		out, err := lease.Dispatch(context.Background(), runner, pageToolCall("foreground"))
		done <- inlineRunnerResult{out, err}
	}()
	request, _ := requestedCalls(t, waitForRequestedToolCalls(t, h.record, 1)[0])
	if status, _ := postLiveToolResult(t, h.srv, h.record.id, request, `{"outputs":[{"call_id":"foreground","output":"pong from original client"}]}`); status != http.StatusOK {
		t.Fatalf("result status=%d", status)
	}
	if result := waitInlineRunner(t, done); result.err != nil || result.output.Content != "pong from original client" {
		t.Fatalf("result=%+v", result)
	}
	// Hold the device gate. Expiry must reject without publishing another round.
	runner.turn <- struct{}{}
	go func() {
		out, err := lease.Dispatch(context.Background(), runner, pageToolCall("queued"))
		done <- inlineRunnerResult{out, err}
	}()
	h.record.unsubscribe(owner)
	result := waitInlineRunner(t, done)
	var failure *core.ClientToolError
	if !errors.As(result.err, &failure) || failure.Code != "client_binding_expired" || failure.Outcome != "not_dispatched" {
		t.Fatalf("queued expiry=%v", result.err)
	}
	<-runner.turn
	if len(requestedToolCalls(h.record)) != 1 {
		t.Fatal("queued expired call reached device")
	}
}

// This provider asks the ordinary engine lane to run a script, not a raw client
// bridge shortcut. The script's client result must return through typed HTTP.
type foregroundScriptProvider struct {
	result chan core.ToolExecutionResponse
}

func (p *foregroundScriptProvider) Name() string       { return "foreground-script" }
func (p *foregroundScriptProvider) Credential() string { return "test" }
func (p *foregroundScriptProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, InlineToolLoop: true}
}
func (p *foregroundScriptProvider) Stream(ctx context.Context, _ core.Request) (core.Stream, error) {
	stream := &pendingTestStream{events: make(chan core.Event)}
	go func() {
		defer close(stream.events)
		call := core.ToolCall{ID: "script_outer", Name: "code_mode", Arguments: json.RawMessage(`{"code":"text((await tools.webmcp__ping({message:'hello'})).text)"}`)}
		answer := make(chan core.ToolExecutionResponse, 1)
		select {
		case stream.events <- core.Event{Type: core.EventToolCall, Tool: &call, ToolResponse: answer}:
		case <-ctx.Done():
			return
		}
		select {
		case result := <-answer:
			p.result <- result
		case <-ctx.Done():
			return
		}
		select {
		case stream.events <- core.Event{Type: core.EventDone}:
		case <-ctx.Done():
		}
	}()
	return stream, nil
}

func TestTypedClientForegroundScriptHTTPRoundTrip(t *testing.T) {
	provider := &foregroundScriptProvider{result: make(chan core.ToolExecutionResponse, 1)}
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
		e := engine.NewEngine(provider, nil)
		e.RegisterTool(codemodetool.New())
		rt := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: e, defaultModel: "mock-model", maxTurns: 4}
		rt.Touch()
		return rt, nil
	})
	defer srv.sessionMgr.Close()
	defer srv.responseRuns.Close()
	ts := newServeHTTPTestServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"compose my phone","client_message_id":"typed-script-1","stream":true,"tools":[{"type":"function","name":"webmcp__ping","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	req.Header.Set("session_id", "typed-script")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("response=%d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	for {
		name, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("no nested client request")
		}
		if name != "response.client_tool.requested" {
			continue
		}
		var event struct {
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		post, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses/"+resp.Header.Get("x-response-id")+"/client_tool_calls/"+event.CallID+"/result", strings.NewReader(`{"output":"phone foreground result"}`))
		if err != nil {
			t.Fatal(err)
		}
		post.Header.Set("Content-Type", "application/json")
		reply, err := ts.Client().Do(post)
		if err != nil {
			t.Fatal(err)
		}
		reply.Body.Close()
		if reply.StatusCode != http.StatusOK {
			t.Fatalf("post=%d", reply.StatusCode)
		}
		select {
		case result := <-provider.result:
			if result.Err != nil || !strings.Contains(result.Result.Content, "phone foreground result") {
				t.Fatalf("script result=%+v", result)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return
	}
}

func TestLiveClientForegroundPinnedOwnerGenerationAndSession(t *testing.T) {
	h := newInlineRunnerHarness(t, "lease-pinned")
	_, owner, _, _ := h.record.subscribe(0)
	defer h.record.unsubscribe(owner)
	pinLiveForegroundFactory(h)
	original := h.factory("resp_original").(inlinePageToolRun).ClientToolLease()
	if original == nil {
		t.Fatal("no original lease")
	}
	// A second observer makes delivery ambiguous and ends existing capability.
	_, second, _, _ := h.record.subscribe(0)
	if original.Check() == nil {
		t.Fatal("new observer did not revoke original owner")
	}
	h.record.unsubscribe(second)
	// The same schemas still constitute a new host generation.
	if err := h.record.setClientTools(h.record.declaredClientTools()); err != nil {
		t.Fatal(err)
	}
	if h.factory("resp_old_factory").(inlinePageToolRun).ClientToolLease() != nil {
		t.Fatal("old factory rebound after same-schema generation change")
	}
	pinLiveForegroundFactory(h)
	current := h.factory("resp_current").(inlinePageToolRun).ClientToolLease()
	if current == nil || current.Binding().Generation == original.Binding().Generation {
		t.Fatal("new offered generation was not pinned")
	}
	if _, err := h.srv.rebindLiveSession(h.record, liveSwitchTarget{SessionID: "other-session"}); err != nil {
		t.Fatal(err)
	}
	if current.Check() == nil {
		t.Fatal("actual live session switch did not revoke")
	}
}

func TestTypedClientForegroundUnsupportedOffersStayDirectOnly(t *testing.T) {
	srv := &serveServer{}
	for _, test := range []struct {
		enabled bool
		name    string
	}{{false, "webmcp__ping"}, {true, "generic_passthrough"}} {
		_, factory, closeForeground := srv.typedForegroundClientToolRunner(context.Background(), nil, []core.ToolSpec{{Name: test.name, Schema: map[string]any{"type": "object"}}}, test.enabled)
		defer closeForeground()
		if factory != nil {
			t.Fatalf("unsupported foreground offer %q admitted", test.name)
		}
	}
}

func TestTypedClientForegroundReplayFencePreservesDirectReplay(t *testing.T) {
	ctx, _, run, runner, closeForeground := typedLeaseFixture(t, context.Background())
	defer closeForeground()
	marked := responseRunEvent{Event: "response.client_tool.requested", Data: json.RawMessage(`{"foreground_only":true,"call_id":"original"}`)}
	var before strings.Builder
	if err := run.writeForegroundSafeEvent(ctx, &before, marked); err != nil || before.Len() == 0 {
		t.Fatalf("active foreground frame lost: %v", err)
	}
	runner.ClientToolLease().Revoke("original stream disconnected before frame delivery")
	var recovered strings.Builder
	if err := run.writeForegroundSafeEvent(context.Background(), &recovered, marked); err != nil || recovered.Len() != 0 {
		t.Fatalf("recovery replayed expired foreground frame: %q %v", recovered.String(), err)
	}
	direct := responseRunEvent{Event: "response.client_tool.requested", Data: json.RawMessage(`{"call_id":"direct"}`)}
	if err := run.writeForegroundSafeEvent(context.Background(), &recovered, direct); err != nil || recovered.Len() == 0 {
		t.Fatalf("direct inline recovery changed: %v", err)
	}
}

func TestLiveClientForegroundReplayFence(t *testing.T) {
	h := newInlineRunnerHarness(t, "lease-replay")
	_, owner, _, _ := h.record.subscribe(0)
	defer h.record.unsubscribe(owner)
	pinLiveForegroundFactory(h)
	runner := h.factory("resp_original").(inlinePageToolRun)
	done := make(chan inlineRunnerResult, 1)
	go func() {
		out, err := runner.ClientToolLease().Dispatch(context.Background(), runner, pageToolCall("replay"))
		done <- inlineRunnerResult{out, err}
	}()
	event := waitForRequestedToolCalls(t, h.record, 1)[0]
	if event.Data["foreground_only"] != true || !h.record.clientToolEventDeliverable(event) {
		t.Fatal("active request not marked/deliverable")
	}
	h.record.unsubscribe(owner)
	if h.record.clientToolEventDeliverable(event) {
		t.Fatal("expired foreground event could replay after reconnect")
	}
	waitInlineRunner(t, done)
}

func TestTypedClientPublicationFailureNotDispatched(t *testing.T) {
	_, _, run, runner, closeForeground := typedLeaseFixture(t, context.Background())
	defer closeForeground()
	run.mu.Lock()
	run.status = "completed"
	run.mu.Unlock()
	_, err := runner.ClientToolLease().Dispatch(context.Background(), runner, pageToolCall("unpublished"))
	var failure *core.ClientToolError
	if !errors.As(err, &failure) || failure.Outcome != "not_dispatched" {
		t.Fatalf("publish failure = %v", err)
	}
	if len(run.events) != 0 || len(runner.pendingSnapshot()) != 0 {
		t.Fatal("failed publication was advertised")
	}
}

func TestTypedClientExpiredAnswerHTTPConflict(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending_unwinding", true: "duplicate_receipt"}[accepted], func(t *testing.T) {
			_, srv, run, runner, closeForeground := typedLeaseFixture(t, context.Background())
			defer closeForeground()
			published, release := make(chan struct{}), make(chan struct{})
			runner.mailbox = core.NewClientToolMailbox(runner.names, 2, time.Hour, func(ctx context.Context, _ core.ToolCall) error {
				if err := core.BeginClientToolDispatch(ctx); err != nil {
					return err
				}
				close(published)
				if !accepted {
					<-release
				}
				return nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := runner.ClientToolLease().Dispatch(context.Background(), runner, pageToolCall("original"))
				done <- err
			}()
			<-published
			post := func() int {
				req := httptest.NewRequest(http.MethodPost, "/result", strings.NewReader(`{"output":"result"}`))
				req.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				srv.handleTypedClientToolResult(rr, req, run, "original")
				return rr.Code
			}
			if accepted {
				if code := post(); code != http.StatusOK {
					t.Fatalf("active answer=%d", code)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			runner.ClientToolLease().Revoke("original foreground ended")
			if code := post(); code != http.StatusConflict {
				t.Fatalf("expired answer acknowledged: %d", code)
			}
			if !accepted {
				close(release)
				<-done
			}
		})
	}
}

func TestLiveClientExpiredAnswersAndResend(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending_unwinding", true: "duplicate_receipt"}[accepted], func(t *testing.T) {
			h := newInlineRunnerHarness(t, "review-expired")
			_, owner, _, _ := h.record.subscribe(0)
			defer h.record.unsubscribe(owner)
			pinLiveForegroundFactory(h)
			lease := h.factory("resp_original").(inlinePageToolRun).ClientToolLease()
			if _, err := h.record.beginClientToolRequest("review_request", h.record.sessionID, "resp_original", []liveClientToolCall{{CallID: "original", Name: "webmcp__ping"}}); err != nil {
				t.Fatal(err)
			}
			h.record.mu.Lock()
			h.record.clientToolRequestLocked("review_request").lease = lease
			h.record.mu.Unlock()
			result := liveClientToolResult{Outputs: []string{"result"}}
			if accepted && h.record.completeClientToolRequest("review_request", result) != liveDelegationResultAccepted {
				t.Fatal("active answer rejected")
			}
			lease.Revoke("response teardown")
			before := len(requestedToolCalls(h.record))
			h.record.resendClientToolRequest("review_request")
			if len(requestedToolCalls(h.record)) != before {
				t.Fatal("expired request republished during unwind")
			}
			if got := h.record.completeClientToolRequest("review_request", result); got != liveDelegationResultConflict {
				t.Fatalf("expired result acknowledged: %v", got)
			}
		})
	}
}

func TestInlinePageToolRunNilTeardown(t *testing.T) {
	(inlinePageToolRun{}).ResponseClientToolsDone()
	(inlinePageToolRun{inlinePageTools: &inlinePageTools{d: &serveLiveDelegator{}}}).ResponseClientToolsDone()
}

func TestTypedClientForegroundOnlyOriginalHTTPConnection(t *testing.T) {
	provider := &foregroundScriptProvider{result: make(chan core.ToolExecutionResponse, 1)}
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
		e := engine.NewEngine(provider, nil)
		e.RegisterTool(codemodetool.New())
		rt := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: e, defaultModel: "mock-model", maxTurns: 4}
		rt.Touch()
		return rt, nil
	})
	defer srv.sessionMgr.Close()
	defer srv.responseRuns.Close()
	ts := newServeHTTPTestServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"phone","client_message_id":"two-clients","stream":true,"tools":[{"type":"function","name":"webmcp__ping","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	req.Header.Set("session_id", "two-clients")
	original, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Body.Close()
	if original.StatusCode != http.StatusOK {
		t.Fatalf("original status=%d", original.StatusCode)
	}
	scanner := bufio.NewScanner(original.Body)
	for {
		name, _, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("original stream did not receive foreground request")
		}
		if name == "response.client_tool.requested" {
			break
		}
	}
	id := original.Header.Get("x-response-id")
	run, ok := srv.responseRuns.get(id)
	if !ok {
		t.Fatal("missing response")
	}
	run.mu.Lock()
	lease := run.clientToolLease
	run.mu.Unlock()
	if lease == nil || lease.Check() != nil {
		t.Fatal("original foreground is not active")
	}
	assertRecovery := func(barrier string) {
		t.Helper()
		if err := run.appendEvent(barrier, nil); err != nil {
			t.Fatal(err)
		}
		get, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/responses/"+id+"/events?after=0", nil)
		if err != nil {
			t.Fatal(err)
		}
		other, err := ts.Client().Do(get)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Body.Close()
		if other.StatusCode != http.StatusOK {
			t.Fatalf("resume status=%d", other.StatusCode)
		}
		scan := bufio.NewScanner(other.Body)
		for {
			name, _, ok := readSSEEvent(t, scan)
			if !ok {
				t.Fatal("resume ended before replay barrier")
			}
			if name == "response.client_tool.requested" {
				t.Fatal("another HTTP client received originating foreground request")
			}
			if name == barrier {
				break
			}
		}
		get, err = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/responses/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := ts.Client().Do(get)
		if err != nil {
			t.Fatal(err)
		}
		defer snapshot.Body.Close()
		var body struct {
			Pending []json.RawMessage `json:"pending_inline_client_tools"`
		}
		if err := json.NewDecoder(snapshot.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Pending) != 0 {
			t.Fatal("generic snapshot exposed foreground dispatch")
		}
	}
	assertRecovery("test.active_replay_barrier")
	if lease.Check() != nil {
		t.Fatal("observer request unexpectedly revoked original owner")
	}
	original.Body.Close()
	select {
	case <-lease.Done():
	case <-ctx.Done():
		t.Fatal("original disconnect did not revoke")
	}
	assertRecovery("test.expired_replay_barrier")
}

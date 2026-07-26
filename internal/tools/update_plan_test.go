package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
)

type testPlanStore struct {
	snapshots map[string]planpkg.Snapshot
	versions  map[string]int64
	saveErr   error
	loads     int
	saves     int
	deletes   int
}

type blockingPlanStore struct {
	blocked chan struct{}
	release chan struct{}
}

func (s *blockingPlanStore) LoadPlanSnapshot(_ context.Context, sessionID string) (planpkg.Snapshot, int64, error) {
	if sessionID == "blocked" {
		close(s.blocked)
		<-s.release
	}
	return planpkg.Snapshot{}, 0, nil
}

func (*blockingPlanStore) SavePlanSnapshot(context.Context, string, planpkg.Snapshot) (int64, error) {
	return 0, nil
}

func (*blockingPlanStore) DeletePlanSnapshot(context.Context, string) error { return nil }

func newTestPlanStore() *testPlanStore {
	return &testPlanStore{snapshots: make(map[string]planpkg.Snapshot), versions: make(map[string]int64)}
}

func (s *testPlanStore) LoadPlanSnapshot(_ context.Context, sessionID string) (planpkg.Snapshot, int64, error) {
	s.loads++
	return s.snapshots[sessionID], s.versions[sessionID], nil
}

func (s *testPlanStore) SavePlanSnapshot(_ context.Context, sessionID string, snapshot planpkg.Snapshot) (int64, error) {
	s.saves++
	if s.saveErr != nil {
		return 0, s.saveErr
	}
	s.versions[sessionID]++
	s.snapshots[sessionID] = snapshot
	return s.versions[sessionID], nil
}

func (s *testPlanStore) DeletePlanSnapshot(_ context.Context, sessionID string) error {
	s.deletes++
	delete(s.snapshots, sessionID)
	delete(s.versions, sessionID)
	return nil
}

func TestUpdatePlanToolSpec(t *testing.T) {
	tool := NewUpdatePlanTool(NewPlanController(nil))
	spec := tool.Spec()
	if spec.Name != UpdatePlanToolName {
		t.Fatalf("name = %q", spec.Name)
	}
	for _, want := range []string{`"plan"`, `"step"`, `"status"`, `In each item, use exactly "step" and "status"`} {
		if !strings.Contains(spec.Description, want) {
			t.Fatalf("description missing %q: %q", want, spec.Description)
		}
	}
	if got := spec.Schema["required"]; !reflect.DeepEqual(got, []string{"plan"}) {
		t.Fatalf("required = %#v", got)
	}
	properties := spec.Schema["properties"].(map[string]any)
	planSchema := properties["plan"].(map[string]any)
	if planSchema["maxItems"] != 20 {
		t.Fatalf("maxItems = %#v", planSchema["maxItems"])
	}
	itemProps := planSchema["items"].(map[string]any)["properties"].(map[string]any)
	status := itemProps["status"].(map[string]any)
	if !reflect.DeepEqual(status["enum"], []string{"pending", "in_progress", "completed"}) {
		t.Fatalf("status enum = %#v", status["enum"])
	}
}

func TestUpdatePlanToolExecutePersistsAndClears(t *testing.T) {
	store := newTestPlanStore()
	controller := NewPlanController(store)
	tool := NewUpdatePlanTool(controller)
	ctx := llm.ContextWithSessionID(context.Background(), "session-1")

	output, err := tool.Execute(ctx, json.RawMessage(`{"explanation":"starting","plan":[{"step":"Inspect","status":"in_progress"}]}`))
	if err != nil {
		t.Fatalf("Execute update: %v", err)
	}
	if output.Content != "Plan updated" || store.saves != 1 {
		t.Fatalf("update output/store = %#v saves=%d", output, store.saves)
	}
	if got := store.snapshots["session-1"].Plan[0].Step; got != "Inspect" {
		t.Fatalf("stored plan step = %q", got)
	}

	output, err = tool.Execute(ctx, json.RawMessage(`{"plan":[]}`))
	if err != nil {
		t.Fatalf("Execute clear: %v", err)
	}
	if output.Content != "Plan cleared" || store.deletes != 1 {
		t.Fatalf("clear output/store = %#v deletes=%d", output, store.deletes)
	}
	if _, ok := store.snapshots["session-1"]; ok {
		t.Fatal("cleared plan remains stored")
	}
}

func TestUpdatePlanToolDoesNotCommitOnPersistenceFailure(t *testing.T) {
	store := newTestPlanStore()
	store.saveErr = errors.New("disk full")
	controller := NewPlanController(store)
	tool := NewUpdatePlanTool(controller)
	ctx := llm.ContextWithSessionID(context.Background(), "session-1")

	_, err := tool.Execute(ctx, json.RawMessage(`{"plan":[{"step":"Inspect","status":"pending"}]}`))
	if err == nil || !strings.Contains(err.Error(), "persist plan") {
		t.Fatalf("Execute error = %v", err)
	}
	messages, prepareErr := tool.PrepareRequestContext(context.Background(), "session-1", []llm.Message{llm.UserText("continue")})
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	if strings.Contains(messagesText(messages), "Inspect") {
		t.Fatalf("failed persistence changed in-memory snapshot: %#v", messages)
	}
}

func TestUpdatePlanToolValidationErrorExplainsExactShape(t *testing.T) {
	tool := NewUpdatePlanTool(NewPlanController(nil))
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "steps alias", raw: `{"steps":[{"id":"inspect","title":"Inspect code","status":"in_progress"}]}`, want: `unknown field "steps"`},
		{name: "string plan", raw: `{"plan":"Inspect code","steps":[]}`, want: "cannot unmarshal string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), json.RawMessage(tt.raw))
			if err == nil {
				t.Fatal("invalid plan shape accepted")
			}
			message := err.Error()
			for _, want := range []string{
				tt.want,
				`required array field "plan"`,
				`Use exactly "step" and "status" in each plan item`,
				`{"explanation":"optional concise reason","plan":[{"step":"Inspect code","status":"in_progress"}]}`,
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("error missing %q: %v", want, err)
				}
			}
		})
	}
}

func TestUpdatePlanToolValidationAndPreview(t *testing.T) {
	tool := NewUpdatePlanTool(NewPlanController(nil))
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"plan":[{"step":"A","status":"in_progress"},{"step":"B","status":"in_progress"}]}`)); err == nil {
		t.Fatal("multiple in-progress steps accepted")
	}
	preview := tool.Preview(json.RawMessage(`{"explanation":"moving on","plan":[{"step":"A","status":"pending"}]}`))
	if !strings.Contains(preview, "1 step") {
		t.Fatalf("Preview() = %q", preview)
	}
	if got := tool.Preview(json.RawMessage(`not json`)); got != "(update plan)" {
		t.Fatalf("invalid Preview() = %q", got)
	}
}

func TestPlanControllersIsolateParentAndChildSessions(t *testing.T) {
	store := newTestPlanStore()
	parent := NewUpdatePlanTool(NewPlanController(store))
	child := NewUpdatePlanTool(NewPlanController(store))
	parentCtx := llm.ContextWithSessionID(context.Background(), "parent-session")
	childCtx := llm.ContextWithSessionID(context.Background(), "child-session")

	if _, err := parent.Execute(parentCtx, json.RawMessage(`{"plan":[{"step":"Parent work","status":"in_progress"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Execute(childCtx, json.RawMessage(`{"plan":[{"step":"Child work","status":"in_progress"}]}`)); err != nil {
		t.Fatal(err)
	}
	if got := store.snapshots["parent-session"].Plan[0].Step; got != "Parent work" {
		t.Fatalf("parent snapshot = %q", got)
	}
	if got := store.snapshots["child-session"].Plan[0].Step; got != "Child work" {
		t.Fatalf("child snapshot = %q", got)
	}
	if _, err := child.Execute(childCtx, json.RawMessage(`{"plan":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.snapshots["child-session"]; ok {
		t.Fatal("child clear left child snapshot")
	}
	if got := store.snapshots["parent-session"].Plan[0].Step; got != "Parent work" {
		t.Fatalf("child clear mutated parent snapshot: %q", got)
	}
}

func TestUpdatePlanRestoresAcrossControllerRestartOnlyWhenCallable(t *testing.T) {
	store := newTestPlanStore()
	store.snapshots["resume"] = planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Resume work", Status: planpkg.StatusInProgress}}}
	store.versions["resume"] = 4

	provider := llm.NewMockProvider("mock").AddTextResponse("continuing")
	tool := NewUpdatePlanTool(NewPlanController(store))
	engine := llm.NewEngine(provider, nil)
	engine.RegisterTool(tool)
	stream, err := engine.Stream(context.Background(), llm.Request{
		SessionID: "resume",
		Messages:  []llm.Message{llm.UserText("continue")},
		Tools:     []llm.ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
	}
	_ = stream.Close()
	requests := provider.RecordedRequests()
	restored := false
	if len(requests) == 1 {
		for _, message := range requests[0].Messages {
			if message.Role == llm.RoleDeveloper && strings.Contains(llm.MessageText(message), "Resume work") {
				restored = true
			}
		}
	}
	if !restored {
		t.Fatalf("resumed provider request = %#v", requests)
	}

	filteredStore := newTestPlanStore()
	filteredTool := NewUpdatePlanTool(NewPlanController(filteredStore))
	filteredProvider := llm.NewMockProvider("mock").AddTextResponse("plain")
	filteredEngine := llm.NewEngine(filteredProvider, nil)
	filteredEngine.RegisterTool(filteredTool)
	stream, err = filteredEngine.Stream(context.Background(), llm.Request{SessionID: "resume", Messages: []llm.Message{llm.UserText("plain")}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
	}
	_ = stream.Close()
	if filteredStore.loads != 0 {
		t.Fatalf("unavailable plan tool loaded store %d times", filteredStore.loads)
	}
}

func TestPlanControllerPromptGuidanceIsCallableOnlyContext(t *testing.T) {
	controller := NewPlanController(nil)
	controller.SetPromptGuidance(true)
	tool := NewUpdatePlanTool(controller)
	messages, err := tool.PrepareRequestContext(context.Background(), "", []llm.Message{llm.UserText("implement")})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != llm.RoleDeveloper {
		t.Fatalf("callable guidance messages = %#v", messages)
	}
	guidance := llm.MessageText(messages[0])
	if !strings.Contains(guidance, "<update_plan_guidance>") {
		t.Fatalf("callable guidance messages = %#v", messages)
	}
	for _, want := range []string{
		`{"explanation":"optional concise reason","plan":[{"step":"Inspect code","status":"in_progress"}]}`,
		`use only the optional "explanation" alongside it`,
		`Set "plan" to an array`,
		`use exactly "step" for its text and "status" for its state`,
	} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("guidance missing %q: %q", want, guidance)
		}
	}
	messages, err = tool.PrepareRequestContext(context.Background(), "", messages)
	if err != nil || len(messages) != 2 {
		t.Fatalf("guidance duplicated: len=%d err=%v", len(messages), err)
	}
}

func TestPlanControllerRestoresOnlyActiveMissingContext(t *testing.T) {
	store := newTestPlanStore()
	store.snapshots["session-1"] = planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Keep working", Status: planpkg.StatusInProgress}}}
	store.versions["session-1"] = 3
	tool := NewUpdatePlanTool(NewPlanController(store))

	messages := []llm.Message{llm.UserText("continue")}
	restored, err := tool.PrepareRequestContext(context.Background(), "session-1", messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 || restored[0].Role != llm.RoleDeveloper || !strings.Contains(llm.MessageText(restored[0]), "Keep working") {
		t.Fatalf("restored messages = %#v", restored)
	}
	restoredAgain, err := tool.PrepareRequestContext(context.Background(), "session-1", restored)
	if err != nil {
		t.Fatal(err)
	}
	if len(restoredAgain) != 2 || store.loads != 1 {
		t.Fatalf("duplicate restoration/load: len=%d loads=%d", len(restoredAgain), store.loads)
	}
}

func TestPlanControllerSkipsHistoryCompletedAndFilteredCompactionDuplicates(t *testing.T) {
	store := newTestPlanStore()
	active := planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Old persisted", Status: planpkg.StatusPending}}}
	store.snapshots["session-1"] = active
	store.versions["session-1"] = 1
	tool := NewUpdatePlanTool(NewPlanController(store))

	callArgs := json.RawMessage(`{"plan":[{"step":"Already represented","status":"in_progress"}]}`)
	history := []llm.Message{
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call", Name: UpdatePlanToolName, Arguments: callArgs}}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call", Name: UpdatePlanToolName, Content: "Plan updated"}}}},
	}
	got, err := tool.PrepareRequestContext(context.Background(), "session-1", history)
	if err != nil || len(got) != len(history) {
		t.Fatalf("history restoration = len %d err %v", len(got), err)
	}
	if store.saves != 1 || store.snapshots["session-1"].Plan[0].Step != "Already represented" {
		t.Fatalf("history snapshot did not reconcile durable state: saves=%d snapshot=%#v", store.saves, store.snapshots["session-1"])
	}

	result := &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("summary")}}
	if err := tool.PrepareCompactionContext(context.Background(), "session-1", result); err != nil {
		t.Fatal(err)
	}
	if len(result.EphemeralMessages) != 1 || !strings.Contains(llm.MessageText(result.EphemeralMessages[0]), "Already represented") {
		t.Fatalf("compaction restoration = %#v", result.EphemeralMessages)
	}

	completedStore := newTestPlanStore()
	completedStore.snapshots["done"] = planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Finished", Status: planpkg.StatusCompleted}}}
	completedStore.versions["done"] = 1
	completedTool := NewUpdatePlanTool(NewPlanController(completedStore))
	unchanged, err := completedTool.PrepareRequestContext(context.Background(), "done", []llm.Message{llm.UserText("next")})
	if err != nil || len(unchanged) != 1 {
		t.Fatalf("completed plan was restored: %#v err=%v", unchanged, err)
	}
}

func TestPlanControllerDoesNotBlockOtherSessionsDuringStoreIO(t *testing.T) {
	store := &blockingPlanStore{
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
	tool := NewUpdatePlanTool(NewPlanController(store))
	blockedDone := make(chan error, 1)
	go func() {
		_, err := tool.PrepareRequestContext(context.Background(), "blocked", []llm.Message{llm.UserText("wait")})
		blockedDone <- err
	}()
	select {
	case <-store.blocked:
	case <-time.After(time.Second):
		t.Fatal("blocked session never reached store")
	}

	fastDone := make(chan error, 1)
	go func() {
		_, err := tool.PrepareRequestContext(context.Background(), "fast", []llm.Message{llm.UserText("go")})
		fastDone <- err
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("fast session restore: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(store.release)
		<-blockedDone
		t.Fatal("slow store I/O for one session blocked another session")
	}
	close(store.release)
	if err := <-blockedDone; err != nil {
		t.Fatalf("blocked session restore: %v", err)
	}
}

func TestPlanControllerStoreAttachmentDoesNotResurrectClearedPlan(t *testing.T) {
	controller := NewPlanController(nil)
	tool := NewUpdatePlanTool(controller)
	ctx := llm.ContextWithSessionID(context.Background(), "session-1")
	if _, err := tool.Execute(ctx, json.RawMessage(`{"plan":[{"step":"Temporary","status":"in_progress"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"plan":[]}`)); err != nil {
		t.Fatal(err)
	}

	store := newTestPlanStore()
	store.snapshots["session-1"] = planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Stale durable plan", Status: planpkg.StatusInProgress}}}
	store.versions["session-1"] = 7
	controller.SetStore(store)
	messages, err := tool.PrepareRequestContext(context.Background(), "session-1", []llm.Message{llm.UserText("continue")})
	if err != nil {
		t.Fatal(err)
	}
	if store.loads != 0 || strings.Contains(messagesText(messages), "Stale durable plan") {
		t.Fatalf("cleared plan was reloaded: loads=%d messages=%#v", store.loads, messages)
	}
}

func TestPlanControllerDoesNotLeakBlankSessionStateIntoFreshRun(t *testing.T) {
	tool := NewUpdatePlanTool(NewPlanController(nil))
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"plan":[{"step":"First run only","status":"in_progress"}]}`)); err != nil {
		t.Fatal(err)
	}
	messages, err := tool.PrepareRequestContext(context.Background(), "", []llm.Message{llm.UserText("unrelated run")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(messagesText(messages), "First run only") {
		t.Fatalf("blank-session plan leaked into fresh run: %#v", messages)
	}
}

func messagesText(messages []llm.Message) string {
	var parts []string
	for _, message := range messages {
		parts = append(parts, llm.MessageText(message))
	}
	return strings.Join(parts, "\n")
}

func TestPlanControllerReconcilesHistoryClearToStore(t *testing.T) {
	store := newTestPlanStore()
	store.snapshots["session-1"] = planpkg.Snapshot{Plan: []planpkg.Step{{Step: "Stale", Status: planpkg.StatusInProgress}}}
	store.versions["session-1"] = 2
	tool := NewUpdatePlanTool(NewPlanController(store))
	history := []llm.Message{
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "clear", Name: UpdatePlanToolName, Arguments: json.RawMessage(`{"plan":[]}`)}}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "clear", Name: UpdatePlanToolName, Content: "Plan cleared"}}}},
	}
	if _, err := tool.PrepareRequestContext(context.Background(), "session-1", history); err != nil {
		t.Fatal(err)
	}
	if store.deletes != 1 {
		t.Fatalf("history clear did not delete durable state: deletes=%d", store.deletes)
	}
}

func TestUpdatePlanIsOptInAndNonMutating(t *testing.T) {
	if containsToolName(StandardToolNames(), UpdatePlanToolName) {
		t.Fatal("update_plan appeared in standard tools")
	}
	if !containsToolName(ValidToolNames(), UpdatePlanToolName) {
		t.Fatal("update_plan missing from opt-in/valid names")
	}
	if !ValidToolName(UpdatePlanToolName) {
		t.Fatal("update_plan rejected by validation")
	}
	if GetToolKind(UpdatePlanToolName) != KindSessionState {
		t.Fatalf("kind = %q", GetToolKind(UpdatePlanToolName))
	}
	for _, kind := range MutatorKinds {
		if kind == KindSessionState {
			t.Fatal("session-state tool included in filesystem mutators")
		}
	}
}

func containsToolName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

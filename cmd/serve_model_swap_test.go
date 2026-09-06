package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type modelSwapSessionViewStore struct {
	session.Store
	sess *session.Session
	err  error
}

func (s *modelSwapSessionViewStore) Get(context.Context, string) (*session.Session, error) {
	return s.sess, s.err
}

type blockingModelSwapReplaceStore struct {
	session.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockingModelSwapReplaceStore) ReplaceMessages(ctx context.Context, sessionID string, messages []session.Message) error {
	close(s.entered)
	select {
	case <-s.release:
		return s.Store.ReplaceMessages(ctx, sessionID, messages)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBeginResponseModelSwapRestoresWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name            string
		requestedModel  string
		removeWorkspace bool
	}{
		{name: "effort-only switch", requestedModel: "old-model"},
		{name: "model switch", requestedModel: "new-model"},
		{name: "restoration failure rolls back", requestedModel: "new-model", removeWorkspace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, store := newServeProjectTestServer(t)
			project := &session.Project{Name: "Swap project", CanonicalDir: t.TempDir()}
			if err := store.CreateProject(ctx, project); err != nil {
				t.Fatal(err)
			}
			sess := &session.Session{
				ID: session.NewID(), Provider: "mock", ProviderKey: "mock", Model: "old-model",
				Mode: session.ModeChat, Origin: session.OriginWeb,
				ProjectID: project.ID, CWD: project.CanonicalDir,
			}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			newToolManager := func() *tools.ToolManager {
				t.Helper()
				cfg := tools.ToolConfig{Enabled: []string{tools.ReadFileToolName}, RequireExplicitWorkingDir: true}
				mgr, err := tools.NewToolManager(&cfg, nil)
				if err != nil {
					t.Fatal(err)
				}
				return mgr
			}
			previous, prevProvider := newCloseTrackingServeRuntime()
			previous.providerKey, previous.defaultModel = "mock", "old-model"
			previous.store, previous.toolMgr = store, newToolManager()
			candidate, candProvider := newCloseTrackingServeRuntime()
			candidate.providerKey, candidate.defaultModel = "mock", tc.requestedModel
			candidate.store, candidate.toolMgr = store, newToolManager()
			manager := newServeSessionManager(time.Minute, 10, nil)
			t.Cleanup(manager.Close)
			srv.sessionMgr = manager
			putTestSession(manager, sess.ID, previous)
			srv.runtimeFactory = func(context.Context, string, string) (*serveRuntime, error) {
				// The previous runtime has already been restored by this point.
				// Make only the candidate's workspace initialization fail.
				if tc.removeWorkspace {
					if err := os.RemoveAll(project.CanonicalDir); err != nil {
						return nil, err
					}
				}
				return candidate, nil
			}

			if err := srv.ensureRuntimeBaseDirForSession(ctx, sess.ID, previous); err != nil {
				t.Fatal(err)
			}
			previous.toolMgr.ApprovalMgr.WorkspacePromptFunc = func(string) (tools.WorkspaceApprovalResult, error) {
				return tools.WorkspaceApprovalResult{Approved: true}, nil
			}
			path := filepath.Join(project.CanonicalDir, "file.txt")
			if err := os.WriteFile(path, []byte("workspace fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if outcome, err := previous.toolMgr.ApprovalMgr.CheckPathApproval(tools.ReadFileToolName, path, path, false); err != nil || outcome != tools.ProceedAlways {
				t.Fatalf("confirm primary workspace: outcome=%v err=%v", outcome, err)
			}
			grantsBefore, err := store.ListWorkspaceGrants(ctx, sess.ID)
			if err != nil || len(grantsBefore) != 1 {
				t.Fatalf("initial workspace grants = %#v, err=%v", grantsBefore, err)
			}
			candidate.toolMgr.ApprovalMgr.WorkspacePromptFunc = func(string) (tools.WorkspaceApprovalResult, error) {
				t.Error("model switch requested another primary workspace approval")
				return tools.WorkspaceApprovalResult{}, nil
			}

			plan := responseModelSwapPlan{
				enabled: true, previousProvider: "mock", previousModel: "old-model", previousEffort: "medium",
				requestedProvider: "mock", requestedModel: tc.requestedModel, requestedEffort: "high",
			}
			swap, err := srv.beginResponseModelSwap(ctx, sess.ID, plan, []llm.Message{llm.UserText("continue")})
			if swap != nil {
				t.Cleanup(swap.markRolledBack)
			}
			if tc.removeWorkspace {
				if err == nil || swap != nil {
					t.Fatalf("workspace restoration should fail: swap=%p err=%v", swap, err)
				}
				if current, ok := manager.Get(sess.ID); !ok || current != previous {
					t.Fatalf("rollback runtime = %p, want previous %p", current, previous)
				}
				if !candProvider.closed.Load() || prevProvider.closed.Load() {
					t.Fatalf("rollback cleanup: candidate closed=%v, previous closed=%v", candProvider.closed.Load(), prevProvider.closed.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := swap.candidate.toolMgr.BaseDir(); !sameServePath(got, project.CanonicalDir) {
				t.Fatalf("candidate working directory = %q, want %q", got, project.CanonicalDir)
			}
			approval := swap.candidate.toolMgr.ApprovalMgr
			capabilities := approval.WorkspaceCapabilities()
			if len(capabilities) != 1 || !capabilities[0].Primary || capabilities[0].Status != "confirmed" || !sameServePath(capabilities[0].Path, project.CanonicalDir) {
				t.Fatalf("candidate lost confirmed primary workspace: %#v", capabilities)
			}
			if outcome, err := approval.CheckPathApproval(tools.ReadFileToolName, path, path, false); err != nil || outcome != tools.ProceedAlways {
				t.Fatalf("resumed path approval: outcome=%v err=%v", outcome, err)
			}
			grant, err := approval.GrantWorkspace(ctx, project.CanonicalDir, session.WorkspaceAccessWrite, "continue work in the same project")
			if err != nil || grant.Changed || !grant.Persisted || !grant.Capability.Primary {
				t.Fatalf("grant of existing primary was not idempotent: %#v, err=%v", grant, err)
			}
			grantsAfter, err := store.ListWorkspaceGrants(ctx, sess.ID)
			if err != nil || !reflect.DeepEqual(grantsAfter, grantsBefore) {
				t.Fatalf("swap changed persisted grants: before=%#v after=%#v err=%v", grantsBefore, grantsAfter, err)
			}
		})
	}
}

func TestModelSwapStartMessageNamesEffortOnlyChange(t *testing.T) {
	plan := responseModelSwapPlan{
		previousProvider:  "chatgpt",
		previousModel:     "gpt-5.6-sol",
		previousEffort:    "medium",
		requestedProvider: "chatgpt",
		requestedModel:    "gpt-5.6-sol",
		requestedEffort:   "high",
	}
	got := plan.startMessage(nil)
	want := "Switching reasoning effort on chatgpt:gpt-5.6-sol: medium → high; trying existing context…"
	if got != want {
		t.Fatalf("startMessage() = %q, want %q", got, want)
	}
}

func TestModelSwapStartMessageIncludesEffortWhenModelAlsoChanges(t *testing.T) {
	plan := responseModelSwapPlan{
		previousProvider:  "chatgpt",
		previousModel:     "gpt-5.6-sol",
		previousEffort:    "",
		requestedProvider: "chatgpt",
		requestedModel:    "gpt-5.6-luna",
		requestedEffort:   "high",
	}
	got := plan.startMessage(nil)
	want := "Switching model: chatgpt:gpt-5.6-sol / auto → chatgpt:gpt-5.6-luna / high; trying existing context…"
	if got != want {
		t.Fatalf("startMessage() = %q, want %q", got, want)
	}
}

func TestInsertModelSwapMarkerFallsBackToLatestUser(t *testing.T) {
	history := []llm.Message{
		llm.UserText("original trigger"),
		llm.AssistantText("old reply"),
		llm.UserText("normalized trigger"),
		llm.AssistantText("current reply"),
	}
	updated := insertModelSwapMarkerAfterTrigger(
		history,
		llm.ModelSwapEventMessage(llm.ModelSwapMarker{Status: "succeeded"}),
	)

	if len(updated) != 5 || updated[2].Role != llm.RoleUser || updated[3].Role != llm.RoleEvent || updated[4].Role != llm.RoleAssistant {
		t.Fatalf("marker matched an older repeated prompt instead of the latest user: %#v", updated)
	}
}

func TestInsertModelSwapMarkerImageOnlyTriggerUsesLatestUser(t *testing.T) {
	imageOnly := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartImage, ImagePath: "image.png"}}}
	history := []llm.Message{
		llm.UserText("handover summary"),
		imageOnly,
		llm.AssistantText("reply"),
	}
	updated := insertModelSwapMarkerAfterTrigger(
		history,
		llm.ModelSwapEventMessage(llm.ModelSwapMarker{Status: "succeeded"}),
	)

	if len(updated) != 4 || updated[1].Role != llm.RoleUser || updated[2].Role != llm.RoleEvent || updated[3].Role != llm.RoleAssistant {
		t.Fatalf("image-only trigger anchored marker to handover context: %#v", updated)
	}
}

func TestPersistModelSwapMarkerSurvivesCancelledRequestContext(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "new", Model: "new-model", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	history := []llm.Message{llm.UserText("continue"), llm.AssistantText("reply")}
	if err := store.ReplaceMessages(ctx, sess.ID, []session.Message{
		*session.NewMessage(sess.ID, history[0], 0),
		*session.NewMessage(sess.ID, history[1], 1),
	}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	candidate := &serveRuntime{store: store, history: copyLLMMessageSlice(history)}
	server := &serveServer{store: store}
	plan := responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	server.persistModelSwapMarker(cancelledCtx, sess.ID, plan, candidate, "succeeded", "naive")

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(messages) != 3 || messages[1].Role != llm.RoleEvent {
		t.Fatalf("cancelled request context lost durable model-swap marker: %#v", messages)
	}
}

func TestPersistModelSwapMarkerUsesServerStoreWhenCandidateStoreMissing(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "new", Model: "new-model", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	history := []llm.Message{llm.UserText("continue"), llm.AssistantText("reply")}
	if err := store.ReplaceMessages(ctx, sess.ID, []session.Message{
		*session.NewMessage(sess.ID, history[0], 0),
		*session.NewMessage(sess.ID, history[1], 1),
	}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	candidate := &serveRuntime{history: copyLLMMessageSlice(history)}
	server := &serveServer{store: store}
	plan := responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}
	server.persistModelSwapMarker(ctx, sess.ID, plan, candidate, "succeeded", "naive")

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(messages) != 3 || messages[0].Role != llm.RoleUser || messages[1].Role != llm.RoleEvent || messages[2].Role != llm.RoleAssistant {
		t.Fatalf("server-store fallback persisted wrong marker order: %#v", messages)
	}
	if candidate.store != store {
		t.Fatal("candidate runtime did not adopt the server session store")
	}
}

func TestPersistModelSwapMarkerTreatsZeroValueSessionAsUncompacted(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "new", Model: "new-model", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	history := []llm.Message{llm.UserText("continue"), llm.AssistantText("reply")}
	if err := store.ReplaceMessages(ctx, sess.ID, []session.Message{
		*session.NewMessage(sess.ID, history[0], 0),
		*session.NewMessage(sess.ID, history[1], 1),
	}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	viewStore := &modelSwapSessionViewStore{
		Store: store,
		sess:  &session.Session{ID: sess.ID, CompactionSeq: 0, CompactionCount: 0},
	}
	candidate := &serveRuntime{store: viewStore, history: copyLLMMessageSlice(history)}
	server := &serveServer{store: viewStore}
	plan := responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}
	server.persistModelSwapMarker(ctx, sess.ID, plan, candidate, "succeeded", "naive")

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(messages) != 3 || messages[1].Role != llm.RoleEvent {
		t.Fatalf("zero-value session was misclassified as compacted: %#v", messages)
	}
}

func TestPersistModelSwapMarkerDoesNotRewriteUnknownCompactionState(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "new", Model: "new-model", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, msg := range []llm.Message{llm.UserText("old prompt"), llm.AssistantText("old reply")} {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	activeHistory := []llm.Message{
		llm.UserText("compacted context"),
		llm.UserText("continue"),
		llm.AssistantText("new reply"),
	}
	compacted := make([]session.Message, 0, len(activeHistory))
	for _, msg := range activeHistory {
		compacted = append(compacted, *session.NewMessage(sess.ID, msg, -1))
	}
	if err := store.CompactMessages(ctx, sess.ID, compacted); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	before, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get before marker: %v", err)
	}

	viewStore := &modelSwapSessionViewStore{Store: store, err: errors.New("metadata unavailable")}
	candidate := &serveRuntime{store: viewStore, history: copyLLMMessageSlice(activeHistory)}
	server := &serveServer{store: viewStore}
	plan := responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}
	server.persistModelSwapMarker(ctx, sess.ID, plan, candidate, "succeeded", "naive")

	after, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after marker: %v", err)
	}
	if after.CompactionSeq != before.CompactionSeq || after.CompactionCount != before.CompactionCount {
		t.Fatalf("unknown compaction state cleared boundary from %d/%d to %d/%d", before.CompactionSeq, before.CompactionCount, after.CompactionSeq, after.CompactionCount)
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(messages) < 2 || messages[0].TextContent != "old prompt" || messages[1].TextContent != "old reply" {
		t.Fatalf("unknown compaction state rewrote pre-compaction scrollback: %#v", messages)
	}
	if candidate.historyPersisted {
		t.Fatal("marker history was marked persisted after metadata lookup failure")
	}
}

func TestPersistModelSwapMarkerHoldsRuntimeLockThroughPersistence(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "new", Model: "new-model", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	history := []llm.Message{llm.UserText("continue"), llm.AssistantText("reply")}
	if err := store.ReplaceMessages(ctx, sess.ID, []session.Message{
		*session.NewMessage(sess.ID, history[0], 0),
		*session.NewMessage(sess.ID, history[1], 1),
	}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	blocking := &blockingModelSwapReplaceStore{
		Store:   store,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	candidate := &serveRuntime{store: blocking, history: copyLLMMessageSlice(history)}
	server := &serveServer{store: blocking}
	plan := responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}

	done := make(chan struct{})
	go func() {
		server.persistModelSwapMarker(ctx, sess.ID, plan, candidate, "succeeded", "naive")
		close(done)
	}()

	<-blocking.entered
	lockWasHeld := !candidate.mu.TryLock()
	if !lockWasHeld {
		candidate.mu.Unlock()
	}
	close(blocking.release)
	<-done

	if !lockWasHeld {
		t.Fatal("runtime lock was released while model-swap history was being persisted")
	}
}

func TestPersistModelSwapMarkerPreservesCompactedScrollback(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "new", Model: "new-model", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, msg := range []llm.Message{llm.UserText("old prompt"), llm.AssistantText("old reply")} {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	activeHistory := []llm.Message{
		llm.UserText("compacted context"),
		llm.UserText("continue"),
		llm.AssistantText("new reply"),
	}
	compacted := make([]session.Message, 0, len(activeHistory))
	for _, msg := range activeHistory {
		compacted = append(compacted, *session.NewMessage(sess.ID, msg, -1))
	}
	if err := store.CompactMessages(ctx, sess.ID, compacted); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}
	before, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get before marker: %v", err)
	}

	candidate := &serveRuntime{store: store, history: copyLLMMessageSlice(activeHistory)}
	server := &serveServer{store: store}
	plan := responseModelSwapPlan{enabled: true, previousProvider: "old", previousModel: "old-model", requestedProvider: "new", requestedModel: "new-model"}
	server.persistModelSwapMarker(ctx, sess.ID, plan, candidate, "succeeded", "naive")

	after, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after marker: %v", err)
	}
	if after.CompactionSeq != before.CompactionSeq || after.CompactionCount != before.CompactionCount {
		t.Fatalf("compaction boundary changed from seq/count %d/%d to %d/%d", before.CompactionSeq, before.CompactionCount, after.CompactionSeq, after.CompactionCount)
	}

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(messages) < 2 || messages[0].TextContent != "old prompt" || messages[1].TextContent != "old reply" {
		t.Fatalf("pre-compaction scrollback was not preserved: %#v", messages)
	}
	markerIndex := -1
	for i, msg := range messages {
		if _, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
			markerIndex = i
			break
		}
	}
	if markerIndex == 0 || markerIndex+1 >= len(messages) || messages[markerIndex-1].TextContent != "continue" || messages[markerIndex+1].TextContent != "new reply" {
		t.Fatalf("model-swap marker has wrong compacted-history position, index=%d messages=%#v", markerIndex, messages)
	}
}

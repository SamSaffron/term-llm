import { batch } from '@preact/signals';
import { APIError } from '../api/client';
import type {
  ApprovalMode,
  ApprovalPrompt,
  AskUserPrompt,
  CurrentPlan,
  Goal,
  Session,
} from '../domain/types';
import { planSummary } from '../domain/plan';
import { updateSessionRoute } from '../platform/routing';
import type { TabEventType } from '../platform/tab-sync';
import type { AppStoreServices } from './app-store-services';
import type { SessionStore } from './session-store';
import type { ComposerStore } from './composer-store';
import type { RunEngine } from './run-engine';
import type { InteractionStore } from './interaction-store';
import type { SideQuestionStore } from './side-question-store';
import type { SkillStore } from './skill-store';
import type { BranchStore } from './branch-store';
import type { ReviewStore } from './review-store';
import type { PlanStore } from './plan-store';
import type { GoalStore } from './goal-store';
import type { WidgetStore } from './widget-store';
import { approvalPrompt, askUserPrompt, listFrom, recordValue } from './store-utils';

export interface SelectionStoreHost {
  publishSessionChange: (
    type?: TabEventType,
    sessionId?: string,
    responseId?: string,
    revision?: number,
    operationId?: string,
  ) => void;
}

/** Owns navigation, selection generations, and authoritative session hydration. */
export class SelectionStore {
  private epoch = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly sessionsStore: SessionStore,
    private readonly composer: ComposerStore,
    private readonly runEngine: RunEngine,
    private readonly interactions: InteractionStore,
    private readonly sideQuestions: SideQuestionStore,
    private readonly skillStore: SkillStore,
    private readonly branches: BranchStore,
    private readonly review: ReviewStore,
    private readonly plans: PlanStore,
    private readonly goals: GoalStore,
    private readonly widgets: WidgetStore,
    private readonly host: SelectionStoreHost,
  ) {}

  get generation(): number {
    return this.epoch;
  }

  async selectSession(session: Session, replace = false): Promise<void> {
    this.composer.persist();
    this.composer.releaseResources(this.composer.attachments.peek(), false);
    this.sideQuestions.reset();
    const epoch = ++this.epoch;
    batch(() => {
      this.sessionsStore.activate(session);
      this.plans.current.value = null;
      this.plans.openState.value = false;
      this.plans.seen.value = null;
      this.goals.state.value = null;
      this.interactions.askUser.value = null;
      this.interactions.approval.value = null;
      this.branches.tree.value = null;
      this.branches.pathCount.value = 0;
      if (this.review.diff.value.sessionId !== session.id)
        this.review.diff.value = {
          ...this.review.diff.value,
          sessionId: session.id,
          git: Boolean(session.fileChangeSummary?.git),
          files: [],
          error: '',
          comments: this.review.diff.value.comments.filter(
            (comment) => !comment.sessionId || comment.sessionId === session.id,
          ),
        };
    });
    this.services.storage.setItem(this.services.keys.activeSession, session.id);
    this.services.storage.removeItem(this.services.keys.draftSessionActive);
    updateSessionRoute(this.services.config.prefix, session, replace);
    this.composer.restore(session.id, 'session');
    this.composer.syncRuntimeFromSession(session);
    // Sidebar/status metadata already identifies the running response. Begin
    // adopting it immediately instead of waiting for transcript hydration,
    // which does not own live-run state.
    if (session.activeResponseId)
      void this.runEngine.resumeResponse(session.id, session.activeResponseId);
    await this.loadSession(session.id, epoch);
    if (epoch !== this.epoch) return;
    const current =
      this.sessionsStore.sessions.value.find(
        (entry) => entry.id === this.sessionsStore.activeSessionId.value,
      ) || session;
    await this.skillStore.loadSkills(current.id).catch(() => {
      if (epoch === this.epoch) this.skillStore.skills.value = [];
    });
    if (epoch !== this.epoch) return;
    void this.branches.refresh(current.id);
    if (current.activeResponseId)
      void this.runEngine.resumeResponse(current.id, current.activeResponseId);
  }

  newChat(replace = false, projectId?: string, persistCurrent = true): void {
    // Bootstrap has not hydrated the active persisted draft yet. Saving the
    // empty initial composer here would look like a stale edit after reload.
    if (persistCurrent) this.composer.persist();
    this.composer.releaseResources(this.composer.attachments.peek(), false);
    this.sideQuestions.reset();
    ++this.epoch;
    const currentSession = this.sessionsStore.activeSession.peek();
    const requestedProject =
      projectId === undefined
        ? currentSession
          ? currentSession.projectId || ''
          : this.sessionsStore.activeProjectId.peek()
        : projectId;
    const selectedProject =
      requestedProject &&
      this.sessionsStore.projects.value.some(
        (project) =>
          project.id === requestedProject && !project.archived && project.available !== false,
      )
        ? requestedProject
        : '';
    this.composer.beginNewDraft(Boolean(currentSession), selectedProject);
    batch(() => {
      this.sessionsStore.activateDraft(selectedProject);
      this.plans.current.value = null;
      this.plans.openState.value = false;
      this.plans.seen.value = null;
      this.goals.state.value = null;
      this.review.diff.value = {
        ...this.review.diff.peek(),
        open: false,
        maximized: false,
      };
      this.interactions.askUser.value = null;
      this.interactions.approval.value = null;
      this.skillStore.skills.value = [];
      this.branches.tree.value = null;
      this.branches.pathCount.value = 0;
    });
    this.services.storage.removeItem(this.services.keys.activeSession);
    if (selectedProject)
      this.services.storage.setItem(this.services.keys.lastProject, selectedProject);
    updateSessionRoute(this.services.config.prefix, null, replace);
    this.composer.restore(this.composer.storageId(), 'draft');
  }

  async resolveAndSelectSession(id: string, replace = false): Promise<void> {
    try {
      const data = await this.services.endpoints.selectedSession(id);
      const source = recordValue(data.selected_session);
      if (!source) return this.newChat(replace);
      const session = this.sessionsStore.sessionFrom(source);
      const existing = this.sessionsStore.sessions.value.find((entry) => entry.id === session.id);
      if (!existing) this.sessionsStore.prepend(session);
      await this.selectSession(existing || session, replace);
    } catch (error) {
      this.services.toast(error, 'error');
    }
  }

  async loadSession(id: string, epoch = this.epoch): Promise<void> {
    const sampledAskUser = this.interactions.askUser.peek();
    const sampledApproval = this.interactions.approval.peek();
    const sampledInterjectionRevision = this.runEngine.interjectionStateRevision;
    try {
      const [state, selected] = await Promise.all([
        this.services.endpoints.sessionState(id),
        this.services.endpoints.selectedSession(id),
      ]);
      if (epoch !== this.epoch || this.sessionsStore.activeSessionId.peek() !== id) return;
      const selectedSource = recordValue(selected.selected_session) || {};
      const sideload = recordValue(selected.selected_transcript) || {};
      const bodies = recordValue(sideload.bodies) || {};
      const serverMessages = listFrom(bodies, 'messages', 'items');
      const lastResponseId = String(state.lastResponseId || state.last_response_id || '').trim();
      const stateActiveResponseId = String(state.active_response_id || '').trim();
      const stateReportsActive = Boolean(state.active_run || stateActiveResponseId);
      const selectedRevision = Number(
        bodies.rev ?? selectedSource.transcript_rev ?? selectedSource.rev,
      );
      const incoming = {
        ...this.sessionsStore.sessionFrom({
          id,
          ...selectedSource,
          // The selected transcript revision owns the message bodies being installed.
          transcript_rev: bodies.rev ?? selectedSource.transcript_rev ?? selectedSource.rev,
          messages: serverMessages,
        }),
        ...(Number.isFinite(selectedRevision) ? { messageBodiesRev: selectedRevision } : {}),
      };
      const currentIndex = this.sessionsStore.sessions.value.findIndex(
        (session) => session.id === id || (incoming.id && session.id === incoming.id),
      );
      const current =
        currentIndex >= 0 ? this.sessionsStore.sessions.value[currentIndex] : undefined;
      // selected_transcript is authoritative here, including an empty transcript.
      // Session state is authoritative for its durable continuation anchor.
      const policy = recordValue(state.approval_policy);
      const policyMode = (value: unknown): ApprovalMode | undefined => {
        const mode = String(value || '');
        return mode === 'prompt' || mode === 'auto' || mode === 'yolo' ? mode : undefined;
      };
      const updated = {
        // Selected transcript payloads own message bodies, not live response
        // ownership. Preserve the sidebar/status evidence sampled before this
        // request, and strengthen it with the session-state endpoint when that
        // endpoint confirms an active run.
        ...this.sessionsStore.mergeSession(current, incoming, true, true),
        ...(stateReportsActive
          ? {
              activeRun: true,
              ...(stateActiveResponseId ? { activeResponseId: stateActiveResponseId } : {}),
            }
          : {}),
        lastResponseId: lastResponseId || null,
        ...(policy
          ? {
              approvalDefaultMode: policyMode(policy.default_mode),
              approvalRequestedMode: policyMode(policy.requested_mode),
              approvalEffectiveMode: policyMode(policy.effective_mode),
              guardianAvailable: Boolean(policy.guardian_available),
              guardianAutoSuspended: Boolean(policy.guardian_auto_suspended),
            }
          : {
              approvalDefaultMode: this.services.config.approvalMode,
              approvalRequestedMode: this.services.config.approvalMode,
              approvalEffectiveMode: this.services.config.approvalMode,
              guardianAvailable: undefined,
              guardianAutoSuspended: false,
            }),
      };
      if (currentIndex >= 0 && current) this.sessionsStore.update(current.id, () => updated);
      else this.sessionsStore.prepend(updated);
      if (updated.id !== id) this.runEngine.rekeySession(id, updated.id, selectedSource);
      if (stateActiveResponseId)
        void this.runEngine.resumeResponse(updated.id, stateActiveResponseId);
      const planSource = state.current_plan || selectedSource.plan_summary;
      let loadedPlan: CurrentPlan | null = null;
      if (planSource && typeof planSource === 'object') {
        const raw = planSource as Record<string, unknown>;
        const plan = raw.plan || raw.steps;
        loadedPlan = Array.isArray(plan)
          ? { explanation: String(raw.explanation || ''), plan: plan as CurrentPlan['plan'] }
          : null;
      }
      this.plans.update(loadedPlan);
      this.plans.seen.value = loadedPlan ? planSummary(loadedPlan).signature : '';
      this.goals.state.value =
        state.goal && typeof state.goal === 'object' ? (state.goal as Goal) : updated.goal || null;
      this.widgets.apply(selected.widget_status);
      const asks = Array.isArray(state.pending_ask_users)
        ? state.pending_ask_users
        : [state.pending_ask_user];
      const approvals = Array.isArray(state.pending_approvals)
        ? state.pending_approvals
        : [state.pending_approval];
      const activeResponse = String(state.active_response_id || updated.activeResponseId || '');
      const recoveredAsks = asks
        .map((value) => askUserPrompt(value, updated.id))
        .filter((value): value is AskUserPrompt => Boolean(value?.callId));
      const recoveredApprovals = approvals
        .map((value) => approvalPrompt(value, updated.id))
        .filter((value): value is ApprovalPrompt => Boolean(value?.id));
      for (const prompt of recoveredAsks)
        this.interactions.upsert('ask-user', updated.id, activeResponse, prompt.callId!, prompt);
      for (const prompt of recoveredApprovals)
        this.interactions.upsert('approval', updated.id, activeResponse, prompt.id!, prompt);
      for (const resolved of listFrom(state, 'resolved_interactions')) {
        const requestId = String(resolved.request_id || '');
        const kind = String(resolved.kind || '') === 'approval' ? 'approval' : 'ask-user';
        if (requestId)
          this.interactions.resolve(
            kind,
            updated.id,
            activeResponse,
            requestId,
            String(resolved.outcome || 'resolved'),
            Number(resolved.resolved_at) || Date.now(),
          );
      }
      const pendingInteractionIDs = new Set([
        ...recoveredAsks.map((prompt) => `ask-user:${prompt.callId}`),
        ...recoveredApprovals.map((prompt) => `approval:${prompt.id}`),
      ]);
      const reconciledInteractions = { ...this.interactions.interactions.peek() };
      for (const [key, record] of Object.entries(reconciledInteractions)) {
        if (
          record.sessionId !== updated.id ||
          !['waiting', 'dismissed', 'submitting', 'failed'].includes(record.state) ||
          (activeResponse && record.responseId && record.responseId !== activeResponse) ||
          pendingInteractionIDs.has(`${record.kind}:${record.requestId}`)
        )
          continue;
        reconciledInteractions[key] = {
          ...record,
          state: activeResponse ? 'resolved-elsewhere' : 'cancelled-by-agent',
          outcome: activeResponse ? 'resolved' : 'cancelled-by-agent',
          resolvedAt: Date.now(),
        };
      }
      this.interactions.interactions.value = reconciledInteractions;
      if (this.interactions.askUser.peek() === sampledAskUser)
        this.interactions.askUser.value =
          recoveredAsks.find((prompt) =>
            this.interactions.shouldOpen('ask-user', updated.id, prompt.callId!),
          ) || null;
      if (this.interactions.approval.peek() === sampledApproval)
        this.interactions.approval.value =
          recoveredApprovals.find((prompt) =>
            this.interactions.shouldOpen('approval', updated.id, prompt.id!),
          ) || null;
      this.runEngine.reconcilePendingInterjections(updated.id, state, sampledInterjectionRevision);
      this.runEngine.reconcileLoadedIntents(updated.id, incoming.messages, Boolean(activeResponse));
      if (activeResponse)
        this.sessionsStore.patch(updated.id, { activeResponseId: activeResponse });
    } catch (error) {
      if (epoch === this.epoch) this.services.toast(error, 'error');
    }
  }

  async mutateTranscript(operation: 'undo' | 'redo'): Promise<void> {
    const session = this.sessionsStore.activeSession.value;
    if (!session || this.runEngine.streaming.value)
      return this.services.toast(`Cannot ${operation} while work is active.`, 'error');
    const durable = [...session.messages].reverse().find((message) => message.durableRowId);
    const owner = session.id;
    try {
      const result = await this.services.endpoints.mutateTranscript(owner, operation, {
        expected_rev: session.transcriptRev || 0,
        expected_head_id: durable?.durableRowId || 0,
      });
      await this.runEngine.refreshSessionMessages(owner, Number(result.rev) || 0);
      if (this.sessionsStore.activeSessionId.peek() === owner)
        this.composer.prompt.value = operation === 'undo' ? String(result.user_text || '') : '';
      this.services.toast(
        operation === 'undo'
          ? result.attachments_omitted
            ? 'Removed the latest turn. Attachments were not restored.'
            : 'Removed the latest turn. Your prompt is back in the composer.'
          : 'Restored the undone turn.',
        result.attachments_omitted ? 'error' : 'success',
      );
      if (this.review.diff.value.open) void this.review.loadDiff();
    } catch (error) {
      if (error instanceof APIError && error.status === 409)
        await this.runEngine.refreshSessionMessages(owner);
      this.services.toast(error, 'error');
    }
  }
}

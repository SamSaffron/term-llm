import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { Session } from '../domain/types';
import { errorMessage } from '../domain/text';
import type { AppStoreServices } from './app-store-services';
import type { Modal } from './store-types';

export interface CommitChange {
  path: string;
  old_path?: string;
  kind: string;
  staged?: boolean;
  unstaged?: boolean;
  untracked?: boolean;
  partially_staged?: boolean;
  additions?: number;
  deletions?: number;
  binary?: boolean;
}
export interface CommitStatus {
  branch?: string;
  detached?: boolean;
  staged: CommitChange[];
  unstaged: CommitChange[];
  untracked: CommitChange[];
  summary?: { files?: number; additions?: number; deletions?: number; binary_files?: number };
  fingerprint: Record<string, unknown>;
  status_token?: string;
  selection_available?: boolean;
  selection_unavailable_reason?: string;
  total_staged?: number;
  total_unstaged?: number;
  total_untracked?: number;
  truncated?: boolean;
}
export type CommitPhase =
  | 'closed'
  | 'loading'
  | 'choosing_scope'
  | 'planning_scope'
  | 'reviewing_scope'
  | 'staging'
  | 'drafting_message'
  | 'editing'
  | 'committing'
  | 'success'
  | 'error';
export interface CommitUIState {
  sessionId: string;
  phase: CommitPhase;
  intent: string;
  status: CommitStatus | null;
  selected: string[];
  scopeSummary: string;
  message: string;
  generated: string;
  agent: string;
  agentSource: string;
  error: string;
  info: string;
  runId: string;
  operationId: string;
  result: Record<string, unknown> | null;
  reviewRequired: boolean;
  reviewingFromEditor: boolean;
  selectionNeedsApply: boolean;
}

const normalizeCommitStatus = (value: unknown): CommitStatus => {
  const status =
    value && typeof value === 'object'
      ? (value as Partial<CommitStatus>)
      : ({} as Partial<CommitStatus>);
  return {
    ...status,
    staged: Array.isArray(status.staged) ? status.staged : [],
    unstaged: Array.isArray(status.unstaged) ? status.unstaged : [],
    untracked: Array.isArray(status.untracked) ? status.untracked : [],
    fingerprint:
      status.fingerprint && typeof status.fingerprint === 'object' ? status.fingerprint : {},
  };
};

const fingerprintKey = (value: Record<string, unknown>): string => {
  const operation = (value.operation || {}) as Record<string, unknown>;
  return JSON.stringify([
    value.checkout_id || '',
    value.head_state || '',
    value.head_oid || '',
    value.index_tree || '',
    operation.kind || '',
    operation.digest || '',
    operation.head_oids || [],
  ]);
};

const initialState = (): CommitUIState => ({
  sessionId: '',
  phase: 'closed',
  intent: '',
  status: null,
  selected: [],
  scopeSummary: '',
  message: '',
  generated: '',
  agent: '',
  agentSource: '',
  error: '',
  info: '',
  runId: '',
  operationId: '',
  result: null,
  reviewRequired: false,
  reviewingFromEditor: false,
  selectionNeedsApply: false,
});

export class CommitStore {
  readonly state = signal<CommitUIState>(initialState());
  private epoch = 0;
  private abort: AbortController | null = null;
  private reviewBaseline = new Set<string>();

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: {
      activeSession: ReadonlySignal<Session | null>;
      draftActive: ReadonlySignal<boolean>;
      modal: Signal<Modal>;
      busy: () => boolean;
      changed: () => void;
    },
  ) {}

  async open(intent = ''): Promise<boolean> {
    const session = this.options.activeSession.peek();
    if (!session || this.options.draftActive.peek()) {
      this.services.toast(
        'Start or select a persisted conversation before using /commit.',
        'error',
      );
      return false;
    }
    if (this.options.busy()) {
      this.services.toast(
        'Wait for the active response or operation before using /commit.',
        'error',
      );
      return false;
    }
    this.reset(false);
    const epoch = ++this.epoch;
    this.options.modal.value = 'commit';
    this.state.value = {
      ...initialState(),
      sessionId: session.id,
      intent,
      phase: 'loading',
      info: 'Inspecting the active checkout…',
    };
    const recovered = await this.recoverOperation(session.id, epoch);
    if (recovered) return true;
    try {
      const status = normalizeCommitStatus(await this.services.endpoints.commitStatus(session.id));
      if (!this.current(epoch, session.id)) return false;
      this.state.value = { ...this.state.peek(), status };
      const stagedCount = status.total_staged ?? status.staged.length;
      const workingCount =
        (status.total_unstaged ?? status.unstaged.length) +
        (status.total_untracked ?? status.untracked.length);
      if (stagedCount > 0 && workingCount > 0) {
        this.patch({ phase: 'choosing_scope', info: '' });
      } else if (stagedCount > 0) {
        void this.generate(epoch);
      } else if (intent.trim()) {
        void this.planScope(epoch);
      } else {
        void this.stage('all', [], epoch);
      }
      return true;
    } catch (error) {
      if (!this.current(epoch, session.id)) return false;
      this.patch({ phase: 'error', error: errorMessage(error), info: '' });
      return false;
    }
  }

  reset(close = true): void {
    this.epoch += 1;
    this.abort?.abort();
    const current = this.state.peek();
    if (current.runId && current.sessionId)
      void this.services.endpoints
        .cancelCommitRun(current.sessionId, current.runId)
        .catch(() => undefined);
    this.abort = null;
    this.reviewBaseline.clear();
    this.state.value = initialState();
    if (close && this.options.modal.peek() === 'commit') this.options.modal.value = '';
  }

  close(): void {
    const state = this.state.peek();
    const dirty = state.message !== state.generated && Boolean(state.message.trim());
    if (dirty && !window.confirm('Discard the edited commit message?')) return;
    this.reset();
  }

  async chooseEverything(): Promise<void> {
    await this.stage('all');
  }
  async chooseStaged(): Promise<void> {
    await this.generate();
  }
  async followRequest(): Promise<void> {
    await this.planScope();
  }
  setMessage(message: string): void {
    this.patch({ message });
  }
  setSelected(path: string, included: boolean): void {
    const selected = new Set(this.state.peek().selected);
    if (included) selected.add(path);
    else selected.delete(path);
    const selectionNeedsApply =
      selected.size !== this.reviewBaseline.size ||
      [...selected].some((selectedPath) => !this.reviewBaseline.has(selectedPath));
    this.patch({ selected: [...selected], selectionNeedsApply, error: '' });
  }
  allPaths(): string[] {
    const status = this.state.peek().status;
    return [
      ...new Set(
        [...(status?.staged || []), ...(status?.unstaged || []), ...(status?.untracked || [])].map(
          (entry) => entry.path,
        ),
      ),
    ].sort();
  }
  async reviewFiles(): Promise<void> {
    const current = this.state.peek();
    const mustReview = current.reviewRequired;
    let status = current.status;
    if (mustReview) {
      try {
        status = normalizeCommitStatus(
          await this.services.endpoints.commitStatus(current.sessionId),
        );
      } catch (error) {
        this.patch({ error: errorMessage(error) });
        return;
      }
    }
    const selected = (status?.staged || []).map((entry) => entry.path);
    this.reviewBaseline = new Set(selected);
    this.patch({
      phase: 'reviewing_scope',
      status,
      selected,
      reviewRequired: mustReview,
      reviewingFromEditor: true,
      selectionNeedsApply: false,
      scopeSummary: '',
      error: '',
    });
  }
  async backToMessage(): Promise<void> {
    const state = this.state.peek();
    if (state.phase !== 'reviewing_scope') return;
    if (!state.selectionNeedsApply && !state.reviewRequired) {
      if (!state.reviewingFromEditor) {
        this.patch({ error: 'Choose at least one file before returning to the message.' });
        return;
      }
      this.patch({ phase: 'editing', reviewingFromEditor: false, error: '', info: '' });
      return;
    }
    if (!state.selected.length) {
      this.patch({ error: 'Choose at least one file.' });
      return;
    }
    let mode: 'all' | 'exact_selection' = 'exact_selection';
    let paths = state.selected;
    if (!state.status?.selection_available) {
      if (state.selected.length !== this.allPaths().length) {
        this.patch({
          error:
            'Exact selection is unavailable. Select every file to continue, or press Escape to close.',
        });
        return;
      }
      mode = 'all';
      paths = [];
    }
    await this.stage(mode, paths, this.epoch, !state.reviewingFromEditor);
  }

  async regenerate(): Promise<void> {
    const state = this.state.peek();
    if (
      state.message !== state.generated &&
      state.message.trim() &&
      !window.confirm('Replace your edited message with a new draft?')
    )
      return;
    await this.generate();
  }

  async commit(): Promise<void> {
    const state = this.state.peek();
    const subject = state.message.split('\n')[0]?.trim();
    if (state.reviewRequired) {
      this.patch({ error: 'Refresh and review the staged files before committing.' });
      return;
    }
    if (!subject || !state.status) {
      this.patch({ error: 'A non-empty subject is required.' });
      return;
    }
    const epoch = this.epoch;
    this.patch({
      phase: 'committing',
      error: '',
      info: 'Running Git hooks and signing. This operation cannot be cancelled.',
    });
    try {
      const key = `commit_${crypto.randomUUID()}`;
      this.services.storage.setItem(
        this.operationStorageKey(state.sessionId),
        JSON.stringify({
          key,
          message: state.message,
          fingerprint: state.status.fingerprint,
        }),
      );
      const created = await this.services.endpoints.createCommitOperation(
        state.sessionId,
        { message: state.message, expected_fingerprint: state.status.fingerprint },
        key,
      );
      const operationId = String(created.operation_id || '');
      this.patch({ operationId });
      this.services.storage.setItem(
        this.operationStorageKey(state.sessionId),
        JSON.stringify({
          operationId,
          key,
          message: state.message,
          fingerprint: state.status.fingerprint,
        }),
      );
      const operation = await this.pollOperation(state.sessionId, operationId, epoch);
      if (!this.current(epoch, state.sessionId)) return;
      const status = String(operation.status || '');
      this.services.storage.removeItem(this.operationStorageKey(state.sessionId));
      if (status === 'succeeded') {
        this.patch({
          phase: 'success',
          result: (operation.result as Record<string, unknown>) || {},
          info: '',
          error: '',
        });
        this.options.changed();
      } else if (status === 'uncertain') {
        this.patch({
          phase: 'error',
          error: String(
            operation.error || 'Commit outcome is uncertain. Refresh status before retrying.',
          ),
          info: '',
          reviewRequired: true,
        });
      } else {
        const refreshed = normalizeCommitStatus(
          await this.services.endpoints.commitStatus(state.sessionId),
        );
        const unchanged =
          fingerprintKey(refreshed.fingerprint) === fingerprintKey(state.status.fingerprint) &&
          refreshed.staged.length > 0;
        this.patch({
          phase: 'editing',
          status: refreshed,
          error: `${String(operation.error || 'Git commit failed.')} ${
            unchanged
              ? 'The staged state is unchanged; the message was preserved for retry.'
              : 'Review the refreshed files before retrying.'
          }`,
          info: '',
          reviewRequired: !unchanged,
        });
      }
    } catch (error) {
      if (this.current(epoch, state.sessionId))
        this.patch({
          phase: 'editing',
          error: `${errorMessage(error)} Refresh and review repository status before retrying.`,
          info: '',
          reviewRequired: true,
        });
    }
  }

  private async planScope(epoch = this.epoch): Promise<void> {
    const state = this.state.peek();
    if (!state.status) return;
    if (!state.status.selection_available) {
      this.reviewBaseline.clear();
      this.patch({
        phase: 'reviewing_scope',
        selected: [],
        reviewingFromEditor: false,
        selectionNeedsApply: false,
        error:
          state.status.selection_unavailable_reason ||
          'Exact selection is unavailable. Select every file to continue, or press Escape to close.',
      });
      return;
    }
    this.patch({ phase: 'planning_scope', error: '', info: 'Planning a whole-file scope…' });
    try {
      const created = await this.services.endpoints.createCommitRun(state.sessionId, {
        kind: 'scope',
        intent: state.intent,
        expected_status_token: state.status.status_token,
        expected_fingerprint: state.status.fingerprint,
      });
      const runId = String(created.run_id || '');
      this.patch({
        runId,
        agent: String(created.agent_name || ''),
        agentSource: String(created.agent_source || ''),
      });
      const completed = await this.pollRun(state.sessionId, runId, epoch);
      if (!this.current(epoch, state.sessionId)) return;
      if (String(completed.status) !== 'complete')
        throw new Error(String(completed.error || 'Scope planning failed.'));
      const proposal = (completed.proposal || {}) as Record<string, unknown>;
      const mode = String(proposal.mode || '');
      const summary = String(proposal.summary || '');
      if (mode === 'all') {
        this.patch({ scopeSummary: summary });
        await this.stage('all', [], epoch);
        return;
      }
      const selected =
        mode === 'selected' && Array.isArray(proposal.include_paths)
          ? proposal.include_paths.map(String)
          : [];
      this.reviewBaseline.clear();
      this.patch({
        phase: 'reviewing_scope',
        selected,
        reviewingFromEditor: false,
        selectionNeedsApply: selected.length > 0,
        scopeSummary: summary,
        info: '',
        error: mode === 'needs_manual' ? 'This request needs manual whole-file selection.' : '',
      });
    } catch (error) {
      this.reviewBaseline.clear();
      if (this.current(epoch, state.sessionId))
        this.patch({
          phase: 'reviewing_scope',
          selected: [],
          reviewingFromEditor: false,
          selectionNeedsApply: false,
          error: `Scope planning failed: ${errorMessage(error)}`,
          info: '',
        });
    }
  }

  private async stage(
    mode: 'all' | 'exact_selection',
    paths: string[] = [],
    epoch = this.epoch,
    generateMessage = true,
  ): Promise<void> {
    const state = this.state.peek();
    if (!state.status) return;
    if (mode === 'exact_selection' && !paths.length) {
      this.patch({ error: 'Choose at least one file.' });
      return;
    }
    this.patch({
      phase: 'staging',
      error: '',
      info: mode === 'all' ? 'Staging all changes…' : 'Applying exact whole-file selection…',
    });
    try {
      const status = normalizeCommitStatus(
        await this.services.endpoints.commitStage(state.sessionId, {
          mode,
          paths,
          expected_status_token: state.status.status_token,
          expected_fingerprint: state.status.fingerprint,
        }),
      );
      if (!this.current(epoch, state.sessionId)) return;
      const stagedPaths = status.staged.map((entry) => entry.path);
      this.reviewBaseline = new Set(stagedPaths);
      this.patch({
        status,
        selected: stagedPaths,
        reviewRequired: false,
        selectionNeedsApply: false,
      });
      this.options.changed();
      if (generateMessage) {
        await this.generate(epoch);
      } else {
        this.patch({
          phase: 'editing',
          reviewingFromEditor: false,
          info: 'File selection applied. The existing message was preserved.',
          error: '',
        });
      }
    } catch (error) {
      if (this.current(epoch, state.sessionId))
        this.patch({
          phase: 'error',
          error: `${errorMessage(error)} Refresh and review repository status before continuing.`,
          info: '',
          reviewRequired: true,
        });
    }
  }

  private async generate(epoch = this.epoch): Promise<void> {
    const state = this.state.peek();
    if (!state.status) return;
    this.patch({
      phase: 'drafting_message',
      error: '',
      info: 'Drafting a staged-only commit message…',
    });
    try {
      const created = await this.services.endpoints.createCommitRun(state.sessionId, {
        kind: 'message',
        intent: state.intent,
        scope_summary: state.scopeSummary,
        expected_fingerprint: state.status.fingerprint,
      });
      const runId = String(created.run_id || '');
      this.patch({
        runId,
        agent: String(created.agent_name || ''),
        agentSource: String(created.agent_source || ''),
      });
      const completed = await this.pollRun(state.sessionId, runId, epoch);
      if (!this.current(epoch, state.sessionId)) return;
      if (String(completed.status) !== 'complete')
        throw new Error(String(completed.error || 'Message drafting failed.'));
      const message = String(completed.message || '');
      if (!message.trim()) throw new Error('Message agent returned an empty draft.');
      this.patch({
        phase: 'editing',
        message,
        generated: message,
        error: '',
        info: '',
        runId: '',
        reviewRequired: false,
      });
    } catch (error) {
      if (this.current(epoch, state.sessionId))
        this.patch({
          phase: 'editing',
          error: `Message generation failed; enter one manually. ${errorMessage(error)}`,
          info: '',
        });
    }
  }

  private operationStorageKey(sessionId: string): string {
    return `term-llm.commit-operation.${sessionId}`;
  }
  private async recoverOperation(sessionId: string, epoch: number): Promise<boolean> {
    const raw = this.services.storage.getItem(this.operationStorageKey(sessionId));
    if (!raw) return false;
    try {
      const saved = JSON.parse(raw) as {
        operationId?: string;
        key?: string;
        message?: string;
        fingerprint?: Record<string, unknown>;
      };
      let operationId = String(saved.operationId || '');
      if (!operationId) {
        if (!saved.key || !saved.message || !saved.fingerprint)
          throw new Error('saved operation is incomplete');
        const replay = await this.services.endpoints.createCommitOperation(
          sessionId,
          { message: saved.message, expected_fingerprint: saved.fingerprint },
          saved.key,
        );
        operationId = String(replay.operation_id || '');
        if (!operationId) throw new Error('server did not return the saved operation');
        this.services.storage.setItem(
          this.operationStorageKey(sessionId),
          JSON.stringify({ ...saved, operationId }),
        );
      }
      this.patch({
        phase: 'committing',
        operationId,
        message: String(saved.message || ''),
        info: 'Reconnecting to the in-flight Git commit…',
      });
      const operation = await this.pollOperation(sessionId, operationId, epoch);
      if (!this.current(epoch, sessionId)) return true;
      this.services.storage.removeItem(this.operationStorageKey(sessionId));
      if (String(operation.status) === 'succeeded') {
        this.patch({
          phase: 'success',
          result: (operation.result as Record<string, unknown>) || {},
          info: '',
          error: '',
        });
        this.options.changed();
        return true;
      }
      const status = normalizeCommitStatus(await this.services.endpoints.commitStatus(sessionId));
      const uncertain = String(operation.status) === 'uncertain';
      const unchanged =
        !uncertain &&
        !!saved.fingerprint &&
        fingerprintKey(status.fingerprint) === fingerprintKey(saved.fingerprint) &&
        status.staged.length > 0;
      this.patch({
        phase: uncertain ? 'error' : 'editing',
        status,
        reviewRequired: !unchanged,
        info: '',
        error: `${String(
          operation.error || (uncertain ? 'Commit outcome is uncertain.' : 'Git commit failed.'),
        )} ${
          unchanged
            ? 'The staged state is unchanged; the message was preserved for retry.'
            : 'Review repository status before retrying.'
        }`,
      });
      return true;
    } catch (error) {
      this.patch({
        phase: 'error',
        reviewRequired: true,
        info: '',
        error: `Could not reconnect to the saved commit operation: ${errorMessage(error)}`,
      });
      return true;
    }
  }

  private async pollRun(
    sessionId: string,
    runId: string,
    epoch: number,
  ): Promise<Record<string, unknown>> {
    for (;;) {
      if (!this.current(epoch, sessionId)) throw new DOMException('Cancelled', 'AbortError');
      const value = await this.services.endpoints.commitRun(sessionId, runId);
      if (!['running', 'cancelling'].includes(String(value.status))) return value;
      await delay(300);
    }
  }
  private async pollOperation(
    sessionId: string,
    operationId: string,
    epoch: number,
  ): Promise<Record<string, unknown>> {
    for (;;) {
      const value = await this.services.endpoints.commitOperation(sessionId, operationId);
      if (!['queued', 'running'].includes(String(value.status))) return value;
      if (!this.current(epoch, sessionId)) throw new DOMException('Detached', 'AbortError');
      await delay(400);
    }
  }
  private current(epoch: number, sessionId: string): boolean {
    return (
      epoch === this.epoch &&
      this.state.peek().sessionId === sessionId &&
      this.options.activeSession.peek()?.id === sessionId
    );
  }
  private patch(patch: Partial<CommitUIState>): void {
    this.state.value = { ...this.state.peek(), ...patch };
  }
}
const delay = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

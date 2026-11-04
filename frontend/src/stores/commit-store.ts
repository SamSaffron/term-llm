import { signal } from '@preact/signals';
import type { AppStoreServices } from './app-store-services';
import {
  commitPaths,
  initialState,
  type CommitStoreOptions,
  type CommitUIState,
  type CommitWorkflowHost,
  type PublishForm,
} from './commit-store-types';
import type { CommitWorkflow } from './commit-workflow';

export type {
  CommitChange,
  CommitPhase,
  CommitStatus,
  CommitUIState,
  PublishForm,
} from './commit-store-types';

/** Mirrors the modal-chunk fallback: a failed workflow chunk is dismissible and retryable. */
const LOAD_ERROR = 'Could not load commit controls. Reload the page to retry.';

/**
 * Eagerly available facade for the Git commit workflow.
 *
 * `AppStore` constructs this store at startup, so only the signal and the
 * synchronous state edits live here. The workflow itself (status, staging,
 * message runs, commits and publishing) is imported on the first action, which
 * keeps it out of the initial static graph. Both halves share one signal, one
 * review baseline and one epoch so reset/session switches still cancel work.
 */
export class CommitStore {
  readonly state = signal<CommitUIState>(initialState());
  private readonly host: CommitWorkflowHost;
  private readonly reviewBaseline = new Set<string>();
  private workflow: CommitWorkflow | null = null;
  private loading: Promise<CommitWorkflow | null> | null = null;
  private epoch = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: CommitStoreOptions,
  ) {
    this.host = {
      services: this.services,
      state: this.state,
      options: this.options,
      patch: (patch) => this.patch(patch),
      setReviewBaseline: (paths) => {
        this.reviewBaseline.clear();
        for (const path of paths) this.reviewBaseline.add(path);
      },
      clearReviewBaseline: () => this.reviewBaseline.clear(),
    };
  }

  async open(intent = ''): Promise<boolean> {
    if (this.services.isDisposed) return false;
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
    const epoch = this.epoch;
    const owner = session.id;
    const draft = this.options.draftActive.peek();
    this.options.modal.value = 'commit';
    this.state.value = {
      ...initialState(),
      sessionId: session.id,
      intent,
      phase: 'loading',
      info: 'Inspecting the active checkout…',
    };
    const loaded = this.workflow;
    if (loaded) return loaded.open(intent);
    const workflow = await this.loadWorkflow();
    if (!workflow) {
      if (this.services.isDisposed || epoch !== this.epoch) return false;
      if (!this.ownerUnchanged(owner, draft)) {
        this.reset();
        return false;
      }
      this.patch({ phase: 'error', error: LOAD_ERROR, info: '' });
      return false;
    }
    // Closing, resetting or switching conversations while the chunk loads must
    // not open (or reopen) the workflow for a store that has moved on.
    if (this.services.isDisposed || epoch !== this.epoch) return false;
    if (!this.ownerUnchanged(owner, draft)) {
      this.reset();
      return false;
    }
    return workflow.open(intent);
  }

  reset(close = true): void {
    this.epoch += 1;
    this.workflow?.cancel();
    this.resetState(close);
  }

  close(): void {
    const state = this.state.peek();
    if (state.phase === 'committing' || state.publishBusy) return;
    const dirty =
      state.phase !== 'success' &&
      state.message !== state.generated &&
      Boolean(state.message.trim());
    if (dirty && !window.confirm('Discard the edited commit message?')) return;
    this.reset();
  }

  chooseEverything(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.chooseEverything());
  }
  chooseStaged(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.chooseStaged());
  }
  followRequest(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.followRequest());
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
    return commitPaths(this.state.peek().status);
  }
  reviewFiles(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.reviewFiles());
  }
  backToMessage(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.backToMessage());
  }
  regenerate(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.regenerate());
  }
  commit(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.commit());
  }
  preparePublish(kind: 'push' | 'pr'): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.preparePublish(kind));
  }
  editPublish(
    patch: Partial<Pick<PublishForm, 'branch' | 'base' | 'title' | 'body' | 'draft'>>,
  ): void {
    const state = this.state.peek();
    if (state.publishForm && !state.publishBusy)
      this.patch({ publishForm: { ...state.publishForm, ...patch } });
  }
  cancelPublish(): void {
    if (!this.state.peek().publishBusy) this.patch({ publishForm: null, error: '', info: '' });
  }
  publish(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.publish());
  }
  reconnectPublish(): Promise<void> {
    return this.runWithWorkflow((workflow) => workflow.reconnectPublish());
  }

  private runWithWorkflow(action: (workflow: CommitWorkflow) => Promise<void>): Promise<void> {
    if (this.services.isDisposed) return Promise.resolve();
    const loaded = this.workflow;
    if (loaded) return action(loaded);
    const epoch = this.epoch;
    const owner = this.options.activeSession.peek()?.id || '';
    const draft = this.options.draftActive.peek();
    return this.loadWorkflow().then((workflow) => {
      if (!workflow) {
        if (epoch === this.epoch && this.ownerUnchanged(owner, draft) && !this.services.isDisposed)
          this.patch({ error: LOAD_ERROR });
        return;
      }
      if (epoch !== this.epoch || !this.ownerUnchanged(owner, draft) || this.services.isDisposed)
        return;
      return action(workflow);
    });
  }

  /** The queued action must still belong to the conversation that requested it. */
  private ownerUnchanged(owner: string, draft: boolean): boolean {
    return (
      (this.options.activeSession.peek()?.id || '') === owner &&
      this.options.draftActive.peek() === draft
    );
  }

  private loadWorkflow(): Promise<CommitWorkflow | null> {
    if (this.workflow) return Promise.resolve(this.workflow);
    if (!this.loading) {
      this.loading = import('./commit-workflow')
        .then(({ CommitWorkflow }) => {
          const workflow = new CommitWorkflow(this.host);
          this.workflow = workflow;
          this.loading = null;
          return workflow;
        })
        .catch((error: unknown) => {
          this.loading = null;
          console.error('Failed to load the commit workflow', error);
          return null;
        });
    }
    return this.loading;
  }

  private patch(patch: Partial<CommitUIState>): void {
    this.state.value = { ...this.state.peek(), ...patch };
  }

  private resetState(close: boolean): void {
    this.reviewBaseline.clear();
    this.state.value = initialState();
    if (close && this.options.modal.peek() === 'commit') this.options.modal.value = '';
  }
}

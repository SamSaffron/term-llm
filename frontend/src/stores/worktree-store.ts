import { computed, signal, type ReadonlySignal, type Signal } from '@preact/signals';
import { APIError } from '../api/client';
import type { Project, Session } from '../domain/types';
import { readDrafts, saveDraft } from '../platform/storage';
import type { Modal, SendOptions } from './store-types';
import { listFrom, recordValue, worktreeErrorMessage } from './store-utils';
import type { AppStoreServices } from './app-store-services';

export interface WorktreeStoreOptions {
  projectsEnabled: ReadonlySignal<boolean>;
  worktreesEnabled: ReadonlySignal<boolean>;
  activeProjectId: ReadonlySignal<string>;
  activeSession: ReadonlySignal<Session | null>;
  draftActive: ReadonlySignal<boolean>;
  prompt: ReadonlySignal<string>;
  projects: ReadonlySignal<Project[]>;
  modal: Signal<Modal>;
  selectedDraftWorktree: Signal<string>;
  draftStorageId: () => string;
  patchSession: (id: string, patch: Partial<Session>) => void;
  send: (options?: SendOptions) => Promise<void>;
}

/** Owns worktree discovery and worktree commands for the active project. */
export class WorktreeStore {
  readonly worktrees = signal<Record<string, unknown>[]>([]);
  readonly error = signal('');
  readonly currentDir: ReadonlySignal<string>;

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: WorktreeStoreOptions,
  ) {
    // A root checkout is represented by an empty path. Select by conversation
    // mode rather than truthiness so root can never fall through to draft-only
    // state left over from another composer.
    this.currentDir = computed(() =>
      this.options.draftActive.value
        ? this.options.selectedDraftWorktree.value
        : this.options.activeSession.value?.worktreeDir || '',
    );
  }

  available(): boolean {
    if (!this.options.projectsEnabled.value) return this.options.worktreesEnabled.value;
    const projectId =
      this.options.activeSession.value?.projectId || this.options.activeProjectId.value;
    const project = this.options.projects.value.find((entry) => entry.id === projectId);
    return Boolean(project?.git && project.available !== false && !project.archived);
  }

  private projectId(): string {
    return this.options.activeSession.value?.projectId || this.options.activeProjectId.value;
  }

  async load(): Promise<void> {
    const projectId = this.projectId();
    this.error.value = '';
    if (this.options.projectsEnabled.value && !projectId) {
      this.worktrees.value = [];
      this.error.value = 'Choose a project before selecting a worktree.';
      return;
    }
    try {
      const data = this.options.projectsEnabled.value
        ? await this.services.endpoints.projectWorktrees(projectId)
        : await this.services.endpoints.legacyWorktrees();
      this.worktrees.value = listFrom(data, 'worktrees', 'items');
    } catch (error) {
      this.worktrees.value = [];
      this.error.value = worktreeErrorMessage(error);
    }
  }

  async create(name: string, clean = false): Promise<void> {
    const projectId = this.projectId();
    this.error.value = '';
    try {
      if (this.options.projectsEnabled.value)
        await this.services.endpoints.createProjectWorktree(projectId, { name, clean });
      else await this.services.api.post('/v1/worktrees', { name, clean });
      await this.load();
    } catch (error) {
      throw new Error(worktreeErrorMessage(error), { cause: error });
    }
  }

  private setDraftSelection(dir: string, close: boolean): void {
    this.options.selectedDraftWorktree.value = dir;
    const id = this.options.draftStorageId();
    const draft = readDrafts(this.services.storage, this.services.keys.draftMessages).find(
      (entry) => entry.sessionId === id,
    );
    saveDraft(this.services.storage, this.services.keys.draftMessages, {
      ...(draft || {
        sessionId: id,
        content: this.options.prompt.value,
        updated: Date.now(),
      }),
      worktreeDir: dir,
      projectId: this.options.activeProjectId.value,
    });
    if (close) this.options.modal.value = '';
  }

  chooseDraft(dir: string): void {
    if (!this.options.draftActive.value) return;
    this.setDraftSelection(dir, true);
  }

  async switchTo(dir: string): Promise<void> {
    const active = this.options.activeSession.value;
    if (!active) throw new Error('Choose a conversation before switching worktrees.');
    const data = await this.services.endpoints.switchWorktree(this.projectId(), dir, active.id);
    this.options.patchSession(active.id, {
      worktreeDir: String(data.worktree_dir || ''),
      workingDir: String(data.cwd || ''),
    });
    await this.load();
    this.services.toast(
      dir ? 'Conversation switched to worktree.' : 'Conversation switched to root.',
      'success',
    );
  }

  private async finishMutation(
    data: Record<string, unknown>,
    action: 'merged' | 'promoted',
    sourceDir: string,
  ): Promise<Record<string, unknown>> {
    await this.load();
    const cleanup = recordValue(data.cleanup);
    if (
      cleanup?.removed === true &&
      this.options.draftActive.value &&
      this.options.selectedDraftWorktree.value === sourceDir
    )
      this.setDraftSelection('', false);
    const result = recordValue(data.result);
    const movedSession = recordValue(data.session);
    const warning = String(data.warning || '');
    const inUse = Array.isArray(cleanup?.in_use) ? cleanup.in_use.length : 0;
    const active = this.options.activeSession.value;
    if (active && movedSession && String(movedSession.id || '') === active.id)
      this.options.patchSession(active.id, {
        worktreeDir: String(movedSession.worktree_dir || ''),
        workingDir: String(movedSession.cwd || result?.root_dir || active.workingDir || ''),
      });
    if (warning) this.services.toast(warning, 'info');
    else if (cleanup?.removed === true) {
      this.services.toast(`Worktree ${action} and old checkout removed.`, 'success');
    } else if (inUse > 0)
      this.services.toast(
        `Worktree ${action}; old checkout kept because ${inUse} other ${inUse === 1 ? 'conversation uses' : 'conversations use'} it.`,
        'info',
      );
    else this.services.toast(`Worktree ${action}.`, 'success');
    return data;
  }

  async merge(dir: string, force = false): Promise<Record<string, unknown>> {
    const sessionId = this.options.activeSession.value?.id || '';
    const data = await this.services.endpoints.mergeWorktree(
      this.projectId(),
      dir,
      sessionId,
      force,
    );
    return this.finishMutation(data, 'merged', dir);
  }

  private applyMovedSession(data: Record<string, unknown>): void {
    const active = this.options.activeSession.value;
    const movedSession = recordValue(data.session);
    const result = recordValue(data.result);
    if (!active || !movedSession || String(movedSession.id || '') !== active.id) return;
    this.options.patchSession(active.id, {
      worktreeDir: String(movedSession.worktree_dir || ''),
      workingDir: String(movedSession.cwd || result?.root_dir || active.workingDir || ''),
    });
  }

  private movedSessionFromError(error: unknown): void {
    if (!(error instanceof APIError) || !error.body) return;
    try {
      this.applyMovedSession(JSON.parse(error.body) as Record<string, unknown>);
    } catch {
      // Preserve the original API error when the response is not JSON.
    }
  }

  async recover(dir: string): Promise<Record<string, unknown>> {
    const active = this.options.activeSession.value;
    if (!active) throw new Error('Choose a conversation before starting assisted recovery.');
    let data: Record<string, unknown>;
    try {
      data = await this.services.endpoints.assistedMergeWorktree(this.projectId(), dir, active.id);
    } catch (error) {
      // The server follows the TUI ordering and moves the conversation to root
      // before applying the snapshot. Keep the web projection in sync even if
      // the apply then fails (for example, because root became dirty).
      this.movedSessionFromError(error);
      throw error;
    }
    // Update workspace ownership before send(). A first response otherwise
    // includes the stale worktree_dir and re-binds the run to the source tree.
    this.applyMovedSession(data);
    await this.load();
    this.options.modal.value = '';
    const notice = String(data.notice || data.message || 'Assisted recovery started.');
    this.services.toast(notice, data.prompt ? 'success' : 'info');
    const prompt = String(data.prompt || '').trim();
    if (prompt) {
      try {
        await this.options.send({ inputText: prompt, preserveComposer: true });
      } catch (error) {
        this.services.toast(
          `Could not send the recovery prompt: ${worktreeErrorMessage(error)}`,
          'error',
        );
        throw error;
      }
    }
    return data;
  }

  async promote(dir: string, branch: string): Promise<Record<string, unknown>> {
    const sessionId = this.options.activeSession.value?.id || '';
    const data = await this.services.endpoints.promoteWorktree(
      this.projectId(),
      dir,
      branch,
      sessionId,
    );
    return this.finishMutation(data, 'promoted', dir);
  }

  async remove(dir: string, force = false): Promise<Record<string, unknown>> {
    const result = await this.services.endpoints.removeWorktree(this.projectId(), dir, force);
    if (
      this.options.draftActive.value &&
      this.options.selectedDraftWorktree.value &&
      this.options.selectedDraftWorktree.value === dir
    )
      this.setDraftSelection('', false);
    await this.load();
    return result || {};
  }
}

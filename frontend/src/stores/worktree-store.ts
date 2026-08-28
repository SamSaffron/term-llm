import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { Project, Session } from '../domain/types';
import { readDrafts, saveDraft } from '../platform/storage';
import type { Modal } from './store-types';
import { listFrom, worktreeErrorMessage } from './store-utils';
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
}

/** Owns worktree discovery and worktree commands for the active project. */
export class WorktreeStore {
  readonly worktrees = signal<Record<string, unknown>[]>([]);
  readonly error = signal('');

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: WorktreeStoreOptions,
  ) {}

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

  async create(name: string): Promise<void> {
    const projectId = this.projectId();
    try {
      if (this.options.projectsEnabled.value)
        await this.services.endpoints.createProjectWorktree(projectId, { name });
      else await this.services.api.post('/v1/worktrees', { name });
      await this.load();
    } catch (error) {
      this.error.value = worktreeErrorMessage(error);
    }
  }

  chooseDraft(dir: string): void {
    if (!this.options.draftActive.value) return;
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
    this.options.modal.value = '';
  }

  async diff(dir: string): Promise<string> {
    const data = await this.services.endpoints.worktreeDiff(this.projectId(), dir);
    return String(data.diff || data.patch || '');
  }

  async merge(dir: string): Promise<void> {
    await this.services.endpoints.mergeWorktree(this.projectId(), dir);
    await this.load();
    this.services.toast('Worktree merged.', 'success');
  }

  async promote(dir: string, branch: string): Promise<void> {
    await this.services.endpoints.promoteWorktree(this.projectId(), dir, branch);
    await this.load();
    this.services.toast('Worktree promoted.', 'success');
  }

  async remove(dir: string, force = false): Promise<Record<string, unknown>> {
    const result = await this.services.endpoints.removeWorktree(this.projectId(), dir, force);
    await this.load();
    return result || {};
  }
}

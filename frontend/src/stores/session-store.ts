import { computed, signal, type Signal } from '@preact/signals';
import { APIError } from '../api/client';
import type { Project, Session } from '../domain/types';
import type { Modal, HubAgent } from './store-types';
import type { AppStoreServices } from './app-store-services';
import {
  array,
  compareSessionsByActivity,
  listFrom,
  recordValue,
  sessionFrom as sanitizeSessionFrom,
} from './store-utils';

export type SidebarView = 'recent' | 'projects';

function semanticEqual(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true;
  if (Array.isArray(left) || Array.isArray(right))
    return (
      Array.isArray(left) &&
      Array.isArray(right) &&
      left.length === right.length &&
      left.every((value, index) => semanticEqual(value, right[index]))
    );
  if (!left || !right || typeof left !== 'object' || typeof right !== 'object') return false;
  const leftPrototype = Object.getPrototypeOf(left);
  if (leftPrototype !== Object.getPrototypeOf(right)) return false;
  if (leftPrototype !== Object.prototype && leftPrototype !== null) return false;
  const leftRecord = left as Record<string, unknown>;
  const rightRecord = right as Record<string, unknown>;
  const leftKeys = Object.keys(leftRecord).filter((key) => leftRecord[key] !== undefined);
  const rightKeys = Object.keys(rightRecord).filter((key) => rightRecord[key] !== undefined);
  return (
    leftKeys.length === rightKeys.length &&
    leftKeys.every(
      (key) => Object.hasOwn(rightRecord, key) && semanticEqual(leftRecord[key], rightRecord[key]),
    )
  );
}

function sameIdentityList<T>(left: T[], right: T[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

const TRANSCRIPT_ONLY_SESSION_FIELDS = [
  'usage',
  'goal',
  'mcpServers',
  'mcpEnabled',
  'transcriptRev',
  'messageBodiesRev',
  'lastResponseId',
  'activeResponseId',
  'activeModel',
  'activeProvider',
  'activeEffort',
  'activeReasoningMode',
  'workingDir',
  'worktreeDir',
  'fileChangeSummary',
] as const satisfies readonly (keyof Session)[];

function sidebarSessionProjection(session: Session): Session {
  const messageCount = Number.isFinite(session.messageCount)
    ? Math.max(0, session.messageCount || 0)
    : session.messages.filter((message) => message.role === 'user' || message.role === 'assistant')
        .length;
  const projection = { ...session, messages: [], messageCount };
  for (const field of TRANSCRIPT_ONLY_SESSION_FIELDS) delete projection[field];
  return projection;
}

export interface SessionStoreHost {
  hasRun: (sessionId: string) => boolean;
  modal: Signal<Modal>;
  publishSessionChange: () => void;
  refreshSidebar: () => Promise<void>;
  newChat: (replace?: boolean, projectId?: string) => void;
}

/** Owns session/project catalog state, sidebar loading, search, and catalog mutations. */
export class SessionStore {
  readonly sessions = signal<Session[]>([]);
  private readonly sidebarSessionCache = new Map<string, Session>();
  private sidebarSessionList: Session[] = [];
  readonly sidebarSessions = computed(() => {
    const sessions = this.sessions.value;
    const present = new Set(sessions.map((session) => session.id));
    for (const id of this.sidebarSessionCache.keys()) {
      if (!present.has(id)) this.sidebarSessionCache.delete(id);
    }
    const projected = sessions.map((session) => {
      const candidate = sidebarSessionProjection(session);
      const previous = this.sidebarSessionCache.get(session.id);
      if (previous && semanticEqual(previous, candidate)) return previous;
      this.sidebarSessionCache.set(session.id, candidate);
      return candidate;
    });
    if (sameIdentityList(this.sidebarSessionList, projected)) return this.sidebarSessionList;
    this.sidebarSessionList = projected;
    return projected;
  });
  readonly recentSessions = signal<Session[]>([]);
  readonly recentCursor = signal('');
  readonly sidebarView: Signal<SidebarView>;
  readonly projects = signal<Project[]>([]);
  readonly noProjectCursor = signal('');
  readonly projectsEnabled = signal(false);
  readonly worktreesEnabled = signal(false);
  readonly activeProjectId = signal('');
  readonly activeSessionId = signal('');
  readonly draftActive = signal(true);
  readonly sidebarCollapsed: Signal<boolean>;
  readonly sidebarOpen = signal(false);
  readonly sidebarSearch = signal('');
  readonly searchResults = signal<Session[] | null>(null);
  readonly searchLoading = signal(false);
  readonly searchError = signal('');
  readonly showHidden: Signal<boolean>;
  readonly hubAgents = signal<HubAgent[]>([]);
  readonly renameTarget = signal<Session | null>(null);
  readonly projectTarget = signal<Session | null>(null);
  readonly activeSession = computed(
    () => this.sessions.value.find((session) => session.id === this.activeSessionId.value) || null,
  );

  private searchAbort: AbortController | null = null;
  private searchTimer = 0;
  private lastSidebarRefreshAt = 0;
  private sidebarGeneration = 0;
  private lastAppliedSidebarGeneration = 0;
  private sidebarRefreshPromise: Promise<void> | null = null;
  private recentTailLoaded = false;
  private hubAgentLastFetch = 0;
  private hubAgentFetch: Promise<void> | null = null;

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: SessionStoreHost,
  ) {
    this.sidebarCollapsed = signal(
      services.storage.getItem(services.keys.sidebarCollapsed) === '1',
    );
    this.sidebarView = signal(
      services.storage.getItem(services.keys.sidebarView) === 'projects' ? 'projects' : 'recent',
    );
    this.showHidden = signal(services.storage.getItem(services.keys.showHiddenSessions) === '1');
  }

  get sidebarRefreshedAt(): number {
    return this.lastSidebarRefreshAt;
  }

  patch(id: string, patch: Partial<Session>): void {
    this.sessions.value = this.sessions
      .peek()
      .map((session) => (session.id === id ? { ...session, ...patch } : session));
  }

  update(id: string, updater: (session: Session) => Session): void {
    this.sessions.value = this.sessions
      .peek()
      .map((session) => (session.id === id ? updater(session) : session));
  }

  replace(sessions: Session[]): void {
    if (!sameIdentityList(this.sessions.peek(), sessions)) this.sessions.value = sessions;
  }

  rekeyRecent(oldID: string, replacement: Session): void {
    if (!this.recentSessions.peek().some((session) => session.id === oldID)) return;
    this.recentSessions.value = this.recentSessions
      .peek()
      .map((session) => (session.id === oldID ? replacement : session))
      .filter(
        (session, index, entries) =>
          entries.findIndex((candidate) => candidate.id === session.id) === index,
      )
      .sort(compareSessionsByActivity);
  }

  prepend(session: Session): void {
    this.sessions.value = [session, ...this.sessions.peek()];
    if (this.projectsEnabled.peek()) {
      this.recentSessions.value = [
        session,
        ...this.recentSessions.peek().filter((entry) => entry.id !== session.id),
      ].sort(compareSessionsByActivity);
    }
  }

  find(id: string): Session | undefined {
    return this.sessions.peek().find((session) => session.id === id);
  }

  setSidebarView(view: SidebarView): void {
    this.sidebarView.value = view;
    this.services.storage.setItem(this.services.keys.sidebarView, view);
  }

  activate(session: Session): void {
    this.sidebarOpen.value = false;
    this.activeSessionId.value = session.id;
    this.activeProjectId.value = session.projectId || '';
    this.draftActive.value = false;
  }

  activateDraft(projectId: string): void {
    this.sidebarOpen.value = false;
    this.activeSessionId.value = '';
    this.activeProjectId.value = projectId;
    this.draftActive.value = true;
  }

  applyCapabilities(projectsEnabled: boolean, worktreesEnabled: boolean): void {
    this.projectsEnabled.value = projectsEnabled;
    this.worktreesEnabled.value = worktreesEnabled;
  }

  sessionFrom(value: Record<string, unknown>): Session {
    return sanitizeSessionFrom(this.services.config, value);
  }

  mergeSession(
    existing: Session | undefined,
    incoming: Session,
    replaceMessages = false,
    preserveLiveState = false,
  ): Session {
    if (!existing) return incoming;
    const storeReplaced = Boolean(
      existing.attentionStoreInstanceId &&
      incoming.attentionStoreInstanceId &&
      existing.attentionStoreInstanceId !== incoming.attentionStoreInstanceId,
    );
    const existingSeq = existing.attentionSeq || 0;
    const incomingSeq = incoming.attentionSeq ?? (storeReplaced ? 0 : existingSeq);
    const markerSource =
      storeReplaced || (incoming.attentionSeq !== undefined && incomingSeq >= existingSeq)
        ? incoming
        : existing;
    const attentionSeq = storeReplaced ? incomingSeq : Math.max(existingSeq, incomingSeq);
    const seenThroughSeq = storeReplaced
      ? incoming.seenThroughSeq || 0
      : Math.max(existing.seenThroughSeq || 0, incoming.seenThroughSeq || 0);
    const hasAttention = Boolean(
      existing.attentionStoreInstanceId ||
      incoming.attentionStoreInstanceId ||
      existing.attentionSeq !== undefined ||
      incoming.attentionSeq !== undefined,
    );
    return {
      ...existing,
      ...incoming,
      ...(hasAttention
        ? {
            attentionStoreInstanceId:
              markerSource.attentionStoreInstanceId || existing.attentionStoreInstanceId,
            attentionSeq,
            attentionResponseId: storeReplaced
              ? incoming.attentionResponseId
              : (markerSource.attentionResponseId ?? existing.attentionResponseId),
            attentionFinalRev: storeReplaced
              ? incoming.attentionFinalRev
              : (markerSource.attentionFinalRev ?? existing.attentionFinalRev),
            attentionOutcome: storeReplaced
              ? incoming.attentionOutcome
              : (markerSource.attentionOutcome ?? existing.attentionOutcome),
            attentionTerminalAt: storeReplaced
              ? incoming.attentionTerminalAt
              : (markerSource.attentionTerminalAt ?? existing.attentionTerminalAt),
            seenThroughSeq,
            attentionUnseen: attentionSeq > seenThroughSeq,
          }
        : {}),
      messages: replaceMessages || incoming.messages.length ? incoming.messages : existing.messages,
      lastResponseId: incoming.lastResponseId || existing.lastResponseId,
      activeResponseId: preserveLiveState
        ? incoming.activeResponseId || existing.activeResponseId
        : incoming.activeResponseId,
      activeRun: preserveLiveState
        ? (incoming.activeRun ?? existing.activeRun)
        : incoming.activeRun,
      usage: incoming.usage || existing.usage,
      goal: incoming.goal ?? existing.goal,
      transcriptRev: incoming.transcriptRev ?? existing.transcriptRev,
      messageBodiesRev:
        replaceMessages || incoming.messages.length
          ? (incoming.messageBodiesRev ?? existing.messageBodiesRev)
          : existing.messageBodiesRev,
      fileChangeSummary: incoming.fileChangeSummary || existing.fileChangeSummary,
    };
  }

  /**
   * Rows the client owns survive a server rebuild: live runs, unsent drafts,
   * and the open conversation. Deliberately scoped to applySidebar's
   * rebuild-from-server loop only — do not centralize into replace(), which
   * must stay lossy so RunEngine.rekeySession can drop the pre-rekey draft_
   * row while activeSessionId still points at it (it re-points afterward).
   */
  private retainedLocally(id: string): boolean {
    return this.host.hasRun(id) || id.startsWith('draft_') || id === this.activeSessionId.peek();
  }

  applySidebar(data: Record<string, unknown>): void {
    const direct = listFrom(data, 'data', 'sessions', 'items').map((entry) =>
      this.sessionFrom(entry),
    );
    const recent = listFrom(data, 'recent_sessions').map((entry) => this.sessionFrom(entry));
    const recentCursor = String(data.recent_next_cursor || '');
    const previousRecent = this.recentSessions.peek();
    const previousRecentIDs = new Set(previousRecent.map((session) => session.id));
    const groups = listFrom(data, 'groups');
    const projects: Project[] = [];
    const ungrouped: Session[] = [...direct];
    const incompleteProjectIDs = new Set<string>();
    let noProjectPageIncomplete = groups.length === 0 && Boolean(data.next_cursor);
    // Flat listings (projects disabled) carry the cursor at the top level;
    // project sidebars carry it on their "no project" group below.
    this.noProjectCursor.value = String(data.next_cursor || '');
    for (const group of groups) {
      const sessions = listFrom(group, 'sessions', 'items');
      const projectSource =
        group.project && typeof group.project === 'object'
          ? (group.project as Record<string, unknown>)
          : null;
      if (!projectSource || group.no_project) {
        ungrouped.push(...sessions.map((entry) => this.sessionFrom(entry)));
        this.noProjectCursor.value = String(group.next_cursor || '');
        noProjectPageIncomplete = Boolean(group.next_cursor);
        continue;
      }
      const projectID = String(projectSource.id || '');
      if (group.next_cursor) incompleteProjectIDs.add(projectID);
      const project: Project = {
        id: projectID,
        name: String(projectSource.name || projectSource.title || 'Project'),
        path: String(projectSource.canonical_dir || projectSource.path || ''),
        archived: Boolean(projectSource.archived_at || projectSource.archived),
        available: projectSource.available !== false,
        unavailableReason: String(projectSource.unavailable_reason || ''),
        git: projectSource.git === true,
        sessions: sessions.map((entry) =>
          this.sessionFrom({
            ...entry,
            project_id: projectSource.id,
            project_name: projectSource.name,
            project_unavailable: projectSource.available === false,
            project_unavailable_reason: projectSource.unavailable_reason,
          }),
        ),
        sessionCount: Number(group.session_count) || sessions.length,
        next_cursor: String(group.next_cursor || ''),
        has_more: Boolean(group.next_cursor),
      };
      projects.push(project);
    }
    const incoming = [
      ...recent,
      ...ungrouped,
      ...projects.flatMap((project) => project.sessions || []),
    ];
    const existing = new Map(this.sessions.peek().map((session) => [session.id, session]));
    const merged = new Map(
      incoming.map((session) => {
        const previous = existing.get(session.id);
        const candidate = this.mergeSession(previous, session, false, true);
        return [session.id, previous && semanticEqual(previous, candidate) ? previous : candidate];
      }),
    );
    // A cursor means the response is only the first page for that catalog partition.
    // Keep already-loaded tail rows until a complete snapshot proves they disappeared.
    for (const [id, session] of existing) {
      const incompletePage = session.projectId
        ? incompleteProjectIDs.has(session.projectId)
        : noProjectPageIncomplete;
      const incompleteRecentPage = Boolean(recentCursor) && previousRecentIDs.has(id);
      if (!merged.has(id) && (incompletePage || incompleteRecentPage || this.retainedLocally(id)))
        merged.set(id, session);
    }
    const nextSessions = [...merged.values()].sort(compareSessionsByActivity);
    if (!sameIdentityList(this.sessions.peek(), nextSessions)) this.sessions.value = nextSessions;

    const listedRecent = recent.map((session) => merged.get(session.id) || session);
    if (recentCursor) {
      const listed = new Set(listedRecent.map((session) => session.id));
      for (const previous of previousRecent) {
        const preserved = merged.get(previous.id);
        if (preserved && !listed.has(previous.id)) {
          listedRecent.push(preserved);
          listed.add(previous.id);
        }
      }
    }
    listedRecent.sort(compareSessionsByActivity);
    if (!sameIdentityList(previousRecent, listedRecent)) {
      this.recentSessions.value = listedRecent;
    }
    if (!recentCursor) {
      this.recentCursor.value = '';
      this.recentTailLoaded = false;
    } else if (!this.recentTailLoaded) {
      this.recentCursor.value = recentCursor;
    }

    const existingProjects = new Map(this.projects.peek().map((project) => [project.id, project]));
    const nextProjects = projects.map((project) => {
      const previous = existingProjects.get(project.id);
      const sessions = project.sessions?.map((summary) => merged.get(summary.id) || summary) || [];
      if (incompleteProjectIDs.has(project.id)) {
        const listed = new Set(sessions.map((session) => session.id));
        for (const summary of previous?.sessions || []) {
          const preserved = merged.get(summary.id);
          if (preserved?.projectId === project.id && !listed.has(summary.id)) {
            sessions.push(preserved);
            listed.add(summary.id);
          }
        }
        sessions.sort(compareSessionsByActivity);
      }
      const candidate = {
        ...project,
        sessions,
      };
      return previous && semanticEqual(previous, candidate) ? previous : candidate;
    });
    if (!sameIdentityList(this.projects.peek(), nextProjects)) this.projects.value = nextProjects;
    this.lastSidebarRefreshAt = Date.now();
  }

  async refreshSidebar(authoritative = true): Promise<void> {
    if (!authoritative && this.sidebarRefreshPromise) return this.sidebarRefreshPromise;
    const generation = authoritative ? ++this.sidebarGeneration : this.sidebarGeneration || 1;
    if (!this.sidebarGeneration) this.sidebarGeneration = generation;
    if (authoritative && this.sidebarRefreshPromise)
      await this.sidebarRefreshPromise.catch(() => undefined);
    const showHidden = this.showHidden.peek();
    const request = (async () => {
      const data = this.projectsEnabled.value
        ? await this.services.endpoints.sidebar(showHidden)
        : await this.services.endpoints.sessions(
            `limit=30&include_archived=${showHidden ? '1' : '0'}`,
          );
      if (
        this.services.isDisposed ||
        generation !== this.sidebarGeneration ||
        showHidden !== this.showHidden.peek() ||
        generation < this.lastAppliedSidebarGeneration
      )
        return;
      this.applySidebar(data);
      this.lastAppliedSidebarGeneration = generation;
    })();
    const tracked = request.finally(() => {
      if (this.sidebarRefreshPromise === tracked) this.sidebarRefreshPromise = null;
    });
    this.sidebarRefreshPromise = tracked;
    return tracked;
  }

  async search(query: string): Promise<void> {
    this.sidebarSearch.value = query;
    this.searchAbort?.abort();
    clearTimeout(this.searchTimer);
    if (!query.trim()) {
      this.searchResults.value = null;
      this.searchLoading.value = false;
      this.searchError.value = '';
      return;
    }
    this.searchLoading.value = true;
    this.searchResults.value = [];
    this.searchError.value = '';
    await new Promise<void>((resolve) => {
      this.searchTimer = window.setTimeout(resolve, 180);
    });
    if (this.sidebarSearch.peek() !== query) return;
    const abort = new AbortController();
    this.searchAbort = abort;
    try {
      const data = await this.services.endpoints.searchSessions(
        query,
        this.showHidden.value,
        this.services.config.sidebarCategories,
        abort.signal,
      );
      if (!abort.signal.aborted && this.sidebarSearch.peek() === query)
        this.searchResults.value = listFrom(data, 'sessions', 'items').map((entry) =>
          this.sessionFrom(entry),
        );
    } catch (error) {
      if (!abort.signal.aborted) {
        this.searchResults.value = null;
        this.searchError.value =
          error instanceof Error ? error.message : 'Could not search conversations';
      }
    } finally {
      if (!abort.signal.aborted) this.searchLoading.value = false;
    }
  }

  async mutateSession(session: Session, patch: Record<string, unknown>): Promise<void> {
    await this.services.endpoints.patchSession(session.id, patch);
    this.sessions.value = this.sessions.value.map((entry) =>
      entry.id === session.id ? ({ ...entry, ...patch } as Session) : entry,
    );
    await this.host.refreshSidebar();
    this.host.publishSessionChange();
  }
  async archiveSession(session: Session): Promise<void> {
    const archived = !session.archived;
    await this.services.endpoints.patchSession(session.id, { archived });
    const keepVisible = this.showHidden.peek() || !archived;
    const reconcile = (entries: Session[]): Session[] =>
      entries.flatMap((entry) => {
        if (entry.id !== session.id) return [entry];
        return keepVisible ? [{ ...entry, archived }] : [];
      });
    this.sessions.value = reconcile(this.sessions.peek());
    this.recentSessions.value = reconcile(this.recentSessions.peek());
    this.projects.value = this.projects.peek().map((project) => {
      const contained = Boolean(project.sessions?.some((entry) => entry.id === session.id));
      if (!contained) return project;
      return {
        ...project,
        sessions: reconcile(project.sessions || []),
        sessionCount:
          !keepVisible && project.sessionCount != null
            ? Math.max(0, project.sessionCount - 1)
            : project.sessionCount,
      };
    });
    if (this.searchResults.peek()) this.searchResults.value = reconcile(this.searchResults.peek()!);
    if (session.id === this.activeSessionId.value && archived) this.host.newChat();
    this.host.publishSessionChange();
  }
  async pinSession(session: Session): Promise<void> {
    await this.mutateSession(session, { pinned: !session.pinned });
  }
  openRename(session: Session): void {
    this.renameTarget.value = session;
    this.host.modal.value = 'rename';
  }
  openProjectPicker(session: Session): void {
    this.projectTarget.value = session;
    this.host.modal.value = 'project';
  }
  openAddProject(): void {
    this.projectTarget.value = null;
    this.host.modal.value = 'project';
  }
  async assignProject(projectId: string): Promise<Record<string, unknown> | null> {
    const session = this.projectTarget.value;
    if (!session) return null;
    const response = await this.services.endpoints.setProject(session.id, {
      project_id: projectId,
    });
    await this.host.refreshSidebar();
    this.host.publishSessionChange();
    this.host.modal.value = '';
    return response;
  }
  async createProjectFromWorkspace(name: string): Promise<Record<string, unknown> | null> {
    const session = this.projectTarget.value;
    if (!session) return null;
    const response = await this.services.endpoints.setProject(session.id, {
      create_from_workspace: true,
      name: name.trim(),
    });
    await this.host.refreshSidebar();
    this.host.publishSessionChange();
    this.host.modal.value = '';
    return response;
  }
  async renameSession(
    change: { name: string } | { generatedShortTitle: string; generatedLongTitle: string },
  ): Promise<void> {
    const session = this.renameTarget.value;
    if (!session) return;
    const patch =
      'name' in change
        ? { name: change.name.trim() }
        : {
            name: '',
            generated_short_title: change.generatedShortTitle.trim(),
            generated_long_title: change.generatedLongTitle.trim(),
          };
    await this.services.endpoints.patchSession(session.id, patch);
    await this.host.refreshSidebar();
    this.host.publishSessionChange();
    this.renameTarget.value = null;
    this.host.modal.value = '';
  }
  async improveTitle(): Promise<{
    title: string;
    detail: string;
    abstained?: boolean;
  }> {
    const session = this.renameTarget.value;
    if (!session) return { title: '', detail: '' };
    const data = await this.services.endpoints.refineTitle(session.id);
    return {
      title: String(data.generated_short_title || data.short_title || session.title || ''),
      detail: String(data.generated_long_title || data.long_title || session.longTitle || ''),
      ...(data.refinement_status === 'abstained' ? { abstained: true } : {}),
    };
  }

  async loadMoreRecent(): Promise<void> {
    const cursor = this.recentCursor.peek();
    if (!cursor) return;
    const data = await this.services.endpoints.recentSessions(cursor, this.showHidden.value);
    const incoming = listFrom(data, 'sessions', 'items').map((entry) => this.sessionFrom(entry));
    const existing = new Map(this.sessions.peek().map((entry) => [entry.id, entry]));
    incoming.forEach((entry) =>
      existing.set(entry.id, this.mergeSession(existing.get(entry.id), entry, false, true)),
    );
    this.sessions.value = [...existing.values()].sort(compareSessionsByActivity);
    const recent = new Map(this.recentSessions.peek().map((entry) => [entry.id, entry]));
    incoming.forEach((entry) => recent.set(entry.id, existing.get(entry.id) || entry));
    this.recentSessions.value = [...recent.values()].sort(compareSessionsByActivity);
    this.recentCursor.value = String(data.next_cursor || '');
    this.recentTailLoaded = true;
  }

  async loadMoreProject(projectId: string): Promise<void> {
    const project = this.projects.value.find((entry) => entry.id === projectId);
    if (!project?.next_cursor) return;
    const data = await this.services.endpoints.projectSessions(
      projectId,
      project.next_cursor,
      this.showHidden.value,
    );
    const incoming = listFrom(data, 'sessions', 'items').map((entry) =>
      this.sessionFrom({ ...entry, project_id: project.id, project_name: project.name }),
    );
    const existing = new Map(this.sessions.value.map((entry) => [entry.id, entry]));
    incoming.forEach((entry) =>
      existing.set(entry.id, this.mergeSession(existing.get(entry.id), entry, false, true)),
    );
    this.sessions.value = [...existing.values()];
    this.projects.value = this.projects.value.map((entry) =>
      entry.id === projectId
        ? {
            ...entry,
            sessions: [
              ...(entry.sessions || []),
              ...incoming.filter(
                (candidate) =>
                  !(entry.sessions || []).some((session) => session.id === candidate.id),
              ),
            ],
            next_cursor: String(data.next_cursor || ''),
            has_more: Boolean(data.next_cursor),
          }
        : entry,
    );
  }

  async loadMoreNoProject(): Promise<void> {
    const cursor = this.noProjectCursor.peek();
    if (!cursor) return;
    const data = await this.services.endpoints.noProjectSessions(cursor, this.showHidden.value);
    const incoming = listFrom(data, 'sessions', 'items').map((entry) => this.sessionFrom(entry));
    const existing = new Map(this.sessions.peek().map((entry) => [entry.id, entry]));
    incoming.forEach((entry) =>
      existing.set(entry.id, this.mergeSession(existing.get(entry.id), entry, false, true)),
    );
    this.sessions.value = [...existing.values()].sort(compareSessionsByActivity);
    this.noProjectCursor.value = String(data.next_cursor || '');
  }
  async mutateProject(project: Project, patch: Record<string, unknown>): Promise<void> {
    await this.services.endpoints.patchProject(project.id, patch);
    await this.host.refreshSidebar();
    this.host.publishSessionChange();
  }
  async startProjectChat(projectId: string): Promise<void> {
    this.host.newChat(false, projectId);
    this.sidebarOpen.value = false;
  }

  async refreshHubAgents(force = false): Promise<void> {
    if (this.hubAgentFetch) return this.hubAgentFetch;
    if (document.visibilityState === 'hidden') return;
    const raw = this.services.config.hub?.url || '';
    if (!raw) {
      this.hubAgents.value = [];
      return;
    }
    let hub: URL;
    try {
      hub = new URL(raw, location.href);
    } catch {
      return;
    }
    if (hub.origin !== location.origin) {
      this.hubAgents.value = [];
      return;
    }
    if (!force && Date.now() - this.hubAgentLastFetch < 60_000) return;
    this.hubAgentLastFetch = Date.now();
    const controller = new AbortController();
    const request = (async () => {
      try {
        const path = `${hub.pathname.replace(/\/+$/, '')}/api/nodes`;
        const data = await this.services.endpoints.hubNodes(
          new URL(path, location.origin).href,
          controller.signal,
        );
        const safePath = (value: unknown): string =>
          typeof value === 'string' && /^\/(?![\\/])/.test(value) ? value : '';
        const target = (node: Record<string, unknown>): string => {
          const sessions = recordValue(node.sessions) || {};
          const active = array(sessions.active);
          const recent = array(sessions.recent);
          return (
            safePath(sessions.resume_path) ||
            safePath(active[0]?.resume_path) ||
            safePath(recent[0]?.resume_path) ||
            safePath(node.new_session_path) ||
            (safePath(node.proxy_path) ? `${safePath(node.proxy_path)}?new=1` : '')
          );
        };
        this.hubAgents.value = array(data.nodes)
          .filter((node) => recordValue(node.status)?.reachable === true)
          .map((node) => {
            const id = String(node.id || '');
            const sessions = recordValue(node.sessions) || {};
            const active = Number(sessions.active_count) > 0 || array(sessions.active).length > 0;
            const unseen = Number(sessions.unseen_count) > 0;
            return {
              id,
              name: String(node.name || id),
              target: target(node),
              active,
              attention: id !== this.services.config.hub?.nodeId && Boolean(active || unseen),
            };
          })
          .filter((entry) => entry.name && entry.target)
          .sort(
            (left, right) =>
              left.name.toLowerCase().localeCompare(right.name.toLowerCase()) ||
              left.id.localeCompare(right.id),
          );
      } catch (error) {
        if (error instanceof APIError && error.status >= 400 && error.status < 500)
          this.hubAgents.value = [];
      }
    })();
    this.hubAgentFetch = request.finally(() => {
      this.hubAgentFetch = null;
    });
    return this.hubAgentFetch;
  }

  dispose(): void {
    this.searchAbort?.abort();
    window.clearTimeout(this.searchTimer);
  }
}

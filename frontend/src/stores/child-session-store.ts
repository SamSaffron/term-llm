import { batch, computed, signal } from '@preact/signals';
import type { ActivitySnapshot, StatsChild } from '../api/endpoints';
import { activeExecutionCount } from '../domain/session-activity-summary';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';

/** Failed sources may still return fresh rows from their successful lookups. */
function mergeUnavailableRows<T>(
  cached: T[] | null | undefined,
  incoming: T[] | null,
  key: (row: T) => string,
  unknown: (row: T) => boolean,
): T[] {
  const rows = new Map((cached || []).map((row) => [key(row), row]));
  for (const row of incoming || []) {
    if (!rows.has(key(row)) || !unknown(row)) rows.set(key(row), row);
  }
  return [...rows.values()];
}

/** Owns child provenance used to resolve links from a parent transcript. */
export class ChildSessionStore {
  readonly parentSessionId = signal('');
  readonly children = signal<StatsChild[]>([]);

  /** Descendant activity has its own scope; children above remain direct-only. */
  readonly activitySessionId = signal('');
  readonly activity = signal<ActivitySnapshot | null>(null);
  readonly activityStale = signal(false);
  // Scripts stay out of the Activity UI but still need timely background reconciliation.
  readonly activityActive = computed(
    () =>
      activeExecutionCount(this.activity.value) > 0 ||
      Boolean(this.activity.value?.cells?.some((cell) => cell.live)),
  );
  private activityEtag = '';
  private activityRequest: AbortController | null = null;
  private activityGeneration = 0;
  private activityInvalidated = false;

  private etag = '';
  private request: AbortController | null = null;
  private generation = 0;
  private disposed = false;

  constructor(private readonly services: AppStoreServices) {}

  selectSession(session: Session | null): void {
    const parentId = session?.id || '';
    this.selectActivity(parentId);
    if (parentId === this.parentSessionId.peek()) return;
    // The selected list is replaced, so its validator must be replaced too.
    // Reusing an old ETag after returning here could leave a 304 with no rows.
    this.etag = '';
    const generation = ++this.generation;
    this.request?.abort(new DOMException('Parent session changed', 'AbortError'));
    this.request = null;
    batch(() => {
      this.parentSessionId.value = parentId;
      this.children.value = [];
    });
    if (parentId) void this.refresh(parentId, generation);
  }

  private selectActivity(id: string): void {
    // Activity belongs to the conversation being viewed, never its ancestors.
    // Clear immediately on drill-down so a leaf cannot inherit the parent's robot.
    if (id === this.activitySessionId.peek()) return;
    this.activityGeneration += 1;
    this.pauseActivity();
    this.activityEtag = '';
    batch(() => {
      this.activitySessionId.value = id;
      this.activity.value = null;
      this.activityStale.value = false;
    });
    if (id) void this.refreshActivity();
  }

  pauseActivity(): void {
    this.activityInvalidated = false;
    this.activityRequest?.abort(new DOMException('Activity paused', 'AbortError'));
    this.activityRequest = null;
  }

  async refreshActivity(invalidate = false): Promise<void> {
    const id = this.activitySessionId.peek();
    if (!id || this.disposed || document.visibilityState === 'hidden') return;
    if (invalidate) {
      this.activityEtag = '';
      this.activityInvalidated = Boolean(this.activityRequest);
    }
    // Shared polling and coalesced events may meet; never replace a healthy
    // in-flight read with another identical read.
    if (this.activityRequest) return;
    const generation = this.activityGeneration;
    const request = new AbortController();
    this.activityRequest = request;
    try {
      const response = await this.services.endpoints.sessionActivity(
        id,
        request.signal,
        this.activityEtag,
      );
      if (this.disposed || generation !== this.activityGeneration || request.signal.aborted) return;
      if (!response.__notModified && response.session_id !== id)
        throw new Error('Activity scope mismatch');
      this.activityEtag = response.__etag || (response.__notModified ? this.activityEtag : '');
      if (!response.__notModified) {
        const previous = this.activity.peek();
        // Optional-source failures must not erase known executions or imply
        // completion. A successful empty response still owns membership.
        this.activity.value = {
          ...response,
          agents:
            response.agents_availability === 'unavailable'
              ? mergeUnavailableRows(
                  previous?.agents,
                  response.agents,
                  (row) => row.session_id,
                  (row) => !row.state || ['unknown', 'unavailable'].includes(row.state),
                )
              : response.agents,
          jobs:
            response.jobs_availability === 'unavailable'
              ? mergeUnavailableRows(
                  previous?.jobs,
                  response.jobs,
                  (row) => `${row.job_id}:${row.run_id}`,
                  (row) => !row.status || ['unknown', 'unavailable'].includes(row.status),
                )
              : response.jobs,
        };
      }
      this.activityStale.value = false;
    } catch {
      if (!this.disposed && generation === this.activityGeneration && !request.signal.aborted)
        this.activityStale.value = true;
    } finally {
      if (this.activityRequest === request) {
        this.activityRequest = null;
        if (this.activityInvalidated) {
          this.activityInvalidated = false;
          void this.refreshActivity(true);
        }
      }
    }
  }

  async refresh(
    parentId = this.parentSessionId.peek(),
    generation = this.generation,
  ): Promise<void> {
    if (!parentId || !this.current(generation)) return;
    this.request?.abort(new DOMException('Child provenance refreshed', 'AbortError'));
    const request = new AbortController();
    this.request = request;
    try {
      const response = await this.services.endpoints.sessionChildren(
        parentId,
        request.signal,
        this.etag,
      );
      if (!this.current(generation) || request.signal.aborted) return;
      if (response.__etag) this.etag = response.__etag;
      if (!response.__notModified)
        this.children.value = this.reconcileChildren(parentId, response.children || []);
    } catch {
      // Link discovery is optional; completed tool results still carry the child id.
    } finally {
      if (this.request === request) this.request = null;
    }
  }

  childrenChanged(parentId: string): void {
    if (
      parentId === this.activitySessionId.peek() ||
      this.activity.peek()?.agents?.some((row) => row.session_id === parentId)
    )
      void this.refreshActivity(true);
    if (!parentId || parentId !== this.parentSessionId.peek()) return;
    this.etag = '';
    void this.refresh(parentId, this.generation);
  }

  private reconcileChildren(parentId: string, incoming: StatsChild[]): StatsChild[] {
    // A finished child leaves the live registry before its parent tool result is
    // persisted. During that handoff the same authoritative row can briefly omit
    // its immutable spawn identity. Retain only that identity; incoming rows
    // still own membership and every other field.
    const previous = new Map(
      this.children
        .peek()
        .filter((child) => child.parent_session_id === parentId)
        .map((child) => [child.session_id, child]),
    );
    return incoming.map((child) => {
      const prior = previous.get(child.session_id);
      if (!prior || child.parent_session_id !== parentId) return child;
      const parentSpawnCallId = child.parent_spawn_call_id || prior.parent_spawn_call_id;
      const parentSpawnItemId = child.parent_spawn_item_id || prior.parent_spawn_item_id;
      if (
        parentSpawnCallId === child.parent_spawn_call_id &&
        parentSpawnItemId === child.parent_spawn_item_id
      )
        return child;
      return {
        ...child,
        ...(parentSpawnCallId ? { parent_spawn_call_id: parentSpawnCallId } : {}),
        ...(parentSpawnItemId ? { parent_spawn_item_id: parentSpawnItemId } : {}),
      };
    });
  }

  private current(generation: number): boolean {
    return !this.disposed && generation === this.generation;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.pauseActivity();
    this.generation += 1;
    this.request?.abort(new DOMException('Child provenance store disposed', 'AbortError'));
    this.request = null;
  }
}

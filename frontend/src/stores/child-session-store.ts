import { batch, signal } from '@preact/signals';
import type { StatsChild } from '../api/endpoints';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';

/** Owns child provenance used to resolve links from a parent transcript. */
export class ChildSessionStore {
  readonly parentSessionId = signal('');
  readonly children = signal<StatsChild[]>([]);

  private etag = '';
  private request: AbortController | null = null;
  private generation = 0;
  private disposed = false;

  constructor(private readonly services: AppStoreServices) {}

  selectSession(session: Session | null): void {
    const parentId = session?.id || '';
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
    this.generation += 1;
    this.request?.abort(new DOMException('Child provenance store disposed', 'AbortError'));
    this.request = null;
  }
}

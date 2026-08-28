import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { ChildRun } from '../domain/child-run';
import { persistAgentReadMarker, readAgentReadMarkers } from '../platform/storage';
import type { AppStoreServices } from './app-store-services';
import { listFrom } from './store-utils';

/** Owns child-agent run summaries, ETags, and durable read markers. */
export class ChildRunStore {
  readonly childRuns = signal<ChildRun[]>([]);
  readonly readMarkers: Signal<Record<string, number>>;
  private readonly etags = new Map<string, string>();

  constructor(
    private readonly services: AppStoreServices,
    private readonly activeSessionId: ReadonlySignal<string>,
  ) {
    this.readMarkers = signal(
      readAgentReadMarkers(services.storage, services.keys.agentReadMarkers),
    );
  }

  reloadReadMarkers(): void {
    this.readMarkers.value = readAgentReadMarkers(
      this.services.storage,
      this.services.keys.agentReadMarkers,
    );
  }

  async load(sessionId = this.activeSessionId.peek()): Promise<void> {
    if (!sessionId) {
      this.childRuns.value = [];
      return;
    }
    try {
      const data = await this.services.endpoints.sessionChildren(
        sessionId,
        this.etags.get(sessionId) || '',
      );
      if (this.activeSessionId.peek() !== sessionId) return;
      const etag = String(data.__etag || '');
      if (etag) this.etags.set(sessionId, etag);
      if (data.__notModified) return;
      this.childRuns.value = listFrom(data, 'children', 'items').map((entry) => {
        const childSessionId = String(entry.session_id || entry.id || '');
        const revision = Number(entry.revision) || 0;
        const state = String(entry.state || 'active');
        const unreadTerminal =
          ['complete', 'error', 'failed'].includes(state.toLowerCase()) &&
          revision > (this.readMarkers.peek()[childSessionId] || 0);
        return {
          sessionId: childSessionId,
          parentSessionId: String(entry.parent_session_id || sessionId),
          parentSpawnItemId: Number(entry.parent_spawn_item_id) || undefined,
          parentSpawnCallId: String(entry.parent_spawn_call_id || '') || undefined,
          title: String(entry.title || entry.agent || 'Agent run'),
          agent: String(entry.agent || '') || undefined,
          taskSummary: String(entry.task_summary || '') || undefined,
          state,
          attention: Boolean(entry.attention) || unreadTerminal,
          responseId: String(entry.response_id || '') || undefined,
          runEpoch: Number(entry.run_epoch) || undefined,
          revision,
          startedAt: Number(entry.started_at) || undefined,
          endedAt: Number(entry.ended_at) || undefined,
          approximateTimes: Boolean(entry.approximate_times),
        };
      });
    } catch {
      // Child runs are additive; status reconciliation will retry.
    }
  }

  markRead(sessionId: string): void {
    const child = this.childRuns.peek().find((entry) => entry.sessionId === sessionId);
    if (!child) return;
    persistAgentReadMarker(
      this.services.storage,
      this.services.keys.agentReadMarkers,
      sessionId,
      child.revision,
    );
    this.readMarkers.value = { ...this.readMarkers.peek(), [sessionId]: child.revision };
  }
}

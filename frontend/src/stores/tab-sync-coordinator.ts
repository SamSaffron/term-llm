import type { ReadonlySignal } from '@preact/signals';
import { parseTabEvent, TabSync, type TabEventType } from '../platform/tab-sync';
import type { AppStoreServices } from './app-store-services';

export interface TabSyncHost {
  startupDone: ReadonlySignal<boolean>;
  activeSessionId: ReadonlySignal<string>;
  currentResponseId: (sessionId: string) => string;
  currentRevision: (sessionId: string) => number | undefined;
  draftStorageId: () => string;
  reconcileDraftStorage: (sessionId: string) => void;
  reloadReviewQueue: () => void;
  reconcilePeerChange: () => Promise<void>;
  onPendingIntentStorage: () => void;
  onAgentReadMarkerStorage: () => void;
  serverEventsEnabled: () => boolean;
}

/** Owns BroadcastChannel and storage-event synchronization between browser tabs. */
export class TabSyncCoordinator {
  private channel: BroadcastChannel | null = null;
  private readonly protocol = new TabSync();
  private readonly peerRevisions = new Map<string, number>();
  private peerTimer = 0;
  private pendingPeerSync = false;

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: TabSyncHost,
  ) {}

  installStorageListener(signal: AbortSignal): void {
    addEventListener(
      'storage',
      (rawEvent) => {
        const event = rawEvent as StorageEvent;
        const { keys } = this.services;
        if (event.key === keys.pendingIntents || event.key?.startsWith(`${keys.pendingIntents}:`))
          this.host.onPendingIntentStorage();
        if (event.key?.startsWith(`${keys.draftMessages}:`))
          this.host.reconcileDraftStorage(this.host.draftStorageId());
        if (event.key?.startsWith(`${keys.diffCommentQueue}:`)) this.host.reloadReviewQueue();
        if (event.key?.startsWith(`${keys.agentReadMarkers}:`))
          this.host.onAgentReadMarkerStorage();
      },
      { signal },
    );
  }

  ensureChannel(): void {
    if (this.channel || typeof BroadcastChannel !== 'function') return;
    const scope = this.services.config.hub?.nodeId || this.services.config.prefix;
    try {
      this.channel = new BroadcastChannel(`term-llm:sessions:${scope}`);
    } catch {
      this.services.bumpDiagnostic('storageFailures');
      return;
    }
    this.channel.addEventListener('message', (message) => {
      const parsed = parseTabEvent(message.data);
      const event = this.protocol.accept(message.data);
      if (!event) {
        if (
          parsed === null &&
          !this.host.serverEventsEnabled() &&
          message.data &&
          typeof message.data === 'object' &&
          'v' in (message.data as object)
        )
          this.queuePeerChange();
        return;
      }
      if (event !== 'legacy') {
        if (event.type === 'draft-changed') {
          if (event.sessionId === this.host.draftStorageId())
            this.host.reconcileDraftStorage(event.sessionId);
          return;
        }
        if (event.type === 'review-comment-changed') {
          this.host.reloadReviewQueue();
          return;
        }
        if (this.host.serverEventsEnabled()) return;
        if (event.revision !== undefined && event.sessionId) {
          const previous = this.peerRevisions.get(event.sessionId) || 0;
          if (previous && event.revision > previous + 1) this.pendingPeerSync = true;
          this.peerRevisions.set(event.sessionId, Math.max(previous, event.revision));
        }
      }
      if (this.host.serverEventsEnabled()) return;
      if (!this.host.startupDone.peek()) {
        this.pendingPeerSync = true;
        return;
      }
      this.queuePeerChange();
    });
  }

  publish(
    type: TabEventType = 'session-upserted',
    sessionId = this.host.activeSessionId.peek(),
    responseId = this.host.currentResponseId(sessionId),
    revision = this.host.currentRevision(sessionId),
    operationId?: string,
  ): void {
    this.channel?.postMessage(
      this.protocol.create(
        type,
        {
          ...(sessionId ? { sessionId } : {}),
          ...(responseId ? { responseId } : {}),
          ...(revision !== undefined ? { revision } : {}),
        },
        operationId,
      ),
    );
  }

  flushPending(): void {
    if (!this.pendingPeerSync) return;
    this.pendingPeerSync = false;
    this.queuePeerChange();
  }

  queuePeerChange(): void {
    window.clearTimeout(this.peerTimer);
    this.peerTimer = window.setTimeout(() => void this.host.reconcilePeerChange(), 150);
  }

  closeChannel(): void {
    window.clearTimeout(this.peerTimer);
    this.channel?.close();
    this.channel = null;
  }

  dispose(): void {
    this.closeChannel();
    this.peerRevisions.clear();
  }
}

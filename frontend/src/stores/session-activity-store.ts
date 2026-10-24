import { batch, computed, signal } from '@preact/signals';
import type { ServeSessionActivity } from '../api/generated/types';
import type { AppStoreServices } from './app-store-services';

export type SessionActivity = ServeSessionActivity;

/** Server activity snapshots, with a short bridge for unconfirmed admission. */
export class SessionActivityStore {
  readonly snapshots = signal<Record<string, SessionActivity | undefined>>({});
  private readonly requests = signal<Record<string, { token: number; checking: boolean }>>({});
  private version = 0;
  private readonly observedVersions = new Map<string, number>();
  private readonly reportedOutcomes = new Map<string, string>();
  readonly unconfirmed = computed(() => Object.keys(this.requests.value).length > 0);
  readonly sessionIds = computed(() => [
    ...new Set([...Object.keys(this.snapshots.value), ...Object.keys(this.requests.value)]),
  ]);
  readonly anyActive = computed(
    () =>
      this.unconfirmed.value ||
      Object.values(this.snapshots.value).some((activity) => activity?.status === 'running'),
  );

  constructor(private readonly services: AppStoreServices) {}

  capture(): number {
    return this.version;
  }

  busy(sessionId: string): boolean {
    return Boolean(
      this.requests.value[sessionId] || this.snapshots.value[sessionId]?.status === 'running',
    );
  }

  begin(sessionId: string): number {
    const token = ++this.version;
    this.observedVersions.set(sessionId, token);
    this.requests.value = { ...this.requests.peek(), [sessionId]: { token, checking: false } };
    return token;
  }

  /** A failed transport has an unknown outcome until the next server sample. */
  checking(sessionId: string, token: number): void {
    if (this.requests.peek()[sessionId]?.token !== token) return;
    this.requests.value = { ...this.requests.peek(), [sessionId]: { token, checking: true } };
    this.observedVersions.set(sessionId, ++this.version);
  }

  reject(sessionId: string, token: number): void {
    if (this.requests.peek()[sessionId]?.token === token) this.clearRequest(sessionId);
  }

  observe(sessionId: string, value: unknown, sampledVersion = this.version): boolean {
    const current = this.snapshots.peek()[sessionId];
    const activity = this.parse(value);
    if (!current && !activity && !this.requests.peek()[sessionId]) return true;
    // Newer revisions of the same attempt remain useful even if a stream frame
    // arrived while this snapshot request was in flight.
    const advancesCurrent =
      activity && current && activity.id === current.id && activity.revision > current.revision;
    if ((this.observedVersions.get(sessionId) || 0) > sampledVersion && !advancesCurrent)
      return false;
    if (activity && current) {
      if (activity.id === current.id && activity.revision < current.revision) return false;
      if (
        activity.id !== current.id &&
        (activity.started_at < current.started_at ||
          (activity.started_at === current.started_at && activity.revision < current.revision))
      )
        return false;
    }
    const sawAttempt =
      (current?.id === activity?.id && current?.status === 'running') ||
      (Boolean(this.requests.peek()[sessionId]) && current?.id !== activity?.id);
    batch(() => {
      const snapshots = { ...this.snapshots.peek() };
      if (activity) snapshots[sessionId] = activity;
      else delete snapshots[sessionId];
      this.snapshots.value = snapshots;
      this.observedVersions.set(sessionId, ++this.version);
      if (this.requests.peek()[sessionId]?.checking) this.clearRequest(sessionId);
    });

    const failed = activity?.status === 'failed';
    const outcome = failed
      ? activity?.error
      : activity?.status === 'completed'
        ? activity.message
        : '';
    if (activity && outcome && sawAttempt && this.reportedOutcomes.get(sessionId) !== activity.id) {
      this.reportedOutcomes.set(sessionId, activity.id);
      this.services.toast(outcome, failed ? 'error' : 'success');
    }
    return true;
  }

  private clearRequest(sessionId: string): void {
    const next = { ...this.requests.peek() };
    delete next[sessionId];
    this.requests.value = next;
  }

  private parse(value: unknown): SessionActivity | undefined {
    if (!value || typeof value !== 'object') return undefined;
    const activity = value as SessionActivity;
    return activity.kind === 'compaction' &&
      typeof activity.id === 'string' &&
      ['running', 'completed', 'failed', 'cancelled'].includes(activity.status) &&
      Number.isFinite(activity.revision) &&
      Number.isFinite(activity.started_at)
      ? activity
      : undefined;
  }
}

import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import { APIError, decodeSSE } from '../api/client';
import { initialProjection } from '../domain/response';
import type { ActiveRun, Message, Session } from '../domain/types';
import type { Modal } from './store-types';
import type { AppStoreServices } from './app-store-services';
import { listFrom, recordValue, uuid } from './store-utils';

export interface SkillStoreHost {
  activeSession: ReadonlySignal<Session | null>;
  activeSessionId: ReadonlySignal<string>;
  streaming: ReadonlySignal<boolean>;
  prompt: Signal<string>;
  modal: Signal<Modal>;
  updateSession: (id: string, updater: (session: Session) => Session) => void;
  setRun: (sessionId: string, run: ReturnType<typeof initialProjection>) => void;
  patchSession: (id: string, patch: Partial<Session>) => void;
  refreshSessionMessages: (sessionId: string) => Promise<void>;
  trackIntent: (
    sessionId: string,
    intent: { id: string; clientMessageId: string; content: string; created: number },
  ) => void;
  retireIntent: (sessionId: string, clientMessageId?: string) => void;
  streamResponse: (responseId: string, sessionId: string, sequence: number) => Promise<void>;
}

/** Owns skill discovery and isolated skill-run stream lifecycles. */
export class SkillStore {
  readonly skills = signal<Record<string, unknown>[]>([]);

  private readonly runAborts = new Map<string, AbortController>();
  private readonly runCursors = new Map<
    string,
    { sessionId: string; eventsURL: string; sequence: number }
  >();
  private epoch = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: SkillStoreHost,
  ) {}

  async loadSkills(sessionId = this.host.activeSession.value?.id || ''): Promise<void> {
    const epoch = ++this.epoch;
    if (!sessionId) {
      this.skills.value = [];
      return;
    }
    const data = await this.services.endpoints.skills(sessionId);
    if (epoch !== this.epoch || this.host.activeSessionId.peek() !== sessionId) return;
    this.skills.value = listFrom(data, 'skills', 'items');
  }
  private skillRunTerminal(status: unknown): boolean {
    return ['complete', 'completed', 'failed', 'cancelled'].includes(
      String(status || '').toLowerCase(),
    );
  }
  private updateSkillRunMessage(
    sessionId: string,
    runId: string,
    patch: Record<string, unknown>,
  ): void {
    this.host.updateSession(sessionId, (session) => {
      const messages = session.messages.map((message) => {
        if (message.role !== 'skill-run' || String(message.runId || '') !== runId) return message;
        const status = String(patch.status || message.status || 'running');
        const output = String(patch.output ?? message.output ?? '');
        const error = String(patch.error ?? message.error ?? '');
        const progress = String(patch.progress ?? message.progress ?? '');
        return {
          ...message,
          ...patch,
          status,
          output,
          error,
          progress,
          content: error || output || progress,
        };
      });
      return { ...session, messages };
    });
  }
  private applySkillRunEvent(runId: string, envelope: Record<string, unknown>): void {
    const cursor = this.runCursors.get(runId);
    if (!cursor) return;
    const sequence = Number(envelope.sequence || envelope.sequence_number) || 0;
    if (sequence && sequence <= cursor.sequence) return;
    if (sequence) cursor.sequence = sequence;
    const type = String(envelope.type || '');
    const data = recordValue(envelope.data) || envelope;
    const patch: Record<string, unknown> = {};
    if (type === 'skill_run.created')
      Object.assign(patch, {
        status: 'running',
        skill: String(data.skill || ''),
        agent: String(data.agent || ''),
        childSessionId: String(data.child_session_id || ''),
      });
    else if (type === 'skill_run.progress')
      patch.progress = String(data.message || data.progress || data.stage || 'Working…');
    else if (type === 'skill_run.completed')
      Object.assign(patch, {
        status: String(data.status || 'completed'),
        output: String(data.output || ''),
        error: String(data.error || ''),
        progress: '',
        childSessionId: String(data.child_session_id || ''),
      });
    else return;
    this.updateSkillRunMessage(cursor.sessionId, runId, patch);
    if (this.skillRunTerminal(patch.status)) {
      this.runAborts.get(runId)?.abort();
      this.runCursors.delete(runId);
      void this.host.refreshSessionMessages(cursor.sessionId);
    }
  }
  private async reconcileSkillRun(runId: string): Promise<void> {
    const cursor = this.runCursors.get(runId);
    if (!cursor) return;
    const snapshot = await this.services.endpoints.skillRun(cursor.sessionId, runId);
    if (this.services.isDisposed || this.runCursors.get(runId) !== cursor) return;
    const events = Array.isArray(snapshot.events) ? snapshot.events : [];
    events.forEach((event) => {
      if (event && typeof event === 'object')
        this.applySkillRunEvent(runId, event as Record<string, unknown>);
    });
    this.updateSkillRunMessage(cursor.sessionId, runId, {
      status: String(snapshot.status || 'running'),
      output: String(snapshot.output || ''),
      error: String(snapshot.error || ''),
      childSessionId: String(snapshot.child_session_id || ''),
    });
    if (this.skillRunTerminal(snapshot.status)) {
      this.runCursors.delete(runId);
      await this.host.refreshSessionMessages(cursor.sessionId);
    }
  }
  private async followSkillRun(runId: string): Promise<void> {
    const cursor = this.runCursors.get(runId);
    if (!cursor || this.runAborts.has(runId)) return;
    const controller = new AbortController();
    this.runAborts.set(runId, controller);
    try {
      const separator = cursor.eventsURL.includes('?') ? '&' : '?';
      const response = await this.services.api.request(
        `${cursor.eventsURL}${separator}after=${encodeURIComponent(cursor.sequence)}`,
        {
          signal: controller.signal,
          headers: { Accept: 'text/event-stream', 'X-Term-LLM-Session-ID': cursor.sessionId },
        },
        { policy: 'stream', retries: 0, timeoutMs: 0, auth: 'session' },
      );
      if (!response.ok || !response.body)
        throw new APIError(
          (await response.text()) || `Skill run stream returned ${response.status}`,
          response.status,
        );
      for await (const frame of decodeSSE(response.body, controller.signal)) {
        let envelope: Record<string, unknown>;
        try {
          envelope = JSON.parse(frame.data) as Record<string, unknown>;
        } catch {
          continue;
        }
        if (!envelope.type && frame.event !== 'message') envelope.type = frame.event;
        this.applySkillRunEvent(runId, envelope);
        if (!this.runCursors.has(runId)) return;
      }
    } catch (error) {
      if (!controller.signal.aborted)
        this.updateSkillRunMessage(cursor.sessionId, runId, {
          progress:
            navigator.onLine === false
              ? 'Offline — run is safe; reconnecting when online'
              : 'Reconnecting…',
          streamError: String(error),
        });
    } finally {
      if (this.runAborts.get(runId) === controller) this.runAborts.delete(runId);
    }
    if (!this.runCursors.has(runId)) return;
    await this.reconcileSkillRun(runId).catch(() => undefined);
    if (this.runCursors.has(runId))
      this.services.schedule(() => void this.followSkillRun(runId), 1_000);
  }
  async invokeSkill(name: string, args: string): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session) return;
    const skill = this.skills.peek().find((entry) => String(entry.name || '') === name);
    if (this.host.streaming.peek() && skill?.execution !== 'isolated') {
      this.services.toast(
        'This main-conversation skill cannot run while a response is active.',
        'error',
      );
      return;
    }
    const id = uuid();
    const invocation = `/${name}${args.trim() ? ` ${args.trim()}` : ''}`;
    const optimistic: Message = {
      id: `pending_${id}`,
      role: 'user',
      content: invocation,
      clientMessageId: id,
      created: Date.now(),
    };
    this.host.updateSession(session.id, (entry) => ({
      ...entry,
      messages: [...entry.messages, optimistic],
      lastMessageAt: Date.now(),
    }));
    this.host.trackIntent(session.id, {
      id: optimistic.id,
      clientMessageId: id,
      content: invocation,
      created: optimistic.created,
    });
    try {
      const data = await this.services.endpoints.invokeSkill(
        session.id,
        { name, arguments: args, client_message_id: id },
        id,
      );
      if (String(data.execution) === 'isolated' && data.run_id) {
        const runId = String(data.run_id);
        const eventsURL = String(
          data.events_url ||
            `/v1/sessions/${encodeURIComponent(session.id)}/skill-runs/${encodeURIComponent(runId)}/events`,
        );
        const message: Message = {
          id: `skill-run-${runId}`,
          role: 'skill-run',
          content: '',
          created: Date.now(),
          runId,
          skill: name,
          status: String(data.status || 'running'),
          childSessionId: String(data.child_session_id || ''),
          eventsURL,
        };
        this.host.updateSession(session.id, (entry) => ({
          ...entry,
          messages: [...entry.messages, message],
        }));
        this.runCursors.set(runId, { sessionId: session.id, eventsURL, sequence: 0 });
        void this.followSkillRun(runId);
      } else {
        const responseId = String(data.response_id || '');
        const epoch = Number(data.run_epoch) || 0;
        if (!responseId || !epoch)
          throw new Error('Skill invocation did not return a response ID and run epoch.');
        const run: ActiveRun = {
          responseId,
          sessionId: session.id,
          epoch,
          status: 'streaming',
          lastSequence: 0,
          startedRev: Number(data.started_rev) || session.transcriptRev || 0,
          startedAt: Number(data.started_at) || Date.now(),
          reconnects: 0,
          requestId: id,
        };
        this.host.setRun(session.id, initialProjection(run));
        this.host.patchSession(session.id, { activeResponseId: responseId });
        void this.host.streamResponse(responseId, session.id, 0);
      }
      this.host.modal.value = '';
      this.services.toast(`Started ${name}.`, 'success');
    } catch (error) {
      this.host.retireIntent(session.id, id);
      if (!this.host.prompt.value) this.host.prompt.value = invocation;
      this.services.toast(error, 'error');
    }
  }
  async cancelSkill(runId: string): Promise<void> {
    const session = this.host.activeSession.value;
    if (!session) return;
    this.updateSkillRunMessage(session.id, runId, {
      status: 'cancelling',
      progress: 'Cancelling…',
    });
    await this.services.endpoints.cancelSkillRun(session.id, runId);
    if (this.runCursors.has(runId))
      await this.reconcileSkillRun(runId).catch(() => this.host.refreshSessionMessages(session.id));
    else await this.host.refreshSessionMessages(session.id);
  }

  dispose(): void {
    this.epoch += 1;
    this.runAborts.forEach((abort) => abort.abort());
    this.runAborts.clear();
    this.runCursors.clear();
  }
}

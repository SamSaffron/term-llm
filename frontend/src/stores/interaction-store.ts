import { signal, type Signal } from '@preact/signals';
import type { ApprovalPrompt, AskUserPrompt, InteractionRecord } from '../domain/types';
import { errorMessage } from '../domain/text';
import type { Modal } from './store-types';
import type { AppStoreServices } from './app-store-services';

export type PublishInteractionChange = (
  type: 'interaction-changed',
  sessionId: string,
  responseId?: string,
) => void;

/** Owns durable interaction prompts and their submission lifecycle. */
export class InteractionStore {
  readonly askUser = signal<AskUserPrompt | null>(null);
  readonly approval = signal<ApprovalPrompt | null>(null);
  readonly interactions = signal<Record<string, InteractionRecord>>({});
  readonly order = signal<string[]>([]);

  private readonly submissions = new Map<string, Promise<void>>();

  constructor(
    private readonly services: AppStoreServices,
    private readonly modal: Signal<Modal>,
    private readonly publish: PublishInteractionChange,
  ) {}

  upsert(
    kind: InteractionRecord['kind'],
    sessionId: string,
    responseId: string,
    requestId: string,
    prompt: ApprovalPrompt | AskUserPrompt,
  ): string {
    const key = `${sessionId}:${responseId}:${requestId}`;
    const existing = this.interactions.peek()[key];
    const record: InteractionRecord = existing || {
      key,
      sessionId,
      responseId,
      requestId,
      kind,
      state: 'waiting',
      order: this.order.peek().length,
      createdAt: Date.now(),
      prompt,
    };
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...record, prompt },
    };
    if (!existing) {
      this.order.value = [...this.order.peek(), key];
      this.publish('interaction-changed', sessionId, responseId);
    }
    return key;
  }

  resolve(
    kind: InteractionRecord['kind'],
    sessionId: string,
    responseId: string,
    requestId: string,
    outcome: string,
    resolvedAt = Date.now(),
  ): void {
    const existing = this.find(kind, sessionId, requestId, responseId);
    const key = existing?.key || `${sessionId}:${responseId}:${requestId}`;
    const normalized = outcome.replaceAll('_', '-');
    const state: InteractionRecord['state'] =
      normalized === 'accepted' || normalized === 'answered'
        ? 'accepted'
        : normalized === 'denied'
          ? 'denied'
          : normalized === 'cancelled-by-user' || normalized === 'cancelled'
            ? 'cancelled-by-user'
            : normalized === 'failed'
              ? 'failed'
              : 'cancelled-by-agent';
    const prompt =
      existing?.prompt ||
      (kind === 'approval'
        ? ({ sessionId, id: requestId, title: 'Access request' } satisfies ApprovalPrompt)
        : ({ sessionId, callId: requestId, questions: [] } satisfies AskUserPrompt));
    const changed = !existing || existing.state !== state || existing.outcome !== outcome;
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: {
        key,
        sessionId,
        responseId,
        requestId,
        kind,
        order: existing?.order ?? this.order.peek().length,
        createdAt: existing?.createdAt || resolvedAt,
        prompt,
        ...existing,
        state,
        outcome,
        resolvedAt,
      },
    };
    if (!existing) this.order.value = [...this.order.peek(), key];
    this.services.bumpDiagnostic('interactionReconciliations');
    if (kind === 'approval' && this.approval.peek()?.id === requestId) this.approval.value = null;
    if (kind === 'ask-user' && this.askUser.peek()?.callId === requestId) this.askUser.value = null;
    if (changed) this.publish('interaction-changed', sessionId, responseId);
  }

  find(
    kind: InteractionRecord['kind'],
    sessionId: string,
    requestId: string,
    responseId = '',
  ): InteractionRecord | null {
    return (
      Object.values(this.interactions.peek()).find(
        (entry) =>
          entry.kind === kind &&
          entry.sessionId === sessionId &&
          entry.requestId === requestId &&
          (!responseId || entry.responseId === responseId),
      ) || null
    );
  }

  shouldOpen(kind: InteractionRecord['kind'], sessionId: string, requestId: string): boolean {
    const state = this.find(kind, sessionId, requestId)?.state;
    return state === 'waiting' || state === 'failed';
  }

  present(
    kind: InteractionRecord['kind'],
    sessionId: string,
    responseId: string,
    requestId: string,
    prompt: ApprovalPrompt | AskUserPrompt,
  ): void {
    this.upsert(kind, sessionId, responseId, requestId, prompt);
    if (!this.shouldOpen(kind, sessionId, requestId)) return;
    if (kind === 'ask-user') this.askUser.value = prompt as AskUserPrompt;
    else this.approval.value = prompt as ApprovalPrompt;
  }

  dismiss(kind: InteractionRecord['kind'], promptOverride?: ApprovalPrompt | AskUserPrompt): void {
    const prompt =
      promptOverride || (kind === 'ask-user' ? this.askUser.peek() : this.approval.peek());
    const requestId =
      kind === 'ask-user'
        ? (prompt as AskUserPrompt | null)?.callId
        : (prompt as ApprovalPrompt | null)?.id;
    if (requestId) {
      const record = this.find(kind, prompt?.sessionId || '', requestId);
      if (record)
        this.interactions.value = {
          ...this.interactions.peek(),
          [record.key]: { ...record, state: 'dismissed' },
        };
    }
    if (kind === 'ask-user') this.askUser.value = null;
    else this.approval.value = null;
    this.modal.value = '';
  }

  open(key: string): void {
    const record = this.interactions.peek()[key];
    if (!record || !['waiting', 'dismissed', 'failed'].includes(record.state)) return;
    if (record.kind === 'ask-user') {
      this.askUser.value = record.prompt as AskUserPrompt;
      this.modal.value = 'ask-user';
    } else {
      this.approval.value = record.prompt as ApprovalPrompt;
      this.modal.value = 'approval';
    }
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...record, state: 'waiting' },
    };
  }

  cancelOutstandingForResponse(sessionId: string, responseId: string): void {
    const interactions = { ...this.interactions.peek() };
    let changed = false;
    for (const [key, interaction] of Object.entries(interactions)) {
      if (
        interaction.sessionId === sessionId &&
        interaction.responseId === responseId &&
        ['waiting', 'dismissed', 'submitting', 'failed'].includes(interaction.state)
      ) {
        interactions[key] = {
          ...interaction,
          state: 'cancelled-by-agent',
          outcome: 'Decision no longer needed',
          resolvedAt: Date.now(),
        };
        changed = true;
      }
    }
    if (changed) this.interactions.value = interactions;
  }

  async answerAskUser(
    answers: unknown = [],
    cancelled = false,
    promptOverride?: AskUserPrompt,
  ): Promise<void> {
    const prompt = promptOverride || this.askUser.value;
    const requestId = prompt?.callId;
    if (!prompt || !requestId) return;
    const record = this.find('ask-user', prompt.sessionId, requestId);
    const key = record?.key || this.upsert('ask-user', prompt.sessionId, '', requestId, prompt);
    const existing = this.submissions.get(key);
    if (existing) return existing;
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...this.interactions.peek()[key], state: 'submitting', error: '' },
    };
    const request = (async () => {
      try {
        const result = (await this.services.endpoints.askUser(
          prompt.sessionId,
          cancelled ? { call_id: requestId, cancelled: true } : { call_id: requestId, answers },
          requestId,
        )) as Record<string, unknown>;
        const authoritative = String(result.status || '') === 'already_resolved';
        this.resolve(
          'ask-user',
          prompt.sessionId,
          record?.responseId || '',
          requestId,
          authoritative
            ? String(result.outcome || 'resolved')
            : cancelled
              ? 'cancelled-by-user'
              : 'answered',
          authoritative ? Number(result.resolved_at) || Date.now() : Date.now(),
        );
      } catch (error) {
        const current = this.interactions.peek()[key];
        this.interactions.value = {
          ...this.interactions.peek(),
          [key]: { ...current, state: 'failed', error: errorMessage(error) },
        };
        throw error;
      }
    })();
    this.submissions.set(key, request);
    try {
      await request;
    } finally {
      if (this.submissions.get(key) === request) this.submissions.delete(key);
    }
  }

  async decideApproval(
    choice: number,
    resumeAuto = false,
    promptOverride?: ApprovalPrompt,
    cancelled = false,
  ): Promise<void> {
    const prompt = promptOverride || this.approval.value;
    const requestId = prompt?.id;
    if (!prompt || !requestId) return;
    const record = this.find('approval', prompt.sessionId, requestId);
    const key = record?.key || this.upsert('approval', prompt.sessionId, '', requestId, prompt);
    const existing = this.submissions.get(key);
    if (existing) return existing;
    this.interactions.value = {
      ...this.interactions.peek(),
      [key]: { ...this.interactions.peek()[key], state: 'submitting', error: '' },
    };
    const denied = prompt.options?.find((option) => option.index === choice)?.choice === 'deny';
    const request = (async () => {
      try {
        const result = (await this.services.endpoints.approval(
          prompt.sessionId,
          cancelled
            ? { approval_id: requestId, cancelled: true }
            : { approval_id: requestId, choice, resume_auto: resumeAuto },
          requestId,
        )) as Record<string, unknown>;
        const authoritative = String(result.status || '') === 'already_resolved';
        this.resolve(
          'approval',
          prompt.sessionId,
          record?.responseId || '',
          requestId,
          authoritative
            ? String(result.outcome || 'resolved')
            : cancelled
              ? 'cancelled-by-user'
              : denied
                ? 'denied'
                : 'accepted',
          authoritative ? Number(result.resolved_at) || Date.now() : Date.now(),
        );
      } catch (error) {
        const current = this.interactions.peek()[key];
        this.interactions.value = {
          ...this.interactions.peek(),
          [key]: { ...current, state: 'failed', error: errorMessage(error) },
        };
        throw error;
      }
    })();
    this.submissions.set(key, request);
    try {
      await request;
    } finally {
      if (this.submissions.get(key) === request) this.submissions.delete(key);
    }
  }
}

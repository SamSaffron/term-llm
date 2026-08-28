import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import { decodeSSE } from '../api/client';
import { errorMessage } from '../domain/text';
import type { Session } from '../domain/types';
import type { Modal, SideQuestionState } from './store-types';
import { recordValue } from './store-utils';
import type { AppStoreServices } from './app-store-services';

export interface SideQuestionOptions {
  activeSession: ReadonlySignal<Session | null>;
  activeSessionId: ReadonlySignal<string>;
  draftActive: ReadonlySignal<boolean>;
  modal: Signal<Modal>;
}

/** Owns the independent side-question stream and its stale-result guards. */
export class SideQuestionStore {
  readonly state = signal<SideQuestionState>({
    sessionId: '',
    loading: false,
    running: false,
    draft: '',
    question: '',
    response: '',
    error: '',
    history: [],
  });

  private abort: AbortController | null = null;
  private epoch = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: SideQuestionOptions,
  ) {}

  reset(): void {
    const state = this.state.peek();
    const owner = state.sessionId;
    this.epoch += 1;
    this.abort?.abort();
    this.abort = null;
    if (state.running && owner)
      void this.services.endpoints.cancelSideQuestion(owner).catch(() => undefined);
    if (this.options.modal.peek() === 'side') this.options.modal.value = '';
    this.state.value = {
      sessionId: '',
      loading: false,
      running: false,
      draft: '',
      question: '',
      response: '',
      error: '',
      history: [],
    };
  }

  open(question = ''): boolean {
    const session = this.options.activeSession.peek();
    if (!session || this.options.draftActive.peek()) {
      this.services.toast('Start the conversation before asking a side question.', 'error');
      return false;
    }
    const value = question.trim();
    if (this.state.peek().sessionId !== session.id) this.reset();
    this.options.modal.value = 'side';
    if (value) {
      void this.ask(value);
      return true;
    }
    const epoch = ++this.epoch;
    this.state.value = {
      ...this.state.peek(),
      sessionId: session.id,
      loading: true,
      error: '',
    };
    void this.recover(session.id, epoch);
    return true;
  }

  setDraft(value: string): void {
    this.state.value = { ...this.state.peek(), draft: value };
  }

  async recover(
    sessionId = this.options.activeSession.peek()?.id || '',
    epoch = ++this.epoch,
  ): Promise<void> {
    if (!sessionId) return;
    try {
      const value = await this.services.endpoints.sideQuestionState(sessionId);
      if (
        epoch !== this.epoch ||
        this.options.activeSessionId.peek() !== sessionId ||
        this.state.peek().sessionId !== sessionId
      )
        return;
      const running = Boolean(value.running);
      this.state.value = {
        ...this.state.peek(),
        sessionId,
        loading: false,
        running,
        question: running ? String(value.question || '') : '',
        response: running ? String(value.response || '') : '',
        error: String(value.error || ''),
        history: Array.isArray(value.history)
          ? (value.history as SideQuestionState['history'])
          : [],
      };
    } catch (error) {
      if (epoch !== this.epoch) return;
      this.state.value = {
        ...this.state.peek(),
        loading: false,
        error: errorMessage(error),
      };
    }
  }

  async ask(question: string): Promise<void> {
    const session = this.options.activeSession.peek();
    const value = question.trim();
    if (!session || !value) return;
    if (this.state.peek().running) {
      this.state.value = { ...this.state.peek(), error: 'A side question is already running.' };
      return;
    }
    this.abort?.abort();
    const controller = new AbortController();
    const epoch = ++this.epoch;
    this.abort = controller;
    this.state.value = {
      ...this.state.peek(),
      sessionId: session.id,
      loading: false,
      running: true,
      draft: '',
      question: value,
      response: '',
      error: '',
    };
    try {
      const response = await this.services.endpoints.startSideQuestion(session.id, value);
      if (!response.ok || !response.body)
        throw new Error((await response.text()) || `Side question failed (${response.status})`);
      let answer = '';
      let generation = Number(response.headers.get('x-side-generation') || 0);
      for await (const frame of decodeSSE(response.body, controller.signal)) {
        if (epoch !== this.epoch || this.options.activeSessionId.peek() !== session.id) return;
        let event: Record<string, unknown>;
        try {
          event = JSON.parse(frame.data) as Record<string, unknown>;
        } catch {
          continue;
        }
        const eventGeneration = Number(event.generation || 0);
        if (!generation && eventGeneration) generation = eventGeneration;
        if (generation && eventGeneration && eventGeneration !== generation) continue;
        if (event.type === 'text_delta') answer += String(event.text || '');
        else if (event.type === 'attempt_discard') answer = '';
        else if (event.type === 'done' && recordValue(event.result))
          answer = String(recordValue(event.result)?.response || answer);
        this.state.value = { ...this.state.peek(), response: answer };
      }
      if (!controller.signal.aborted && epoch === this.epoch) await this.recover(session.id, epoch);
    } catch (error) {
      if (!controller.signal.aborted && epoch === this.epoch) {
        void this.services.endpoints.cancelSideQuestion(session.id).catch(() => undefined);
        this.state.value = {
          ...this.state.peek(),
          loading: false,
          running: false,
          error: errorMessage(error),
        };
      }
    } finally {
      if (this.abort === controller) this.abort = null;
    }
  }

  cancel(): void {
    const state = this.state.peek();
    const owner = state.sessionId;
    const wasRunning = state.running;
    this.epoch += 1;
    this.abort?.abort();
    this.abort = null;
    this.state.value = {
      ...state,
      loading: false,
      running: false,
      question: '',
      response: '',
      error: '',
    };
    if (wasRunning && owner)
      void this.services.endpoints.cancelSideQuestion(owner).catch(() => undefined);
  }

  close(): void {
    if (this.options.modal.peek() === 'side') this.options.modal.value = '';
    if (this.state.peek().running || this.state.peek().loading) this.cancel();
  }

  dispose(): void {
    this.epoch += 1;
    this.abort?.abort();
    this.abort = null;
  }
}

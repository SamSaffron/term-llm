import { signal, type Signal } from '@preact/signals';
import { decodeSSE } from '../api/client';
import type { Endpoints } from '../api/endpoints';

export type ShellStatus = 'idle' | 'connecting' | 'running' | 'reconnecting' | 'exited' | 'error';

export interface ShellSink {
  write(data: Uint8Array): void;
  reset(): void;
}

interface ShellEventData {
  shell_id?: string;
  offset?: number;
  next_offset?: number;
  exit_code?: number;
  data?: string;
}

const delay = (milliseconds: number, signal: AbortSignal): Promise<void> =>
  new Promise((resolve) => {
    const timer = setTimeout(resolve, milliseconds);
    signal.addEventListener(
      'abort',
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });

function decodeBase64(value: string): Uint8Array {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

function encodeBase64(bytes: Uint8Array): string {
  let binary = '';
  for (let offset = 0; offset < bytes.length; offset += 8192) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 8192));
  }
  return btoa(binary);
}

function abortError(error: unknown): boolean {
  return error instanceof DOMException
    ? error.name === 'AbortError'
    : (error as { name?: string } | null)?.name === 'AbortError';
}

/** Owns the detachable browser shell transport; terminal rendering stays in the lazy overlay chunk. */
export class ShellStore {
  readonly enabled = signal(false);
  readonly visible = signal(false);
  readonly status: Signal<ShellStatus> = signal('idle');
  readonly sessionId = signal('');
  readonly shellId = signal('');
  readonly cwd = signal('');
  readonly offset = signal(0);
  readonly exitCode = signal<number | null>(null);
  readonly error = signal('');

  private generation = 0;
  private streamAbort: AbortController | null = null;
  private inputTimer = 0;
  private resizeTimer = 0;
  private inputChunks: Uint8Array[] = [];
  private inputChain: Promise<void> = Promise.resolve();
  private cols = 80;
  private rows = 24;

  constructor(
    private readonly endpoints: Endpoints,
    private readonly toast: (message: unknown, kind?: 'info' | 'success' | 'error') => void,
    private readonly activeSessionId: () => string,
  ) {}

  show(sessionId: string): boolean {
    if (!this.enabled.peek()) {
      this.toast('Interactive shell is unavailable on this server.', 'error');
      return false;
    }
    if (!sessionId || sessionId !== this.activeSessionId()) {
      this.toast('Start the conversation before opening a shell.', 'error');
      return false;
    }
    if (this.sessionId.peek() !== sessionId) this.resetBinding(sessionId);
    this.visible.value = true;
    return true;
  }

  back(): void {
    this.visible.value = false;
    this.detach();
  }

  async connect(cols: number, rows: number, sink: ShellSink): Promise<void> {
    const sessionId = this.sessionId.peek();
    if (!this.bindingActive(sessionId)) return;
    this.cols = cols;
    this.rows = rows;
    const generation = this.invalidateGeneration();
    const controller = new AbortController();
    this.streamAbort = controller;
    this.status.value = 'connecting';
    this.error.value = '';
    // Every overlay mount owns a fresh terminal surface. Replay from the start of
    // the retained server buffer instead of resuming at the old, now invisible,
    // surface's offset.
    this.offset.value = 0;
    sink.reset();
    try {
      const created = await this.endpoints.shellCreate(sessionId, cols, rows);
      if (!this.current(generation, sessionId)) return;
      this.shellId.value = created.shell_id;
      this.cwd.value = created.cwd;
      this.exitCode.value = null;
      this.status.value = 'running';
      await this.superviseStream(generation, sessionId, sink, controller);
    } catch (error) {
      if (!this.current(generation, sessionId) || abortError(error)) return;
      this.status.value = 'error';
      this.error.value = error instanceof Error ? error.message : 'Could not open the shell.';
    }
  }

  async restart(cols: number, rows: number, sink: ShellSink): Promise<void> {
    if (!this.bindingActive()) return;
    this.shellId.value = '';
    this.offset.value = 0;
    this.exitCode.value = null;
    sink.reset();
    await this.connect(cols, rows, sink);
  }

  private async superviseStream(
    generation: number,
    sessionId: string,
    sink: ShellSink,
    controller: AbortController,
  ): Promise<void> {
    let retry = 250;
    while (this.current(generation, sessionId) && !controller.signal.aborted) {
      try {
        const response = await this.endpoints.shellStream(
          sessionId,
          this.shellId.peek(),
          this.offset.peek(),
          controller.signal,
        );
        if (!this.current(generation, sessionId)) {
          await response.body?.cancel();
          return;
        }
        if (response.status === 409) {
          await response.body?.cancel();
          const attached = await this.endpoints.shellCreate(sessionId, this.cols, this.rows);
          if (!this.current(generation, sessionId)) return;
          if (attached.shell_id !== this.shellId.peek()) {
            this.shellId.value = attached.shell_id;
            this.cwd.value = attached.cwd;
            this.offset.value = 0;
            sink.reset();
          }
          continue;
        }
        if (!response.ok || !response.body) {
          const body = await response.text();
          throw new Error(body || `Shell stream returned ${response.status}`);
        }
        this.status.value = 'running';
        this.error.value = '';
        retry = 250;
        for await (const message of decodeSSE(response.body, controller.signal)) {
          if (!this.current(generation, sessionId)) return;
          let data: ShellEventData;
          try {
            data = JSON.parse(message.data) as ShellEventData;
          } catch {
            continue;
          }
          if (message.event === 'reset') {
            this.offset.value = Math.max(0, Number(data.offset) || 0);
            sink.reset();
            continue;
          }
          if (message.event === 'output' && data.data) {
            const start = Math.max(0, Number(data.offset) || 0);
            const next = Math.max(start, Number(data.next_offset) || start);
            if (next <= this.offset.peek()) continue;
            if (start !== this.offset.peek()) {
              this.offset.value = start;
              sink.reset();
            }
            sink.write(decodeBase64(data.data));
            this.offset.value = next;
            continue;
          }
          if (message.event === 'exit') {
            this.offset.value = Math.max(this.offset.peek(), Number(data.offset) || 0);
            this.exitCode.value = Number.isFinite(data.exit_code) ? Number(data.exit_code) : -1;
            this.status.value = 'exited';
            return;
          }
        }
      } catch (error) {
        if (!this.current(generation, sessionId) || controller.signal.aborted || abortError(error))
          return;
        this.error.value = error instanceof Error ? error.message : 'Shell stream disconnected.';
      }
      if (!this.current(generation, sessionId) || controller.signal.aborted) return;
      this.status.value = 'reconnecting';
      await delay(retry, controller.signal);
      retry = Math.min(5000, retry * 2);
    }
  }

  input(data: string): void {
    if (!data || this.status.peek() !== 'running' || !this.bindingActive()) return;
    const generation = this.generation;
    this.inputChunks.push(new TextEncoder().encode(data));
    if (this.inputTimer) return;
    this.inputTimer = window.setTimeout(() => {
      this.inputTimer = 0;
      this.flushInput(generation);
    }, 8);
  }

  private flushInput(generation: number): void {
    if (generation !== this.generation || !this.bindingActive() || !this.inputChunks.length) return;
    const size = this.inputChunks.reduce((total, chunk) => total + chunk.length, 0);
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const chunk of this.inputChunks.splice(0)) {
      bytes.set(chunk, offset);
      offset += chunk.length;
    }
    const sessionId = this.sessionId.peek();
    const shellId = this.shellId.peek();
    for (let start = 0; start < bytes.length; start += 48 * 1024) {
      const payload = encodeBase64(bytes.subarray(start, start + 48 * 1024));
      this.inputChain = this.inputChain
        .then(async () => {
          if (!this.inputCurrent(generation, sessionId, shellId)) return;
          await this.endpoints.shellInput(sessionId, shellId, payload);
        })
        .catch((error: unknown) => {
          if (!this.inputCurrent(generation, sessionId, shellId)) return;
          this.status.value = 'error';
          this.error.value = error instanceof Error ? error.message : 'Shell input failed.';
        });
    }
  }

  resize(cols: number, rows: number): void {
    if (!this.bindingActive()) return;
    this.cols = cols;
    this.rows = rows;
    const generation = this.generation;
    clearTimeout(this.resizeTimer);
    this.resizeTimer = window.setTimeout(() => {
      this.resizeTimer = 0;
      const sessionId = this.sessionId.peek();
      const shellId = this.shellId.peek();
      if (
        generation !== this.generation ||
        !this.bindingActive(sessionId) ||
        !shellId ||
        this.status.peek() === 'exited'
      )
        return;
      void this.endpoints.shellResize(sessionId, shellId, cols, rows).catch(() => {
        // Stream reconnection or a replacement generation will reconcile size.
      });
    }, 80);
  }

  async close(): Promise<void> {
    if (!this.bindingActive()) {
      this.visible.value = false;
      this.resetBinding('');
      return;
    }
    const sessionId = this.sessionId.peek();
    const shellId = this.shellId.peek();
    this.visible.value = false;
    this.resetBinding('');
    const closeGeneration = this.generation;
    if (!sessionId || !shellId) return;
    try {
      await this.endpoints.shellClose(sessionId, shellId);
    } catch (error) {
      if (closeGeneration === this.generation) this.toast(error, 'error');
    }
  }

  detach(): void {
    this.invalidateGeneration();
  }

  dispose(): void {
    this.detach();
  }

  private invalidateGeneration(): number {
    this.generation += 1;
    this.streamAbort?.abort();
    this.streamAbort = null;
    clearTimeout(this.inputTimer);
    clearTimeout(this.resizeTimer);
    this.inputTimer = 0;
    this.resizeTimer = 0;
    this.inputChunks = [];
    this.inputChain = Promise.resolve();
    return this.generation;
  }

  private bindingActive(sessionId = this.sessionId.peek()): boolean {
    return Boolean(
      sessionId &&
      this.visible.peek() &&
      sessionId === this.sessionId.peek() &&
      sessionId === this.activeSessionId(),
    );
  }

  private inputCurrent(generation: number, sessionId: string, shellId: string): boolean {
    return (
      generation === this.generation &&
      shellId !== '' &&
      shellId === this.shellId.peek() &&
      this.status.peek() === 'running' &&
      this.bindingActive(sessionId)
    );
  }

  private current(generation: number, sessionId: string): boolean {
    return generation === this.generation && this.bindingActive(sessionId);
  }

  private resetBinding(sessionId: string): void {
    this.detach();
    this.sessionId.value = sessionId;
    this.shellId.value = '';
    this.cwd.value = '';
    this.offset.value = 0;
    this.exitCode.value = null;
    this.error.value = '';
    this.status.value = 'idle';
  }
}

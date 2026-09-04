import { signal, type Signal } from '@preact/signals';
import { APIError, decodeSSE } from '../api/client';
import type {
  Endpoints,
  ShellCollaborationSnapshot,
  ShellCollaborationState,
} from '../api/endpoints';

export type ShellStatus = 'idle' | 'connecting' | 'running' | 'reconnecting' | 'exited' | 'error';
export type ShellLayout = 'fullscreen' | 'bottom' | 'right';

interface ShellLayoutPreference {
  mode: ShellLayout;
  bottom: number;
  right: number;
}

const defaultShellLayout: ShellLayoutPreference = { mode: 'fullscreen', bottom: 360, right: 520 };

function readShellLayout(storage: Storage, key: string): ShellLayoutPreference {
  try {
    const raw = JSON.parse(storage.getItem(key) || 'null') as Partial<ShellLayoutPreference> | null;
    const mode = raw?.mode;
    return {
      mode: mode === 'bottom' || mode === 'right' || mode === 'fullscreen' ? mode : 'fullscreen',
      bottom: Math.min(1400, Math.max(220, Number(raw?.bottom) || defaultShellLayout.bottom)),
      right: Math.min(1400, Math.max(320, Number(raw?.right) || defaultShellLayout.right)),
    };
  } catch {
    return { ...defaultShellLayout };
  }
}

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
  sequence?: number;
  revision?: number;
  enabled?: boolean;
  state?: ShellCollaborationState;
  command_id?: string;
  tool_call_id?: string;
  reason?: string;
  collaboration?: ShellCollaborationSnapshot;
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
  readonly collaborationSupported = signal(false);
  readonly shellToolAvailable = signal(false);
  readonly collaborationEnabled = signal(false);
  readonly collaborationState: Signal<ShellCollaborationState> = signal('off');
  readonly collaborationRevision = signal(0);
  readonly collaborationSequence = signal(0);
  readonly collaborationReason = signal('');
  readonly activeCommandId = signal('');
  readonly activeToolCallId = signal('');
  readonly collaborationPending: Signal<'enabling' | 'disabling' | 'interrupting' | null> =
    signal(null);
  readonly layout: Signal<ShellLayout>;
  readonly dockBottomSize: Signal<number>;
  readonly dockRightSize: Signal<number>;

  private readonly layoutStorage: Storage;
  private readonly layoutStorageKey: string;
  private eventCursor = 0;
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
    storage: Storage = localStorage,
    layoutStorageKey = 'term_llm_shell_layout',
  ) {
    this.layoutStorage = storage;
    this.layoutStorageKey = layoutStorageKey;
    const preference = readShellLayout(storage, layoutStorageKey);
    this.layout = signal(preference.mode);
    this.dockBottomSize = signal(preference.bottom);
    this.dockRightSize = signal(preference.right);
  }

  setLayout(mode: ShellLayout): void {
    if (this.layout.peek() === mode) return;
    this.layout.value = mode;
    this.persistLayout();
  }

  setDockSize(mode: 'bottom' | 'right', size: number, persist = true): void {
    const minimum = mode === 'bottom' ? 220 : 320;
    const next = Math.max(minimum, Math.min(1400, Math.round(size)));
    const target = mode === 'bottom' ? this.dockBottomSize : this.dockRightSize;
    if (target.peek() === next) {
      if (persist) this.persistLayout();
      return;
    }
    target.value = next;
    if (persist) this.persistLayout();
  }

  private persistLayout(): void {
    try {
      this.layoutStorage.setItem(
        this.layoutStorageKey,
        JSON.stringify({
          mode: this.layout.peek(),
          bottom: this.dockBottomSize.peek(),
          right: this.dockRightSize.peek(),
        } satisfies ShellLayoutPreference),
      );
    } catch {
      // Docking remains usable when storage is unavailable or full.
    }
  }

  show(sessionId = ''): boolean {
    if (!this.enabled.peek()) {
      this.toast('Interactive shell is unavailable on this server.', 'error');
      return false;
    }
    const activeSessionId = this.activeSessionId();
    if ((sessionId && sessionId !== activeSessionId) || (!sessionId && activeSessionId))
      return false;
    if (this.sessionId.peek() !== sessionId) this.resetBinding(sessionId);
    this.visible.value = true;
    return true;
  }

  bind(sessionId: string): boolean {
    if (!this.visible.peek() || !sessionId || sessionId !== this.activeSessionId()) return false;
    if (this.sessionId.peek() !== sessionId) this.resetBinding(sessionId);
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
      if (this.shellId.peek() !== created.shell_id) this.resetCollaborationGeneration();
      this.shellId.value = created.shell_id;
      this.cwd.value = created.cwd;
      this.applyCollaborationSnapshot(
        created.collaboration,
        generation,
        sessionId,
        created.shell_id,
      );
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
            this.resetCollaborationGeneration();
            this.shellId.value = attached.shell_id;
            this.cwd.value = attached.cwd;
            this.offset.value = 0;
            sink.reset();
          }
          this.applyCollaborationSnapshot(
            attached.collaboration,
            generation,
            sessionId,
            attached.shell_id,
          );
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
          if (data.shell_id && data.shell_id !== this.shellId.peek()) continue;
          if (message.event === 'ready') {
            if (data.collaboration)
              this.applyCollaborationSnapshot(
                data.collaboration,
                generation,
                sessionId,
                data.shell_id || this.shellId.peek(),
                true,
              );
            this.eventCursor = Math.max(this.eventCursor, Number(data.sequence) || 0);
            continue;
          }
          if (
            message.event === 'collaboration' ||
            message.event === 'agent_command_started' ||
            message.event === 'agent_command_finished' ||
            message.event === 'collaboration_desynchronized'
          ) {
            this.applyCollaborationEvent(message.event, data, generation, sessionId);
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
            if (data.collaboration)
              this.applyCollaborationSnapshot(
                data.collaboration,
                generation,
                sessionId,
                data.shell_id || this.shellId.peek(),
                true,
              );
            this.offset.value = Math.max(this.offset.peek(), Number(data.offset) || 0);
            this.exitCode.value = Number.isFinite(data.exit_code) ? Number(data.exit_code) : -1;
            this.clearCollaborationAuthority();
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

  async enableCollaboration(): Promise<void> {
    if (!this.bindingActive() || !this.shellId.peek() || this.collaborationPending.peek()) return;
    const generation = this.generation;
    const sessionId = this.sessionId.peek();
    const shellId = this.shellId.peek();
    this.collaborationPending.value = 'enabling';
    this.error.value = '';
    try {
      const snapshot = await this.endpoints.shellCollaboration(sessionId, shellId, true);
      this.applyCollaborationSnapshot(snapshot, generation, sessionId, shellId, true);
    } catch (error) {
      if (this.current(generation, sessionId)) {
        this.applyCollaborationError(error, generation, sessionId, shellId);
        this.error.value = error instanceof Error ? error.message : 'Could not share the shell.';
        this.toast(error, 'error');
      }
    } finally {
      if (this.current(generation, sessionId)) this.collaborationPending.value = null;
    }
  }

  async disableCollaboration(): Promise<void> {
    if (!this.bindingActive() || !this.shellId.peek() || this.collaborationPending.peek()) return;
    const generation = this.generation;
    const sessionId = this.sessionId.peek();
    const shellId = this.shellId.peek();
    this.collaborationPending.value = 'disabling';
    try {
      const snapshot = await this.endpoints.shellCollaboration(sessionId, shellId, false);
      this.applyCollaborationSnapshot(snapshot, generation, sessionId, shellId, true);
    } catch (error) {
      if (this.current(generation, sessionId)) {
        this.applyCollaborationError(error, generation, sessionId, shellId);
        this.toast(error, 'error');
      }
    } finally {
      if (this.current(generation, sessionId)) this.collaborationPending.value = null;
    }
  }

  async interruptCommand(): Promise<void> {
    const commandId = this.activeCommandId.peek();
    if (
      !this.bindingActive() ||
      !this.shellId.peek() ||
      !commandId ||
      this.collaborationPending.peek()
    )
      return;
    const generation = this.generation;
    const sessionId = this.sessionId.peek();
    const shellId = this.shellId.peek();
    this.collaborationPending.value = 'interrupting';
    try {
      const snapshot = await this.endpoints.shellInterrupt(sessionId, shellId, commandId);
      this.applyCollaborationSnapshot(snapshot, generation, sessionId, shellId, true);
    } catch (error) {
      if (this.current(generation, sessionId)) {
        this.applyCollaborationError(error, generation, sessionId, shellId);
        this.toast(error, 'error');
      }
    } finally {
      if (this.current(generation, sessionId)) this.collaborationPending.value = null;
    }
  }

  private applyCollaborationError(
    error: unknown,
    generation: number,
    sessionId: string,
    shellId: string,
  ): void {
    if (!(error instanceof APIError) || !error.body) return;
    try {
      const payload = JSON.parse(error.body) as { collaboration?: ShellCollaborationSnapshot };
      this.applyCollaborationSnapshot(payload.collaboration, generation, sessionId, shellId);
    } catch {
      // The ordinary transport error remains authoritative when no snapshot can be decoded.
    }
  }

  private resetCollaborationGeneration(): void {
    this.collaborationSupported.value = false;
    this.shellToolAvailable.value = false;
    this.collaborationEnabled.value = false;
    this.collaborationState.value = 'off';
    this.collaborationRevision.value = 0;
    this.collaborationSequence.value = 0;
    this.collaborationReason.value = '';
    this.activeCommandId.value = '';
    this.activeToolCallId.value = '';
    this.collaborationPending.value = null;
    this.eventCursor = 0;
  }

  private applyCollaborationSnapshot(
    snapshot: ShellCollaborationSnapshot | undefined,
    generation: number,
    sessionId: string,
    shellId: string,
    _authoritative = false,
  ): void {
    if (!snapshot || !this.current(generation, sessionId)) return;
    if (shellId !== this.shellId.peek() || snapshot.shell_id !== shellId) return;
    const revision = Math.max(0, Number(snapshot.revision) || 0);
    const sequence = Math.max(0, Number(snapshot.sequence) || 0);
    const currentRevision = this.collaborationRevision.peek();
    if (revision < currentRevision) return;
    if (revision === currentRevision && sequence < this.collaborationSequence.peek()) return;
    this.collaborationSupported.value = snapshot.supported === true;
    this.shellToolAvailable.value = snapshot.shell_tool_available === true;
    this.collaborationEnabled.value = snapshot.enabled === true;
    this.collaborationState.value = snapshot.state || 'off';
    this.collaborationRevision.value = revision;
    this.collaborationSequence.value = sequence;
    this.eventCursor = Math.max(this.eventCursor, this.collaborationSequence.peek());
    this.collaborationReason.value = snapshot.reason || '';
    this.activeCommandId.value = snapshot.command_id || '';
    this.activeToolCallId.value = snapshot.tool_call_id || '';
  }

  private applyCollaborationEvent(
    event: string,
    data: ShellEventData,
    generation: number,
    sessionId: string,
  ): void {
    if (!this.current(generation, sessionId) || data.shell_id !== this.shellId.peek()) return;
    const snapshot =
      data.collaboration ||
      (event === 'collaboration'
        ? ({ ...data, shell_id: data.shell_id } as ShellCollaborationSnapshot)
        : undefined);
    const revision = Math.max(0, Number(snapshot?.revision ?? data.revision) || 0);
    if (revision < this.collaborationRevision.peek()) return;
    const sequence = Math.max(0, Number(data.sequence ?? snapshot?.sequence) || 0);
    if (sequence && sequence < this.eventCursor) return;
    if (sequence && sequence === this.eventCursor && !snapshot) return;
    if (sequence) this.eventCursor = sequence;
    if (snapshot) {
      this.applyCollaborationSnapshot(snapshot, generation, sessionId, this.shellId.peek());
      return;
    }
    this.collaborationRevision.value = revision;
    this.collaborationSequence.value = Math.max(this.collaborationSequence.peek(), sequence);
    if (event === 'agent_command_started') {
      this.collaborationEnabled.value = true;
      this.collaborationState.value = 'agent_running';
      this.activeCommandId.value = data.command_id || '';
      this.activeToolCallId.value = data.tool_call_id || '';
    } else if (event === 'agent_command_finished') {
      this.collaborationEnabled.value = data.enabled === true;
      this.collaborationState.value = data.state || (data.enabled ? 'ready' : 'off');
      this.collaborationReason.value = data.reason || '';
      this.activeCommandId.value = '';
      this.activeToolCallId.value = '';
    } else if (event === 'collaboration_desynchronized') {
      this.collaborationEnabled.value = true;
      this.collaborationState.value = 'desynchronized';
      this.collaborationReason.value = data.reason || 'Shared shell synchronization was lost.';
      this.activeCommandId.value = '';
      this.activeToolCallId.value = '';
    }
  }

  private clearCollaborationAuthority(): void {
    this.collaborationEnabled.value = false;
    this.collaborationState.value = 'off';
    this.activeCommandId.value = '';
    this.activeToolCallId.value = '';
    this.collaborationPending.value = null;
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
          // Input rejection does not invalidate the independently supervised SSE
          // transport. Keep authoritative collaboration controls available (most
          // importantly interrupt/stop-sharing) and report the rejected bytes
          // without turning a live terminal into a terminal-level error state.
          this.error.value = error instanceof Error ? error.message : 'Shell input failed.';
          this.toast(error, 'error');
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
    this.collaborationPending.value = null;
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
    this.resetCollaborationGeneration();
  }
}

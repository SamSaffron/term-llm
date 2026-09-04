import type { JSX } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import '../styles/features/shell-terminal.css';
import type { AppStore } from '../stores/app-store';
import type { ShellLayout, ShellSink } from '../stores/shell-store';
import { Icon } from './Icon';
import { useMediaQuery } from './useMediaQuery';

const shellLayouts: Array<{
  mode: ShellLayout;
  label: string;
  title: string;
  icon: 'expand' | 'dock-bottom' | 'dock-right';
}> = [
  { mode: 'fullscreen', label: 'Full screen', title: 'Use full-screen terminal', icon: 'expand' },
  { mode: 'bottom', label: 'Dock bottom', title: 'Dock terminal below chat', icon: 'dock-bottom' },
  { mode: 'right', label: 'Dock right', title: 'Dock terminal beside chat', icon: 'dock-right' },
];

const dockMinimum = (mode: 'bottom' | 'right') => (mode === 'bottom' ? 220 : 320);
const dockMaximum = (mode: 'bottom' | 'right') =>
  Math.round(
    Math.min(
      1400,
      Math.max(
        dockMinimum(mode),
        window[mode === 'bottom' ? 'innerHeight' : 'innerWidth'] *
          (mode === 'bottom' ? 0.75 : 0.72),
      ),
    ),
  );
const clampDockSize = (mode: 'bottom' | 'right', size: number) =>
  Math.max(dockMinimum(mode), Math.min(dockMaximum(mode), Math.round(size)));

const statusLabel = (
  status: AppStore['shellStore']['status']['value'],
  exitCode: number | null,
) => {
  switch (status) {
    case 'idle':
    case 'connecting':
      return 'Opening…';
    case 'reconnecting':
      return 'Reconnecting…';
    case 'exited':
      return `Exited${exitCode === null ? '' : ` (${exitCode})`}`;
    case 'error':
      return 'Connection error';
    default:
      return 'Running';
  }
};

const shellBindingActive = (store: AppStore, sessionId: string) =>
  Boolean(
    sessionId &&
    store.shellStore.visible.peek() &&
    store.shellStore.sessionId.peek() === sessionId &&
    store.activeSession.peek()?.id === sessionId,
  );

export function ShellOverlay({ store }: { store: AppStore }) {
  const host = useRef<HTMLDivElement>(null);
  const terminal = useRef<Terminal | null>(null);
  const fitAddon = useRef<FitAddon | null>(null);
  const sink = useRef<ShellSink | null>(null);
  const dragCleanup = useRef<(() => void) | null>(null);
  const [closing, setClosing] = useState(false);
  const [sharePending, setSharePending] = useState(false);
  const sideDockFallsBelow = useMediaQuery('(width <= 760px)');
  const layout = store.shellStore.layout.value;
  const effectiveLayout = layout === 'right' && sideDockFallsBelow ? 'bottom' : layout;
  const boundSessionId = store.shellStore.sessionId.value;
  const activeSessionId = store.activeSession.value?.id || '';
  const shellVisible = store.shellStore.visible.value;
  const pendingBinding = shellVisible && !boundSessionId;
  const matchesActiveSession = Boolean(
    shellVisible && (pendingBinding || boundSessionId === activeSessionId),
  );

  useEffect(() => {
    if (!matchesActiveSession) store.shellStore.back();
  }, [activeSessionId, boundSessionId, matchesActiveSession, store]);

  useEffect(
    () => () => {
      dragCleanup.current?.();
    },
    [],
  );

  useEffect(() => {
    if (!matchesActiveSession || !boundSessionId) return;
    const target = host.current;
    if (!target) return;
    const instance = new Terminal({
      allowProposedApi: false,
      convertEol: false,
      cursorBlink: true,
      cursorStyle: 'bar',
      fontFamily: '"SFMono-Regular", Consolas, "Liberation Mono", monospace',
      fontSize: 14,
      lineHeight: 1.15,
      scrollback: 10_000,
      theme: {
        background: '#111418',
        foreground: '#dce2e8',
        cursor: '#eef2f5',
        cursorAccent: '#111418',
        selectionBackground: '#42546699',
        black: '#171b20',
        red: '#e06c75',
        green: '#98c379',
        yellow: '#e5c07b',
        blue: '#61afef',
        magenta: '#c678dd',
        cyan: '#56b6c2',
        white: '#d7dae0',
        brightBlack: '#5c6370',
        brightWhite: '#ffffff',
      },
    });
    const fit = new FitAddon();
    instance.loadAddon(fit);
    instance.parser?.registerOscHandler(7770, () => true);
    instance.open(target);
    terminal.current = instance;
    fitAddon.current = fit;
    sink.current = {
      write: (data) => instance.write(data),
      reset: () => instance.reset(),
    };
    const resize = () => {
      if (!shellBindingActive(store, boundSessionId)) return;
      try {
        fit.fit();
        if (instance.cols > 1 && instance.rows > 0)
          store.shellStore.resize(instance.cols, instance.rows);
      } catch {
        // The overlay may be between layout and teardown.
      }
    };
    const frame = requestAnimationFrame(() => {
      if (!shellBindingActive(store, boundSessionId)) return;
      resize();
      instance.focus();
      void store.shellStore.connect(instance.cols, instance.rows, sink.current!);
    });
    const input = instance.onData((data) => {
      if (shellBindingActive(store, boundSessionId)) store.shellStore.input(data);
    });
    const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(resize);
    observer?.observe(target);
    addEventListener('resize', resize);
    return () => {
      cancelAnimationFrame(frame);
      removeEventListener('resize', resize);
      observer?.disconnect();
      input.dispose();
      store.shellStore.detach();
      instance.dispose();
      terminal.current = null;
      fitAddon.current = null;
      sink.current = null;
    };
  }, [activeSessionId, boundSessionId, matchesActiveSession, store]);

  const restart = () => {
    if (!shellBindingActive(store, boundSessionId)) return;
    const instance = terminal.current;
    const output = sink.current;
    if (!instance || !output) return;
    void store.shellStore
      .restart(instance.cols, instance.rows, output)
      .then(() => instance.focus());
  };
  const close = async () => {
    if (!pendingBinding && !shellBindingActive(store, boundSessionId)) return;
    setClosing(true);
    await store.shellStore.close();
  };
  const refitTerminal = () => {
    requestAnimationFrame(() => {
      const instance = terminal.current;
      try {
        fitAddon.current?.fit();
        if (
          instance &&
          shellBindingActive(store, boundSessionId) &&
          instance.cols > 1 &&
          instance.rows > 0
        )
          store.shellStore.resize(instance.cols, instance.rows);
      } catch {
        // Layout can change while the lazy terminal is being detached.
      }
    });
  };
  const selectLayout = (mode: ShellLayout) => {
    store.shellStore.setLayout(mode);
    refitTerminal();
  };
  const resizeDock = (
    mode: 'bottom' | 'right',
    clientX: number,
    clientY: number,
    grabOffset = 0,
    persist = true,
  ) => {
    const size =
      mode === 'bottom'
        ? window.innerHeight - clientY + grabOffset
        : window.innerWidth - clientX + grabOffset;
    store.shellStore.setDockSize(mode, clampDockSize(mode, size), persist);
  };
  const startDockResize = (event: JSX.TargetedPointerEvent<HTMLDivElement>) => {
    if (effectiveLayout === 'fullscreen') return;
    event.preventDefault();
    event.currentTarget.setPointerCapture?.(event.pointerId);
    dragCleanup.current?.();
    const resizeMode = effectiveLayout;
    const currentSize = clampDockSize(
      resizeMode,
      resizeMode === 'bottom'
        ? store.shellStore.dockBottomSize.peek()
        : store.shellStore.dockRightSize.peek(),
    );
    const edge =
      resizeMode === 'bottom' ? window.innerHeight - currentSize : window.innerWidth - currentSize;
    const grabOffset = (resizeMode === 'bottom' ? event.clientY : event.clientX) - edge;
    const move = (moveEvent: PointerEvent) =>
      resizeDock(resizeMode, moveEvent.clientX, moveEvent.clientY, grabOffset, false);
    const cleanup = () => {
      removeEventListener('pointermove', move);
      removeEventListener('pointerup', finish);
      removeEventListener('pointercancel', finish);
      if (dragCleanup.current === cleanup) dragCleanup.current = null;
    };
    const finish = () => {
      cleanup();
      const finalSize =
        resizeMode === 'bottom'
          ? store.shellStore.dockBottomSize.peek()
          : store.shellStore.dockRightSize.peek();
      store.shellStore.setDockSize(resizeMode, finalSize);
      refitTerminal();
    };
    dragCleanup.current = cleanup;
    addEventListener('pointermove', move);
    addEventListener('pointerup', finish, { once: true });
    addEventListener('pointercancel', finish, { once: true });
  };
  const resizeDockWithKeyboard = (event: JSX.TargetedKeyboardEvent<HTMLDivElement>) => {
    if (effectiveLayout === 'fullscreen') return;
    const mode = effectiveLayout;
    const step = event.shiftKey ? 64 : 24;
    const current = clampDockSize(
      mode,
      mode === 'bottom'
        ? store.shellStore.dockBottomSize.peek()
        : store.shellStore.dockRightSize.peek(),
    );
    let next: number | null = null;
    if (event.key === 'Home') next = dockMinimum(mode);
    if (event.key === 'End') next = dockMaximum(mode);
    if (mode === 'bottom') {
      if (event.key === 'ArrowUp') next = current + step;
      if (event.key === 'ArrowDown') next = current - step;
    } else {
      if (event.key === 'ArrowLeft') next = current + step;
      if (event.key === 'ArrowRight') next = current - step;
    }
    if (next !== null) {
      event.preventDefault();
      store.shellStore.setDockSize(mode, clampDockSize(mode, next));
      refitTerminal();
    }
  };
  const status = store.shellStore.status.value;
  const collaborationState = store.shellStore.collaborationState.value;
  const collaborationPending = store.shellStore.collaborationPending.value;
  const requestSharing = async () => {
    if (sharePending || collaborationPending) return;
    if (boundSessionId) {
      await store.shellStore.enableCollaboration();
      return;
    }
    setSharePending(true);
    const sessionId = await store.ensureShellSession();
    if (!sessionId || !store.shellStore.bind(sessionId)) setSharePending(false);
  };
  useEffect(() => {
    if (!sharePending || !boundSessionId) return;
    if (status === 'error' || status === 'exited') {
      setSharePending(false);
      return;
    }
    if (status !== 'running') return;
    setSharePending(false);
    void store.shellStore.enableCollaboration();
  }, [boundSessionId, sharePending, status, store]);
  const canEnableCollaboration =
    matchesActiveSession &&
    (pendingBinding ||
      (status === 'running' &&
        store.shellStore.collaborationSupported.value &&
        store.shellStore.shellToolAvailable.value)) &&
    !store.streaming.value &&
    !store.runActive.value &&
    !collaborationPending &&
    !sharePending;
  const stopSharing = () => void store.shellStore.disableCollaboration();
  const interruptCommand = () => void store.shellStore.interruptCommand();
  const dockMode = effectiveLayout === 'fullscreen' ? null : effectiveLayout;
  const dockMax = dockMode ? dockMaximum(dockMode) : 0;
  const dockValue = dockMode
    ? Math.min(
        dockMax,
        dockMode === 'bottom'
          ? store.shellStore.dockBottomSize.value
          : store.shellStore.dockRightSize.value,
      )
    : 0;
  return (
    <section
      class={`shell-overlay shell-layout-${layout} shell-effective-layout-${effectiveLayout}`}
      style={{
        '--shell-dock-bottom-size': `${store.shellStore.dockBottomSize.value}px`,
        '--shell-dock-right-size': `${store.shellStore.dockRightSize.value}px`,
      }}
      aria-label="Interactive shell"
    >
      {effectiveLayout !== 'fullscreen' && (
        <div
          class={`shell-dock-resizer shell-dock-resizer-${effectiveLayout}`}
          role="separator"
          tabIndex={0}
          aria-label={`Resize ${effectiveLayout === 'bottom' ? 'bottom' : 'right'} terminal dock`}
          aria-orientation={effectiveLayout === 'bottom' ? 'horizontal' : 'vertical'}
          aria-valuemin={dockMode ? dockMinimum(dockMode) : undefined}
          aria-valuemax={dockMax}
          aria-valuenow={dockValue}
          onPointerDown={startDockResize}
          onKeyDown={resizeDockWithKeyboard}
        />
      )}
      <header class="shell-overlay-header">
        <div class="shell-overlay-heading">
          <span class="shell-prompt-mark" aria-hidden="true">
            &gt;_
          </span>
          <div class="shell-title-block">
            <div class="shell-title-row">
              <h1>Shell</h1>
              <span
                class={`shell-status shell-status-${status}`}
                role="status"
                title={
                  status === 'running' ? 'Shell stays running when you return to chat' : undefined
                }
              >
                <span class="shell-status-dot" aria-hidden="true" />
                {pendingBinding
                  ? sharePending
                    ? 'Creating session…'
                    : 'Waiting for conversation'
                  : statusLabel(status, store.shellStore.exitCode.value)}
              </span>
            </div>
            {store.shellStore.cwd.value && (
              <span class="shell-cwd" title={store.shellStore.cwd.value}>
                {store.shellStore.cwd.value}
              </span>
            )}
          </div>
        </div>
        <div class="shell-overlay-actions">
          <div class="shell-layout-picker" role="group" aria-label="Terminal layout">
            {shellLayouts.map((option) => (
              <button
                key={option.mode}
                class={`shell-layout-button ${layout === option.mode ? 'is-active' : ''}`}
                type="button"
                aria-label={option.label}
                aria-pressed={layout === option.mode}
                title={option.title}
                onClick={() => selectLayout(option.mode)}
              >
                <Icon name={option.icon} />
              </button>
            ))}
          </div>
          {(pendingBinding || status === 'running') && collaborationState === 'off' && (
            <button
              class="btn shell-share"
              type="button"
              disabled={!canEnableCollaboration}
              onClick={() => void requestSharing()}
            >
              {sharePending
                ? 'Creating session…'
                : collaborationPending === 'enabling'
                  ? 'Checking shell…'
                  : 'Share with agent'}
            </button>
          )}
          {status === 'running' && collaborationState === 'ready' && (
            <>
              <span class="shell-collaboration-badge shell-collaboration-ready">
                Shared with agent
              </span>
              <button
                class="btn"
                type="button"
                onClick={stopSharing}
                disabled={Boolean(collaborationPending) || !matchesActiveSession}
              >
                {collaborationPending === 'disabling' ? 'Stopping…' : 'Stop sharing'}
              </button>
            </>
          )}
          {status === 'running' && collaborationState === 'agent_running' && (
            <>
              <span class="shell-collaboration-badge shell-collaboration-running">
                Agent running
              </span>
              <button
                class="btn shell-interrupt"
                type="button"
                onClick={interruptCommand}
                disabled={Boolean(collaborationPending) || !matchesActiveSession}
              >
                {collaborationPending === 'interrupting'
                  ? 'Interrupting…'
                  : 'Interrupt agent command'}
              </button>
              <button
                class="btn"
                type="button"
                onClick={stopSharing}
                disabled={Boolean(collaborationPending) || !matchesActiveSession}
              >
                Stop sharing
              </button>
            </>
          )}
          {status === 'running' && collaborationState === 'desynchronized' && (
            <>
              <span class="shell-collaboration-badge shell-collaboration-attention">
                Shared shell needs attention
              </span>
              <button
                class="btn"
                type="button"
                onClick={stopSharing}
                disabled={Boolean(collaborationPending) || !matchesActiveSession}
              >
                Stop sharing
              </button>
            </>
          )}
          {status === 'exited' && (
            <button
              class="btn shell-restart"
              type="button"
              disabled={!matchesActiveSession}
              onClick={restart}
            >
              Restart
            </button>
          )}
          <button
            class="btn shell-back"
            type="button"
            title={
              layout === 'fullscreen'
                ? 'Return to chat; this shell will keep running'
                : 'Hide terminal; this shell will keep running'
            }
            onClick={() => store.shellStore.back()}
          >
            {layout === 'fullscreen' ? <Icon name="arrow-left" /> : null}
            <span>{layout === 'fullscreen' ? 'Back to chat' : 'Hide terminal'}</span>
          </button>
          <button
            class="btn shell-end"
            type="button"
            disabled={closing || !matchesActiveSession}
            onClick={close}
          >
            {closing ? 'Ending…' : 'End shell'}
          </button>
        </div>
      </header>
      {collaborationState === 'agent_running' && (
        <div class="shell-command-notice" role="status">
          Typing in this terminal interacts directly with the running agent command.
        </div>
      )}
      {collaborationState === 'desynchronized' && (
        <div class="shell-collaboration-error" role="alert">
          {store.shellStore.collaborationReason.value ||
            'Return the terminal to a shell prompt, stop sharing, then share it again.'}
        </div>
      )}
      {store.shellStore.error.value && (
        <div class="shell-error" role="alert">
          {store.shellStore.error.value}
        </div>
      )}
      {pendingBinding ? (
        <div
          class="shell-terminal shell-terminal-pending"
          aria-label="Terminal waiting for conversation"
        >
          <span>The shell will connect when this conversation gets a session.</span>
        </div>
      ) : (
        <div class="shell-terminal" ref={host} aria-label="Terminal output" />
      )}
    </section>
  );
}

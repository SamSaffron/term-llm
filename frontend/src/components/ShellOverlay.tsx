import { useEffect, useRef, useState } from 'preact/hooks';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import '../styles/features/shell-terminal.css';
import type { AppStore } from '../stores/app-store';
import type { ShellSink } from '../stores/shell-store';
import { Icon } from './Icon';

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
  const [closing, setClosing] = useState(false);
  const boundSessionId = store.shellStore.sessionId.value;
  const activeSessionId = store.activeSession.value?.id || '';
  const matchesActiveSession = Boolean(
    boundSessionId && boundSessionId === activeSessionId && store.shellStore.visible.value,
  );

  useEffect(() => {
    if (!matchesActiveSession) store.shellStore.back();
  }, [activeSessionId, boundSessionId, matchesActiveSession, store]);

  useEffect(() => {
    if (!matchesActiveSession) return;
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
    if (!shellBindingActive(store, boundSessionId)) return;
    setClosing(true);
    await store.shellStore.close();
  };
  const status = store.shellStore.status.value;
  return (
    <section class="shell-overlay" aria-label="Interactive shell">
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
                {statusLabel(status, store.shellStore.exitCode.value)}
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
            title="Return to chat; this shell will keep running"
            onClick={() => store.shellStore.back()}
          >
            <Icon name="arrow-left" />
            <span>Back to chat</span>
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
      {store.shellStore.error.value && (
        <div class="shell-error" role="alert">
          {store.shellStore.error.value}
        </div>
      )}
      <div class="shell-terminal" ref={host} aria-label="Terminal output" />
    </section>
  );
}

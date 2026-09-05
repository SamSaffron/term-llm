import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppStore } from '../stores/app-store';
import { ShellOverlay } from './ShellOverlay';

const registerOscHandler = vi.hoisted(() => vi.fn());
const terminalOptions = vi.hoisted(() => vi.fn());
const terminalInput = vi.hoisted(() => ({ handler: null as ((data: string) => void) | null }));

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    constructor(options: unknown) {
      terminalOptions(options);
    }
    cols = 80;
    rows = 24;
    parser = { registerOscHandler };
    loadAddon = vi.fn();
    open = vi.fn();
    focus = vi.fn();
    write = vi.fn();
    reset = vi.fn();
    dispose = vi.fn();
    onData = vi.fn((handler: (data: string) => void) => {
      terminalInput.handler = handler;
      return { dispose: vi.fn() };
    });
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));

const config = {
  prefix: '/ui',
  version: 'test',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: [],
  title: 'term-llm',
  locationSharing: true,
  worktrees: false,
  hub: null,
  vapidKey: '',
  pushSupported: false,
  webRTC: false,
  signalingURL: '',
};

const stores: AppStore[] = [];
afterEach(() => {
  terminalInput.handler = null;
  for (const store of stores.splice(0)) store.dispose();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function shellStore(): AppStore {
  const store = new AppStore(config);
  stores.push(store);
  store.shellStore.enabled.value = true;
  store.sessions.value = [
    {
      id: 'session-one',
      title: 'First',
      name: '',
      mode: 'chat',
      origin: 'web',
      archived: false,
      pinned: false,
      created: 1,
      lastMessageAt: 1,
      messages: [],
    },
  ];
  store.activeSessionId.value = 'session-one';
  store.draftActive.value = false;
  store.shellStore.visible.value = true;
  store.shellStore.sessionId.value = 'session-one';
  store.shellStore.cwd.value = '/workspace/project';
  store.shellStore.status.value = 'running';
  store.shellStore.connect = vi.fn(async () => undefined);
  store.shellStore.resize = vi.fn();
  store.shellStore.detach = vi.fn();
  return store;
}

describe('ShellOverlay', () => {
  it('uses the shared CSS code font for the terminal renderer', () => {
    const store = shellStore();
    vi.spyOn(window, 'getComputedStyle').mockReturnValue({
      getPropertyValue: (name: string) => (name === '--font-mono' ? 'Test Mono, monospace' : ''),
    } as CSSStyleDeclaration);
    render(<ShellOverlay store={store} />);
    expect(terminalOptions).toHaveBeenLastCalledWith(
      expect.objectContaining({ fontFamily: 'Test Mono, monospace' }),
    );
  });

  it('keeps a draft shell open and creates a blank session when sharing is requested', async () => {
    const store = shellStore();
    store.sessions.value = [];
    store.activeSessionId.value = '';
    store.draftActive.value = true;
    store.shellStore.sessionId.value = '';
    store.shellStore.status.value = 'idle';
    store.ensureSession = vi.fn(async () => {
      store.activeSessionId.value = 'session-created';
      store.draftActive.value = false;
      return 'session-created';
    });
    store.shellStore.bind = vi.fn((sessionId: string) => {
      store.shellStore.sessionId.value = sessionId;
      store.shellStore.status.value = 'running';
      store.shellStore.collaborationSupported.value = true;
      store.shellStore.shellToolAvailable.value = true;
      return true;
    });
    store.shellStore.enableCollaboration = vi.fn(async () => undefined);

    render(<ShellOverlay store={store} />);
    expect(screen.getByText('Waiting for conversation')).toBeVisible();
    expect(screen.getByLabelText('Terminal waiting for conversation')).toBeVisible();

    await userEvent.click(screen.getByRole('button', { name: 'Share with agent' }));
    await waitFor(() => expect(store.ensureSession).toHaveBeenCalledOnce());
    expect(store.shellStore.bind).toHaveBeenCalledWith('session-created');
    await waitFor(() => expect(store.shellStore.enableCollaboration).toHaveBeenCalledOnce());
  });

  it('presents visible chat/close controls and does not steal Escape', async () => {
    const store = shellStore();
    store.shellStore.back = vi.fn();
    store.shellStore.close = vi.fn(async () => undefined);
    render(<ShellOverlay store={store} />);

    expect(registerOscHandler).toHaveBeenCalledWith(7770, expect.any(Function));
    expect(registerOscHandler.mock.calls.at(-1)?.[1]('P;nonce')).toBe(true);
    expect(screen.getByText('/workspace/project')).toBeVisible();
    expect(screen.getByText('Running')).toBeVisible();
    expect(screen.getByRole('heading', { name: 'Shell' })).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Back to chat' }));
    expect(store.shellStore.back).toHaveBeenCalledOnce();

    let prevented = false;
    const observe = (event: KeyboardEvent) => {
      prevented = event.defaultPrevented;
    };
    window.addEventListener('keydown', observe);
    fireEvent.keyDown(window, { key: 'Escape' });
    window.removeEventListener('keydown', observe);
    expect(prevented).toBe(false);

    await userEvent.click(screen.getByRole('button', { name: 'End shell' }));
    expect(store.shellStore.close).toHaveBeenCalledOnce();
  });

  it('switches between full-screen, bottom, and side docking with an accessible resizer', async () => {
    const store = shellStore();
    render(<ShellOverlay store={store} />);
    const shell = screen.getByLabelText('Interactive shell');

    expect(shell).toHaveClass('shell-layout-fullscreen');
    expect(screen.getByRole('button', { name: 'Full screen' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    await userEvent.click(screen.getByRole('button', { name: 'Dock bottom' }));
    expect(shell).toHaveClass('shell-layout-bottom');
    expect(screen.getByRole('button', { name: 'Dock bottom' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(screen.getByRole('button', { name: 'Hide terminal' })).toBeVisible();

    const bottomResizer = screen.getByRole('separator', { name: 'Resize bottom terminal dock' });
    const before = store.shellStore.dockBottomSize.value;
    fireEvent.keyDown(bottomResizer, { key: 'ArrowUp' });
    expect(store.shellStore.dockBottomSize.value).toBe(before + 24);

    await userEvent.click(screen.getByRole('button', { name: 'Dock right' }));
    expect(shell).toHaveClass('shell-layout-right');
    expect(screen.getByRole('separator', { name: 'Resize right terminal dock' })).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Full screen' }));
    expect(shell).toHaveClass('shell-layout-fullscreen');
    expect(screen.queryByRole('separator')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Back to chat' })).toBeVisible();
  });

  it('clamps keyboard resizing to the visible viewport and supports boundary keys', async () => {
    const store = shellStore();
    store.shellStore.layout.value = 'bottom';
    store.shellStore.dockBottomSize.value = 1400;
    render(<ShellOverlay store={store} />);

    const resizer = screen.getByRole('separator', { name: 'Resize bottom terminal dock' });
    expect(Number(resizer.getAttribute('aria-valuenow'))).toBeLessThanOrEqual(
      Number(resizer.getAttribute('aria-valuemax')),
    );

    fireEvent.keyDown(resizer, { key: 'End' });
    expect(store.shellStore.dockBottomSize.value).toBe(
      Number(resizer.getAttribute('aria-valuemax')),
    );
    fireEvent.keyDown(resizer, { key: 'Home' });
    expect(store.shellStore.dockBottomSize.value).toBe(220);
  });

  it('uses a bottom dock when the saved right dock cannot fit beside chat', () => {
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({
        matches: true,
        media: '(width <= 760px)',
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    );
    const store = shellStore();
    store.shellStore.layout.value = 'right';
    render(<ShellOverlay store={store} />);

    const shell = screen.getByLabelText('Interactive shell');
    expect(shell).toHaveClass('shell-layout-right');
    expect(shell).toHaveClass('shell-effective-layout-bottom');
    expect(screen.getByRole('separator')).toHaveAttribute('aria-orientation', 'horizontal');
  });

  it('offers restart after exit and binds the terminal at its fitted dimensions', async () => {
    const store = shellStore();
    store.shellStore.status.value = 'exited';
    store.shellStore.exitCode.value = 7;
    store.shellStore.restart = vi.fn(async () => undefined);
    render(<ShellOverlay store={store} />);
    await act(async () => {
      await new Promise((resolve) => requestAnimationFrame(resolve));
    });

    expect(screen.getByText('Exited (7)')).toBeVisible();
    expect(store.shellStore.connect).toHaveBeenCalledWith(80, 24, expect.any(Object));
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    expect(store.shellStore.restart).toHaveBeenCalledWith(80, 24, expect.any(Object));
  });

  it('shares in one click and renders collaborative command controls', async () => {
    const store = shellStore();
    store.shellStore.collaborationSupported.value = true;
    store.shellStore.shellToolAvailable.value = true;
    store.shellStore.enableCollaboration = vi.fn(async () => undefined);
    store.shellStore.disableCollaboration = vi.fn(async () => undefined);
    store.shellStore.interruptCommand = vi.fn(async () => undefined);
    render(<ShellOverlay store={store} />);

    await userEvent.click(screen.getByRole('button', { name: 'Share with agent' }));
    expect(store.shellStore.enableCollaboration).toHaveBeenCalledOnce();
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();

    act(() => {
      store.shellStore.collaborationState.value = 'agent_running';
      store.shellStore.activeCommandId.value = 'cmd_one';
    });
    expect(screen.getByText('Agent running')).toBeVisible();
    expect(screen.getByText(/Typing in this terminal interacts directly/)).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Interrupt agent command' }));
    expect(store.shellStore.interruptCommand).toHaveBeenCalledOnce();
    await userEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
    expect(store.shellStore.disableCollaboration).toHaveBeenCalledOnce();
  });

  it('gates enable on authoritative response activity but keeps terminal input live while running', () => {
    const store = shellStore();
    store.shellStore.collaborationSupported.value = true;
    store.shellStore.shellToolAvailable.value = true;
    store.shellStore.input = vi.fn();
    store.sessions.value = [
      {
        ...store.sessions.value[0],
        activeRun: true,
        activeResponseId: 'resp_one',
      },
    ];
    render(<ShellOverlay store={store} />);

    expect(screen.getByRole('button', { name: 'Share with agent' })).toBeDisabled();
    act(() => {
      store.sessions.value = [
        {
          ...store.sessions.value[0],
          activeRun: false,
          activeResponseId: null,
        },
      ];
      store.shellStore.collaborationState.value = 'agent_running';
      store.shellStore.collaborationEnabled.value = true;
      store.shellStore.activeCommandId.value = 'cmd_one';
      terminalInput.handler?.('human reply\r');
    });
    expect(store.shellStore.input).toHaveBeenCalledWith('human reply\r');
    expect(screen.getByText(/Typing in this terminal interacts directly/)).toBeVisible();
  });

  it('shows the desynchronized reason and stop-sharing recovery action', () => {
    const store = shellStore();
    store.shellStore.collaborationState.value = 'desynchronized';
    store.shellStore.collaborationReason.value = 'Prompt marker was not observed.';
    render(<ShellOverlay store={store} />);
    expect(screen.getByText('Shared shell needs attention')).toBeVisible();
    expect(screen.getByText('Prompt marker was not observed.')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Stop sharing' })).toBeEnabled();
  });

  it('disables collaboration mutations when the overlay session is no longer active', async () => {
    const store = shellStore();
    store.shellStore.collaborationState.value = 'agent_running';
    store.shellStore.interruptCommand = vi.fn(async () => undefined);
    store.shellStore.disableCollaboration = vi.fn(async () => undefined);
    render(<ShellOverlay store={store} />);
    act(() => {
      store.sessions.value = [
        ...store.sessions.value,
        { ...store.sessions.value[0], id: 'session-two', title: 'Second' },
      ];
      store.activeSessionId.value = 'session-two';
    });
    const interrupt = screen.getByRole('button', { name: 'Interrupt agent command' });
    const stop = screen.getByRole('button', { name: 'Stop sharing' });
    expect(interrupt).toBeDisabled();
    expect(stop).toBeDisabled();
    await userEvent.click(interrupt);
    await userEvent.click(stop);
    expect(store.shellStore.interruptCommand).not.toHaveBeenCalled();
    expect(store.shellStore.disableCollaboration).not.toHaveBeenCalled();
  });

  it('hides and disables session-owned controls when the active conversation changes', async () => {
    const store = shellStore();
    const back = vi.spyOn(store.shellStore, 'back');
    store.shellStore.status.value = 'exited';
    store.shellStore.restart = vi.fn(async () => undefined);
    store.shellStore.close = vi.fn(async () => undefined);
    render(<ShellOverlay store={store} />);

    act(() => {
      store.sessions.value = [
        ...store.sessions.value,
        { ...store.sessions.value[0], id: 'session-two', title: 'Second' },
      ];
      store.activeSessionId.value = 'session-two';
    });

    await waitFor(() => expect(back).toHaveBeenCalled());
    const restart = screen.getByRole('button', { name: 'Restart' });
    const close = screen.getByRole('button', { name: 'End shell' });
    expect(restart).toBeDisabled();
    expect(close).toBeDisabled();
    await userEvent.click(restart);
    await userEvent.click(close);
    expect(store.shellStore.restart).not.toHaveBeenCalled();
    expect(store.shellStore.close).not.toHaveBeenCalled();
  });
});

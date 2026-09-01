import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppStore } from '../stores/app-store';
import { ShellOverlay } from './ShellOverlay';

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    loadAddon = vi.fn();
    open = vi.fn();
    focus = vi.fn();
    write = vi.fn();
    reset = vi.fn();
    dispose = vi.fn();
    onData = vi.fn(() => ({ dispose: vi.fn() }));
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
  for (const store of stores.splice(0)) store.dispose();
  vi.restoreAllMocks();
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
  it('presents visible chat/close controls and does not steal Escape', async () => {
    const store = shellStore();
    store.shellStore.back = vi.fn();
    store.shellStore.close = vi.fn(async () => undefined);
    render(<ShellOverlay store={store} />);

    expect(screen.getByText('/workspace/project')).toBeVisible();
    expect(screen.getByText('Connected')).toBeVisible();
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

    await userEvent.click(screen.getByRole('button', { name: 'Close shell' }));
    expect(store.shellStore.close).toHaveBeenCalledOnce();
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
    const close = screen.getByRole('button', { name: 'Close shell' });
    expect(restart).toBeDisabled();
    expect(close).toBeDisabled();
    await userEvent.click(restart);
    await userEvent.click(close);
    expect(store.shellStore.restart).not.toHaveBeenCalled();
    expect(store.shellStore.close).not.toHaveBeenCalled();
  });
});

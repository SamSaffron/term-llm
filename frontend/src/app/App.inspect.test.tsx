import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, expect, it, vi } from 'vitest';
import { App } from './App';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';

let store: AppStore;
afterEach(() => store?.dispose());

function setup() {
  store = new AppStore(testConfig);
  store.bootstrap = vi.fn(async () => undefined);
  store.startupDone.value = true;
  store.sessions.value = [testSession()];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  store.endpoints.sessionInspect = vi.fn(async () => ({
    schema_version: 1 as const,
    session_id: 's1',
    transcript_rev: 1,
    scope: 'persisted_history' as const,
    header: { runtime: { loaded: false } },
    blocks: [],
  }));
  render(<App store={store} />);
}

it.each([
  ['MacIntel', { metaKey: true }, { ctrlKey: true }],
  ['Linux x86_64', { ctrlKey: true }, { metaKey: true }],
] as const)(
  'toggles Inspect with the platform primary+O on %s',
  async (platform, primary, wrong) => {
    const original = Object.getOwnPropertyDescriptor(navigator, 'platform');
    Object.defineProperty(navigator, 'platform', { configurable: true, value: platform });
    try {
      setup();
      const wrongEvent = new KeyboardEvent('keydown', { key: 'o', ...wrong, cancelable: true });
      fireEvent(window, wrongEvent);
      expect(wrongEvent.defaultPrevented).toBe(false);
      expect(store.modal.value).toBe('');
      const event = new KeyboardEvent('keydown', { key: 'o', ...primary, cancelable: true });
      fireEvent(window, event);
      expect(event.defaultPrevented).toBe(true);
      expect(store.modal.value).toBe('inspect');
      expect(await screen.findByRole('dialog', { name: 'Inspect' })).toBeVisible();
      fireEvent.keyDown(window, { key: 'O', ...primary });
      await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Inspect' })).toBeNull());
      expect(store.modal.value).toBe('');
    } finally {
      if (original) Object.defineProperty(navigator, 'platform', original);
      else Reflect.deleteProperty(navigator, 'platform');
    }
  },
);

it('leaves primary+O to the shared shell', () => {
  setup();
  const mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform);
  const primary = mac ? { metaKey: true } : { ctrlKey: true };
  const shell = document.createElement('div');
  shell.className = 'shell-overlay';
  const terminal = document.createElement('textarea');
  shell.append(terminal);
  document.body.append(shell);
  try {
    const event = new KeyboardEvent('keydown', {
      key: 'o',
      ...primary,
      cancelable: true,
      bubbles: true,
    });
    fireEvent(terminal, event);
    expect(event.defaultPrevented).toBe(false);
    expect(store.modal.value).toBe('');
  } finally {
    shell.remove();
  }
});

it('leaves browser Open untouched without a session or while authentication is required', () => {
  setup();
  const mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform);
  const primary = mac ? { metaKey: true } : { ctrlKey: true };
  act(() => {
    store.authRequired.value = true;
  });
  const unauthorized = new KeyboardEvent('keydown', { key: 'o', ...primary, cancelable: true });
  fireEvent(window, unauthorized);
  expect(unauthorized.defaultPrevented).toBe(false);
  expect(store.modal.value).toBe('');
  act(() => {
    store.authRequired.value = false;
    store.draftActive.value = true;
    store.activeSessionId.value = '';
  });
  const noSession = new KeyboardEvent('keydown', { key: 'o', ...primary, cancelable: true });
  fireEvent(window, noSession);
  expect(noSession.defaultPrevented).toBe(false);
  expect(store.modal.value).toBe('');
});

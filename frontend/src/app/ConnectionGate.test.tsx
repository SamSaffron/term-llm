import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, expect, it, vi } from 'vitest';
import { App } from './App';
import { readInjectedConfig } from './config';
import { APIError } from '../api/client';
import { AppStore } from '../stores/app-store';

let store: AppStore;
afterEach(() => store?.dispose());
function setup(auth = true, acceptedToken = 'server-secret') {
  store = new AppStore(readInjectedConfig({ TERM_LLM_UI_PREFIX: '/ui' } as Window));
  store.endpoints.capabilities = vi.fn(async () => ({ projects: { enabled: false } }));
  store.endpoints.providers = vi.fn(async () => {
    if (auth && store.token.peek() !== acceptedToken)
      throw new APIError('invalid authentication credentials', 401);
    return { object: 'list', data: [] };
  });
  store.endpoints.verifyToken = vi.fn(async (token) => {
    if (auth && token !== acceptedToken)
      throw new APIError('invalid authentication credentials', 401);
    return { object: 'list', data: [] };
  });
  store.endpoints.sessions = vi.fn(async () => ({ object: 'list', data: [] }));
  store.endpoints.models = vi.fn(async () => ({ object: 'list', data: [] }));
  store.loadWidgetStatus = vi.fn(async () => undefined);
  vi.spyOn(store.notificationController, 'installLifecycle').mockReturnValue(() => undefined);
  return store;
}

it('replaces the entire chat with token instructions, reports invalid tokens inline, and recovers', async () => {
  setup();
  const { container } = render(<App store={store} />);
  expect(await screen.findByRole('heading', { name: 'Connect to your server' })).toBeVisible();
  expect(container.querySelector('#appShell')).toBeNull();
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(screen.queryByText('Enable notifications')).toBeNull();
  expect(store.notificationController.installLifecycle).not.toHaveBeenCalled();
  expect(screen.queryByText(/API key/)).toBeNull();
  expect(screen.getByRole('button', { name: 'Connect to server' })).toBeDisabled();
  const input = screen.getByLabelText('Server token');
  fireEvent.input(input, { target: { value: 'wrong-token' } });
  fireEvent.submit(input.closest('form')!);
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'That token didn’t match. Copy the value after token:',
  );
  expect(input).toHaveAttribute('aria-invalid', 'true');
  expect(localStorage.getItem(store.keys.token)).toBeNull();
  expect(store.token.value).toBe('');
  expect(input).toHaveFocus();
  expect(container.querySelector('#appShell')).toBeNull();
  fireEvent.input(input, { target: { value: '  server-secret  ' } });
  fireEvent.submit(input.closest('form')!);
  await waitFor(() => expect(container.querySelector('#appShell')).not.toBeNull());
  expect(store.token.value).toBe('server-secret');
  expect(localStorage.getItem(store.keys.token)).toBe('server-secret');
  expect(document.cookie).toContain('term_llm_token=server-secret');
  expect(store.authRequired.value).toBe(false);
  expect(store.modal.value).toBe('');
  expect(store.notificationController.installLifecycle).toHaveBeenCalledOnce();
  expect(screen.queryByRole('heading', { name: 'Connect to your server' })).toBeNull();
});

it('keeps the gate stable during verification and distinguishes a network error', async () => {
  setup();
  render(<App store={store} />);
  const input = await screen.findByLabelText('Server token');
  let reject!: (reason: Error) => void;
  store.endpoints.verifyToken = vi.fn(
    () =>
      new Promise<Record<string, unknown>>((_, fail) => {
        reject = fail;
      }),
  );
  fireEvent.input(input, { target: { value: 'server-secret' } });
  fireEvent.submit(input.closest('form')!);
  expect(screen.getByRole('button', { name: 'Connecting…' })).toBeDisabled();
  fireEvent.submit(input.closest('form')!);
  expect(store.endpoints.verifyToken).toHaveBeenCalledOnce();
  await act(async () => reject(new TypeError('Failed to fetch')));
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Check that term-llm is still running',
  );
  expect(screen.queryByText(/That token didn’t match/)).toBeNull();
});

it.each(['no-auth', 'saved-token'])('does not gate a working %s connection', async (mode) => {
  setup(mode === 'saved-token');
  if (mode === 'saved-token') store.services.setToken('server-secret');
  const { container } = render(<App store={store} />);
  await waitFor(() => expect(container.querySelector('#appShell')).not.toBeNull());
  expect(screen.queryByRole('heading', { name: 'Connect to your server' })).toBeNull();
});

it('replaces a rejected saved token with fresh instructions rather than an obscured settings dialog', async () => {
  setup();
  store.services.setToken('old-server-token');
  render(<App store={store} />);
  const input = await screen.findByLabelText('Server token');
  expect(input).toHaveValue('');
  expect(screen.queryByRole('alert')).toBeNull();
  expect(screen.queryByRole('dialog')).toBeNull();
});

it('only explains provider keys after the server rejects one', async () => {
  setup();
  render(<App store={store} />);
  const input = await screen.findByLabelText('Server token');
  expect(screen.queryByText(/API key/)).toBeNull();
  fireEvent.input(input, { target: { value: 'sk-not-a-real-provider-key' } });
  fireEvent.submit(input.closest('form')!);
  expect(await screen.findByRole('alert')).toHaveTextContent('This looks like an API key');
});

it('accepts a custom server token even when it resembles a provider key', async () => {
  setup(true, 'sk-custom-server-token');
  const { container } = render(<App store={store} />);
  const input = await screen.findByLabelText('Server token');
  fireEvent.input(input, { target: { value: 'sk-custom-server-token' } });
  fireEvent.submit(input.closest('form')!);
  await waitFor(() => expect(container.querySelector('#appShell')).not.toBeNull());
  expect(screen.queryByText(/API key/)).toBeNull();
  expect(localStorage.getItem(store.keys.token)).toBe('sk-custom-server-token');
});

it.each([401, 403])(
  'preserves a post-verification %i error and rolls back the rejected token',
  async (status) => {
    setup();
    store.services.setToken('old-token');
    render(<App store={store} />);
    const input = await screen.findByLabelText('Server token');
    store.endpoints.sessions = vi.fn(async () => {
      throw new APIError('Token rotated during startup', status);
    });
    fireEvent.input(input, { target: { value: 'server-secret' } });
    fireEvent.submit(input.closest('form')!);
    expect(await screen.findByRole('alert')).toHaveTextContent('That token didn’t match');
    expect(store.token.value).toBe('old-token');
    expect(localStorage.getItem(store.keys.token)).toBe('old-token');
    expect(document.cookie).toContain('term_llm_token=old-token');
    expect(store.authRequired.value).toBe(true);
  },
);

it('keeps a verified token after a non-auth startup failure and allows retry', async () => {
  setup();
  const { container } = render(<App store={store} />);
  const input = await screen.findByLabelText('Server token');
  store.endpoints.sessions = vi.fn(async () => {
    throw new APIError('Server unavailable', 503);
  });
  fireEvent.input(input, { target: { value: 'server-secret' } });
  fireEvent.submit(input.closest('form')!);
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Check that term-llm is still running',
  );
  expect(localStorage.getItem(store.keys.token)).toBe('server-secret');
  store.endpoints.sessions = vi.fn(async () => ({ object: 'list', data: [] }));
  fireEvent.submit(input.closest('form')!);
  await waitFor(() => expect(container.querySelector('#appShell')).not.toBeNull());
});

it('provides a retry for non-auth initial startup failures without enrolling notifications', async () => {
  setup(false);
  store.endpoints.providers = vi.fn(async () => {
    throw new APIError('Server unavailable', 503);
  });
  const { container } = render(<App store={store} />);
  expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible();
  expect(store.notificationController.installLifecycle).not.toHaveBeenCalled();
  expect(container.querySelector('.startup-spinner')).toBeNull();
  store.endpoints.providers = vi.fn(async () => ({ object: 'list', data: [] }));
  fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(container.querySelector('#appShell')).not.toBeNull());
});

it('preserves the mounted chat when authentication expires and explains reconnecting', async () => {
  setup();
  store.services.setToken('server-secret');
  const { container } = render(<App store={store} />);
  await waitFor(() => expect(container.querySelector('#appShell')).not.toBeNull());
  const shell = container.querySelector('#appShell');
  fireEvent.input(screen.getByRole('textbox', { name: 'Message', exact: true }), {
    target: { value: 'Keep this unsent draft while reconnecting.' },
  });
  const draftId = store.composer.ownerKey();
  const newChat = vi.spyOn(store, 'newChat');
  act(() => {
    store.authRequired.value = true;
  });
  const input = await screen.findByLabelText('Server token');
  expect(screen.getByText('Connection expired. Use the server’s current token.')).toBeVisible();
  expect(container.querySelector('#appShell')).toBe(shell);
  expect(shell).not.toBeVisible();
  fireEvent.input(input, { target: { value: 'server-secret' } });
  fireEvent.submit(input.closest('form')!);
  await waitFor(() => expect(shell).toBeVisible());
  expect(container.querySelector('#appShell')).toBe(shell);
  expect(screen.getByRole('textbox', { name: 'Message', exact: true })).toHaveValue(
    'Keep this unsent draft while reconnecting.',
  );
  expect(store.composer.ownerKey()).toBe(draftId);
  expect(newChat).not.toHaveBeenCalled();
});

it('keeps Settings and the working credential intact when a replacement token is rejected', async () => {
  setup();
  store.services.setToken('server-secret');
  const { container } = render(<App store={store} />);
  await waitFor(() => expect(container.querySelector('#appShell')).not.toBeNull());
  act(() => {
    store.modal.value = 'settings';
  });
  fireEvent.click(screen.getByRole('tab', { name: 'Connection' }));
  fireEvent.input(screen.getByLabelText('Bearer token'), {
    target: { value: 'rejected-replacement' },
  });
  fireEvent.click(screen.getByRole('button', { name: 'Save', exact: true }));
  expect(await screen.findByRole('alert')).toHaveTextContent('invalid authentication credentials');
  expect(store.authRequired.value).toBe(false);
  expect(store.modal.value).toBe('settings');
  expect(store.token.value).toBe('server-secret');
  expect(localStorage.getItem(store.keys.token)).toBe('server-secret');
});

it('does not replace the shared credential while verification is pending', async () => {
  setup();
  store.services.setToken('old-token');
  let reject!: (error: Error) => void;
  store.endpoints.verifyToken = vi.fn(
    () =>
      new Promise<Record<string, unknown>>((_, fail) => {
        reject = fail;
      }),
  );
  const connection = store.connect('candidate');
  expect(store.token.value).toBe('old-token');
  expect(document.cookie).toContain('term_llm_token=old-token');
  expect(localStorage.getItem(store.keys.token)).toBe('old-token');
  reject(new APIError('Rejected', 401));
  await expect(connection).rejects.toMatchObject({ status: 401 });
  expect(store.token.value).toBe('old-token');
});

it.each(['capabilities', 'models'] as const)(
  'does not swallow auth failures from optional %s discovery',
  async (endpoint) => {
    setup();
    store.authRequired.value = true;
    store.endpoints[endpoint] = vi.fn(async () => {
      throw new APIError('Expired', 401);
    });
    await expect(store.connect('server-secret')).rejects.toMatchObject({ status: 401 });
    expect(store.startupDone.value).toBe(false);
    expect(store.authRequired.value).toBe(true);
    expect(localStorage.getItem(store.keys.token)).toBeNull();
  },
);

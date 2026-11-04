import { signal } from '@preact/signals';
import { expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import type { Modal } from './store-types';
import { CommitStore } from './commit-store';

vi.mock('./commit-workflow', () => {
  throw new Error('Chunk unavailable');
});

function fixture() {
  const activeSession = signal<Session | null>({ id: 's1' } as Session);
  const modal = signal<Modal>('');
  const store = new CommitStore(
    {
      toast: vi.fn(),
      storage: localStorage,
      isDisposed: false,
    } as unknown as AppStoreServices,
    {
      activeSession,
      draftActive: signal(false),
      modal,
      busy: () => false,
      changed: vi.fn(),
    },
  );
  return { store, modal, activeSession };
}

it('shows a failed chunk load in the modal and dismisses it', async () => {
  const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined);
  const { store, modal } = fixture();

  await expect(store.open('')).resolves.toBe(false);
  expect(modal.value).toBe('commit');
  expect(store.state.peek().phase).toBe('error');
  expect(store.state.peek().error).toBe(
    'Could not load commit controls. Reload the page to retry.',
  );
  expect(logged).toHaveBeenCalledWith('Failed to load the commit workflow', expect.any(Error));

  store.close();
  expect(modal.value).toBe('');
  expect(store.state.peek().phase).toBe('closed');
});

it('does not report a load error for a conversation that was left during the load', async () => {
  const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined);
  const { store, modal, activeSession } = fixture();
  const pending = store.open('');
  activeSession.value = { id: 's2' } as Session;

  await expect(pending).resolves.toBe(false);
  expect(modal.value).toBe('');
  expect(store.state.peek().phase).toBe('closed');
  expect(store.state.peek().error).toBe('');
  expect(logged).toHaveBeenCalled();
});

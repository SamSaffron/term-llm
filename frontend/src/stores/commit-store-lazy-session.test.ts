import { signal } from '@preact/signals';
import { expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import type { Modal } from './store-types';
import { CommitStore } from './commit-store';

// A separate file gives this test a fresh module registry, so the workflow
// import can stay pending while the conversation switches.
const harness = vi.hoisted(() => ({
  loads: 0,
  gate: null as Promise<void> | null,
  openCalls: 0,
}));

vi.mock('./commit-workflow', async () => {
  harness.loads += 1;
  if (harness.gate) await harness.gate;
  class CommitWorkflow {
    async open(_intent = ''): Promise<boolean> {
      harness.openCalls += 1;
      return true;
    }
    cancel(): void {}
  }
  return {
    CommitWorkflow: CommitWorkflow as unknown as typeof import('./commit-workflow').CommitWorkflow,
  };
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

it('dismisses a pending open when the conversation switches during the chunk load', async () => {
  let release!: () => void;
  harness.gate = new Promise((resolve) => {
    release = resolve;
  });
  const { store, modal, activeSession } = fixture();
  const pending = store.open('');
  activeSession.value = { id: 's2' } as Session;

  harness.gate = null;
  release();
  await expect(pending).resolves.toBe(false);
  expect(harness.loads).toBe(1);
  expect(harness.openCalls).toBe(0);
  expect(modal.value).toBe('');
  expect(store.state.peek().phase).toBe('closed');
});

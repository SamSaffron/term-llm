import { signal } from '@preact/signals';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import type { Modal } from './store-types';
import { CommitStore } from './commit-store';

const harness = vi.hoisted(() => ({
  loads: 0,
  gate: null as Promise<void> | null,
  actionGate: null as Promise<void> | null,
  failConstruction: false,
  openCalls: 0,
  cancelCalls: 0,
  commitCalls: 0,
}));

vi.mock('./commit-workflow', () => {
  harness.loads += 1;
  class CommitWorkflow {
    constructor() {
      if (harness.failConstruction) throw new Error('Chunk unavailable');
    }
    async open(_intent = ''): Promise<boolean> {
      if (harness.gate) await harness.gate;
      harness.openCalls += 1;
      return true;
    }
    cancel(): void {
      harness.cancelCalls += 1;
    }
    async chooseEverything(): Promise<void> {}
    async chooseStaged(): Promise<void> {}
    async followRequest(): Promise<void> {}
    async reviewFiles(): Promise<void> {}
    async backToMessage(): Promise<void> {}
    async regenerate(): Promise<void> {}
    async commit(): Promise<void> {
      if (harness.actionGate) await harness.actionGate;
      harness.commitCalls += 1;
    }
    async preparePublish(_kind: 'push' | 'pr'): Promise<void> {}
    async publish(): Promise<void> {}
    async reconnectPublish(): Promise<void> {}
  }
  return {
    CommitWorkflow: CommitWorkflow as unknown as typeof import('./commit-workflow').CommitWorkflow,
  };
});

function fixture(draft = false) {
  const activeSession = signal<Session | null>({ id: 's1' } as Session);
  const modal = signal<Modal>('');
  const draftActive = signal(draft);
  const store = new CommitStore(
    {
      toast: vi.fn(),
      storage: localStorage,
      isDisposed: false,
    } as unknown as AppStoreServices,
    {
      activeSession,
      draftActive,
      modal,
      busy: () => false,
      changed: vi.fn(),
    },
  );
  return { store, modal, activeSession, draftActive };
}

function gateAction(): () => void {
  let release!: () => void;
  harness.actionGate = new Promise((resolve) => {
    release = resolve;
  });
  return () => {
    harness.actionGate = null;
    release();
  };
}

beforeEach(() => {
  harness.loads = 0;
  harness.gate = null;
  harness.actionGate = null;
  harness.failConstruction = false;
  harness.openCalls = 0;
  harness.cancelCalls = 0;
  harness.commitCalls = 0;
});

describe('lazy commit workflow', () => {
  it('does not load the workflow for construction or synchronous edits', () => {
    const { store } = fixture();
    expect(harness.loads).toBe(0);
    expect(store.state.peek().phase).toBe('closed');

    store.setMessage('draft');
    store.setSelected('a.ts', true);
    store.editPublish({ title: 'ignored without a form' });
    store.cancelPublish();
    expect(store.allPaths()).toEqual([]);
    expect(store.state.peek()).toMatchObject({
      message: 'draft',
      selected: ['a.ts'],
      selectionNeedsApply: true,
    });

    store.reset();
    expect(store.state.peek().phase).toBe('closed');
    expect(harness.loads).toBe(0);
    expect(harness.cancelCalls).toBe(0);
  });

  it('loads once on open and reuses the same workflow', async () => {
    const { store, modal } = fixture();
    await expect(store.open('')).resolves.toBe(true);
    expect(harness.loads).toBe(1);
    expect(harness.openCalls).toBe(1);
    expect(modal.value).toBe('commit');

    store.reset();
    expect(harness.cancelCalls).toBe(1);
    expect(modal.value).toBe('');
    expect(store.state.peek().phase).toBe('closed');

    await expect(store.open('only A')).resolves.toBe(true);
    expect(harness.loads).toBe(1);
    expect(harness.openCalls).toBe(2);
  });

  it('waits for the loaded workflow before running deferred actions', async () => {
    const { store } = fixture();
    const release = gateAction();
    const pending = store.commit();
    expect(harness.commitCalls).toBe(0);

    release();
    await pending;
    expect(harness.commitCalls).toBe(1);
  });

  it('drops a queued action when the conversation changes while the workflow loads', async () => {
    const { store, activeSession } = fixture();
    const pending = store.commit();
    activeSession.value = { id: 's2' } as Session;
    await pending;
    expect(harness.commitCalls).toBe(0);
  });

  it('drops a queued action when a draft starts while the workflow loads', async () => {
    const { store, draftActive } = fixture();
    const pending = store.commit();
    draftActive.value = true;
    await pending;
    expect(harness.commitCalls).toBe(0);
  });

  it('retries construction after a failed load instead of caching the failure', async () => {
    const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const { store, modal } = fixture();
    harness.failConstruction = true;

    await expect(store.open('')).resolves.toBe(false);
    expect(store.state.peek().phase).toBe('error');
    expect(store.state.peek().error).toBe(
      'Could not load commit controls. Reload the page to retry.',
    );
    expect(logged).toHaveBeenCalledWith('Failed to load the commit workflow', expect.any(Error));
    store.close();
    expect(modal.value).toBe('');

    harness.failConstruction = false;
    await expect(store.open('')).resolves.toBe(true);
    expect(harness.openCalls).toBe(1);
  });
});

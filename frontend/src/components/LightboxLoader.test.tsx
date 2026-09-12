import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore, type LightboxState } from '../stores/app-store';
import { LightboxLoader } from './LightboxLoader';

const pendingModule = () => {
  let resolve!: (module: typeof import('./Lightbox')) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<typeof import('./Lightbox')>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { resolve, reject, load: () => promise };
};

function setup(media: LightboxState, load: ReturnType<typeof pendingModule>['load']) {
  const store = new AppStore({
    prefix: '/ui',
    version: 'v1',
    sidebarCategories: ['all'],
    agentName: '',
    agentNames: [],
    title: '',
    locationSharing: false,
    worktrees: false,
    hub: null,
    vapidKey: '',
    webRTC: false,
    signalingURL: '',
  });
  store.lightbox.value = media;
  function Host() {
    return (
      <StoreContext.Provider value={store}>
        {store.lightbox.value && <LightboxLoader load={load} />}
      </StoreContext.Provider>
    );
  }
  const trigger = document.createElement('button');
  trigger.textContent = 'Thumbnail';
  document.body.append(trigger);
  trigger.focus();
  const result = render(<Host />);
  return { ...result, store, trigger };
}

afterEach(() => {
  document.querySelectorAll('body > button').forEach((button) => button.remove());
});

describe('lazy lightbox lifecycle', () => {
  it('releases only owned pending URLs exactly once when dismissed before import resolves', async () => {
    const pending = pendingModule();
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const { trigger } = setup(
      {
        src: 'blob:owned',
        type: 'image',
        items: [
          { key: 'a', src: 'blob:owned', type: 'image', ownsObjectURL: true },
          { key: 'b', src: 'blob:owned', type: 'image', ownsObjectURL: true },
          { key: 'c', src: 'blob:borrowed', type: 'image' },
        ],
      },
      pending.load,
    );
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:owned');
    expect(trigger).toHaveFocus();
    const actual = await import('./Lightbox');
    await act(async () => {
      pending.resolve(actual);
    });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('uses fallback focus when the original trigger disappears while loading', async () => {
    const pending = pendingModule();
    const fallback = document.createElement('button');
    fallback.textContent = 'Composer';
    document.body.append(fallback);
    const { trigger } = setup(
      { src: 'https://example.com/image.png', type: 'image', fallbackFocus: () => fallback },
      pending.load,
    );
    trigger.remove();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    await waitFor(() => expect(fallback).toHaveFocus());
  });

  it('offers reload and the selected original after import failure and releases pending resources on close', async () => {
    const pending = pendingModule();
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    setup(
      {
        src: 'blob:first',
        type: 'image',
        index: 1,
        items: [
          { key: 'a', src: 'blob:first', type: 'image', ownsObjectURL: true },
          { key: 'b', src: 'blob:selected', type: 'image', ownsObjectURL: true },
        ],
      },
      pending.load,
    );
    await act(async () => {
      pending.reject(new Error('Module fetch failed'));
    });
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not load the image viewer');
    expect(screen.getByRole('button', { name: 'Reload page' })).toBeVisible();
    expect(screen.getByRole('link', { name: 'Open original' })).toHaveAttribute(
      'href',
      'blob:selected',
    );
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(revoke).toHaveBeenCalledTimes(2);
  });

  it('hands ownership to the mounted viewer and returns focus to the original trigger on close', async () => {
    const actual = await import('./Lightbox');
    const pending = pendingModule();
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const { trigger } = setup(
      { src: 'blob:owned', type: 'image', ownsObjectURL: true },
      pending.load,
    );
    screen.getByRole('button', { name: 'Close Media preview' }).focus();
    await act(async () => {
      pending.resolve(actual);
    });
    await screen.findByRole('img');
    expect(revoke).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toHaveFocus();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(trigger).toHaveFocus();
    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:owned');
  });
});

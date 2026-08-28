import { beforeEach, describe, expect, it } from 'vitest';
import { OverlayManager } from './overlay-manager';

describe('OverlayManager', () => {
  beforeEach(() => {
    document.body.innerHTML = '<button id="trigger">Open</button><main id="appShell"></main>';
    document.documentElement.style.overflow = '';
  });

  it('reference-counts nested inert and scroll ownership', () => {
    const manager = new OverlayManager();
    const trigger = document.getElementById('trigger') as HTMLButtonElement;
    trigger.focus();
    const first = manager.acquire();
    const second = manager.acquire();
    expect((document.getElementById('appShell') as HTMLElement).inert).toBe(true);
    expect(document.documentElement.style.overflow).toBe('hidden');

    manager.release(second);
    expect((document.getElementById('appShell') as HTMLElement).inert).toBe(true);
    manager.release(first);
    expect((document.getElementById('appShell') as HTMLElement).inert).toBe(false);
    expect(document.documentElement.style.overflow).toBe('');
    expect(document.activeElement).toBe(trigger);
  });

  it('keeps lower surfaces inert when a non-top overlay releases', () => {
    const manager = new OverlayManager();
    const lower = document.createElement('section');
    const upper = document.createElement('section');
    document.body.append(lower, upper);
    const lowerToken = manager.acquire(undefined, lower);
    manager.acquire(undefined, upper);
    expect(lower.inert).toBe(true);

    manager.release(lowerToken);

    expect(upper.inert).toBe(false);
    expect((document.getElementById('appShell') as HTMLElement).inert).toBe(true);
  });

  it('keeps the active overlay root interactive while inerting its siblings and toasts', () => {
    document.body.innerHTML = `
      <main id="appShell">
        <section id="overlayRoot"><aside id="surface"></aside><button id="backdrop"></button></section>
        <div id="content"></div>
      </main>
      <div id="toastRegion"><button>Dismiss toast</button></div>
    `;
    const manager = new OverlayManager();
    const root = document.getElementById('overlayRoot') as HTMLElement;
    const token = manager.acquire(undefined, root);

    expect(root.inert).not.toBe(true);
    expect((document.getElementById('backdrop') as HTMLElement).inert).not.toBe(true);
    expect((document.getElementById('content') as HTMLElement).inert).toBe(true);
    expect((document.getElementById('toastRegion') as HTMLElement).inert).toBe(true);

    manager.release(token);
    expect((document.getElementById('content') as HTMLElement).inert).toBe(false);
    expect((document.getElementById('toastRegion') as HTMLElement).inert).toBe(false);
  });

  it('does not restore focus to a detached trigger', () => {
    const manager = new OverlayManager();
    const trigger = document.getElementById('trigger') as HTMLButtonElement;
    trigger.focus();
    const token = manager.acquire();
    trigger.remove();
    expect(() => manager.release(token)).not.toThrow();
  });
});

import { afterEach, describe, expect, it, vi } from 'vitest';
import { ThemeStore, themeOptions } from './theme-store';

const key = 'term_llm_chat_theme:%2Fui';
let unmount: (() => void) | undefined;
afterEach(() => {
  unmount?.();
  unmount = undefined;
});

describe('browser theme preference', () => {
  it.each([null, '', 'unknown', '<script>'])('keeps core system colors for %s', (saved) => {
    if (saved !== null) localStorage.setItem(key, saved);
    const theme = new ThemeStore('/ui', localStorage);
    unmount = theme.mount();
    expect(theme.selected.value).toBe('system');
    expect(document.documentElement).not.toHaveAttribute('data-chat-theme');
  });

  it.each(themeOptions.filter(({ value }) => value !== 'system'))(
    'applies and restores $label, then returns to the default',
    ({ value }) => {
      const theme = new ThemeStore('/ui', localStorage);
      unmount = theme.mount();
      theme.set(value);
      expect(document.documentElement).toHaveAttribute('data-chat-theme', value);
      unmount();
      const restored = new ThemeStore('/ui', localStorage);
      expect(restored.selected.value).toBe(value);
      unmount = restored.mount();
      expect(document.documentElement).toHaveAttribute('data-chat-theme', value);
      restored.set('system');
      expect(document.documentElement).not.toHaveAttribute('data-chat-theme');
      expect(new ThemeStore('/ui', localStorage).selected.value).toBe('system');
    },
  );

  it('separates UI mounts and normalizes trailing slashes', () => {
    new ThemeStore('/node/personal/', localStorage).set('nord');
    expect(new ThemeStore('/node/personal', localStorage).selected.value).toBe('nord');
    expect(new ThemeStore('/node/atlas', localStorage).selected.value).toBe('system');
    expect(new ThemeStore('/other/node/personal', localStorage).selected.value).toBe('system');
  });

  it('loads palettes only when selected, before existing extension styles', () => {
    const extension = document.createElement('link');
    extension.dataset.extension = 'custom';
    document.head.append(extension);
    try {
      const theme = new ThemeStore('/ui', localStorage);
      unmount = theme.mount();
      expect(document.querySelector('link[data-chat-themes]')).toBeNull();
      theme.set('nord');
      const palette = document.querySelector('link[data-chat-themes]')!;
      expect(palette.nextElementSibling).toBe(extension);
      theme.set('dracula');
      expect(document.querySelectorAll('link[data-chat-themes]')).toHaveLength(1);
      theme.set('system');
      expect(document.querySelector('link[data-chat-themes]')).toBe(palette);
      unmount();
      expect(document.querySelector('link[data-chat-themes]')).toBeNull();
    } finally {
      extension.remove();
    }
  });

  it('leaves the default usable after a load error and retries the next selection', () => {
    const theme = new ThemeStore('/ui', localStorage);
    unmount = theme.mount();
    theme.set('nord');
    const failed = document.querySelector('link[data-chat-themes]')!;
    failed.dispatchEvent(new Event('error'));
    expect(theme.error.value).toMatch(/Could not load/);
    expect(failed.isConnected).toBe(false);
    theme.set('system');
    expect(theme.error.value).toBe('');
    expect(document.documentElement).not.toHaveAttribute('data-chat-theme');
    theme.set('nord');
    expect(document.querySelector('link[data-chat-themes]')).not.toBe(failed);
  });

  it('restores the original browser chrome colors on default and unmount', () => {
    const meta = document.createElement('meta');
    meta.name = 'theme-color';
    meta.content = '#212121';
    document.head.append(meta);
    const style = vi.spyOn(window, 'getComputedStyle').mockReturnValue({
      getPropertyValue: () => '#1e1e2e',
    } as unknown as CSSStyleDeclaration);
    try {
      const theme = new ThemeStore('/ui', localStorage);
      unmount = theme.mount();
      theme.set('catppuccin');
      const palette = document.querySelector('link[data-chat-themes]')!;
      Object.defineProperty(palette, 'sheet', { value: {} });
      palette.dispatchEvent(new Event('load'));
      expect(meta.content).toBe('#1e1e2e');
      theme.set('system');
      expect(meta.content).toBe('#212121');
      theme.set('catppuccin');
      expect(meta.content).toBe('#1e1e2e');
      unmount();
      expect(meta.content).toBe('#212121');
    } finally {
      meta.remove();
      style.mockRestore();
    }
  });

  it('still switches when browser storage is unavailable and ignores invalid choices', () => {
    vi.spyOn(localStorage, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('full');
    });
    const theme = new ThemeStore('/ui', localStorage);
    unmount = theme.mount();
    expect(theme.selected.value).toBe('system');
    theme.set('dracula');
    theme.set('invalid');
    expect(document.documentElement).toHaveAttribute('data-chat-theme', 'dracula');
  });

  it('syncs this mount across tabs, handles clearing storage, and stops on unmount', () => {
    const theme = new ThemeStore('/ui', localStorage);
    unmount = theme.mount();
    const change = (changedKey: string | null, value: string | null) =>
      window.dispatchEvent(new StorageEvent('storage', { key: changedKey, newValue: value }));
    change('another-mount', 'dracula');
    expect(theme.selected.value).toBe('system');
    change(key, 'catppuccin');
    expect(document.documentElement).toHaveAttribute('data-chat-theme', 'catppuccin');
    change(null, null);
    expect(document.documentElement).not.toHaveAttribute('data-chat-theme');
    change(key, 'invalid');
    expect(theme.selected.value).toBe('system');
    unmount();
    change(key, 'nord');
    expect(theme.selected.value).toBe('system');
  });
});

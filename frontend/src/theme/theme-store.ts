import { effect, signal, type Signal } from '@preact/signals';
import themeStylesURL from './themes.css?url';

export const themeOptions = [
  { value: 'system', label: 'Default (system light/dark)' },
  { value: 'sage-light', label: 'Sage Light' },
  { value: 'sage-dark', label: 'Sage Dark' },
  { value: 'catppuccin', label: 'Catppuccin Mocha' },
  { value: 'dracula', label: 'Dracula' },
  { value: 'nord', label: 'Nord' },
  { value: 'tokyo-night', label: 'Tokyo Night' },
] as const;

export type Theme = (typeof themeOptions)[number]['value'];
const isTheme = (value: unknown): value is Theme =>
  themeOptions.some((option) => option.value === value);

/** Browser-local appearance for one UI mount, independent of server extensions. */
export class ThemeStore {
  readonly selected: Signal<Theme>;
  readonly error = signal('');
  private readonly key: string;

  constructor(
    prefix: string,
    private readonly storage: Storage,
  ) {
    this.key = `term_llm_chat_theme:${encodeURIComponent(prefix.replace(/\/+$/, '') || '/')}`;
    let saved: unknown;
    try {
      saved = storage.getItem(this.key);
    } catch {
      // Storage restrictions must not prevent using the default theme.
    }
    this.selected = signal(isTheme(saved) ? saved : 'system');
  }

  set(value: string): void {
    if (!isTheme(value)) return;
    this.selected.value = value;
    try {
      this.storage.setItem(this.key, value);
    } catch {
      // The selection still applies to this page when storage is full or unavailable.
    }
  }

  /** Bind before paint; CSS owns system appearance, so no media listener is needed. */
  mount(): () => void {
    const root = document.documentElement;
    const meta = [...document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')].map(
      (element) => ({ element, content: element.content }),
    );
    let stylesheet: HTMLLinkElement | undefined;
    const updateMeta = () => {
      const color =
        this.selected.peek() !== 'system' && stylesheet?.sheet
          ? getComputedStyle(root).getPropertyValue('--bg').trim()
          : '';
      for (const { element, content } of meta) element.content = color || content;
    };
    const loadStyles = () => {
      if (stylesheet) return;
      const link = document.createElement('link');
      link.rel = 'stylesheet';
      link.href = themeStylesURL;
      link.dataset.chatThemes = '';
      link.onload = updateMeta;
      link.onerror = () => {
        if (this.selected.peek() !== 'system')
          this.error.value = 'Could not load this theme. Reload the page to try again.';
        link.remove();
        stylesheet = undefined;
      };
      stylesheet = link;
      // Keep user extensions last even when a palette is first chosen after startup.
      document.head.insertBefore(link, document.head.querySelector('link[data-extension]'));
    };
    const stop = effect(() => {
      const selected = this.selected.value;
      this.error.value = '';
      if (selected === 'system') delete root.dataset.chatTheme;
      else {
        root.dataset.chatTheme = selected;
        loadStyles();
      }
      updateMeta();
    });
    const onStorage = (event: StorageEvent) => {
      if (event.storageArea && event.storageArea !== this.storage) return;
      if (event.key !== null && event.key !== this.key) return;
      this.selected.value = isTheme(event.newValue) ? event.newValue : 'system';
    };
    window.addEventListener('storage', onStorage);
    return () => {
      stop();
      window.removeEventListener('storage', onStorage);
      if (stylesheet) {
        stylesheet.onload = null;
        stylesheet.onerror = null;
        stylesheet.remove();
      }
      delete root.dataset.chatTheme;
      for (const { element, content } of meta) element.content = content;
    };
  }
}

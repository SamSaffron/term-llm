import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { loadExtensions, removeRenderedExtensions } from './extensions';
import {
  iconOverrides,
  iconOverridesFrom,
  renderedIconSprites,
} from '../components/icon-overrides';
import { readInjectedConfig } from '../app/config';
import { iconHTML } from '../components/Icon';
import { AppStore } from '../stores/app-store';
import { extensionHost, extensionRuntime, type WebExtension } from '../stores/extension-runtime';

// Mirrors the <meta> internal/extensions/icons.go renders in <head>.
function renderedIcons(id: string, generation: string, symbols: string): HTMLMetaElement {
  const meta = document.createElement('meta');
  meta.name = 'term-llm-extension-icons';
  meta.dataset.extension = id;
  meta.dataset.symbols = symbols;
  meta.content = `extensions/${generation}/${id}/icons.svg`;
  document.head.append(meta);
  return meta;
}

function iconStore(enabled: string[], entries: WebExtension[], disabled = false) {
  const status = { disabled, enabled, entries, errors: [], generation: 'current' };
  return {
    endpoints: { extensionsStatus: vi.fn(async () => status) },
    api: { url: (path: string) => `/ui${path}` },
    toast: vi.fn(),
  } as unknown as AppStore;
}

const sprite = (id: string, symbols: string[], extra: Partial<WebExtension> = {}) => ({
  id,
  title: id,
  description: '',
  icons: 'art/icons.svg',
  icon_symbols: symbols,
  ...extra,
});

const at = (path: string) => new URL(path, document.baseURI).href;

beforeEach(() => {
  removeRenderedExtensions();
  extensionRuntime.safeMode.value = false;
  extensionRuntime.errors.value = [];
});
afterEach(() => removeRenderedExtensions());

describe('extension icon sprites', () => {
  it('reads server-announced sprites, later extensions winning', () => {
    renderedIcons('first', 'g1', 'send copy');
    renderedIcons('second', 'g1', 'send');
    expect(iconOverridesFrom(renderedIconSprites(), document.baseURI)).toEqual({
      send: at('extensions/g1/second/icons.svg#send'),
      copy: at('extensions/g1/first/icons.svg#copy'),
    });
  });

  it('applies the status in enabled order and drops the announcements', async () => {
    renderedIcons('stale', 'old', 'send');
    iconOverrides.value = { send: at('extensions/old/stale/icons.svg#send') };
    await loadExtensions(
      iconStore(
        ['second', 'broken', 'first'],
        [
          sprite('first', ['send', 'copy']),
          sprite('second', ['send', 'trash']),
          sprite('broken', ['copy'], { error: 'icon sprite is not well-formed XML' }),
          sprite('disabled', ['menu']),
        ],
      ),
    );
    expect(iconOverrides.value).toEqual({
      send: at('/ui/extensions/current/first/art/icons.svg#send'),
      copy: at('/ui/extensions/current/first/art/icons.svg#copy'),
      trash: at('/ui/extensions/current/second/art/icons.svg#trash'),
    });
    expect(document.head.querySelector('meta[name="term-llm-extension-icons"]')).toBeNull();
  });

  it('escapes sprite paths and repoints icons drawn outside Preact', async () => {
    document.body.innerHTML = `<button>${iconHTML('copy')}</button>`;
    await loadExtensions(
      iconStore(['art'], [sprite('art', ['copy'], { icons: 'art/icons #1%.svg' })]),
    );
    const href = at('/ui/extensions/current/art/art/icons%20%231%25.svg#copy');
    expect(iconOverrides.value).toEqual({ copy: href });
    expect(document.querySelector('use')?.getAttribute('href')).toBe(href);

    removeRenderedExtensions();
    expect(document.querySelector('use')?.getAttribute('href')).toMatch(/icons[^#]*\.svg#copy$/);
  });

  it('keeps the announced icons when the status request fails', async () => {
    iconOverrides.value = { send: at('extensions/g1/art/icons.svg#send') };
    await loadExtensions({
      endpoints: { extensionsStatus: vi.fn(async () => Promise.reject(new Error('offline'))) },
    } as unknown as AppStore);
    expect(iconOverrides.value).toEqual({ send: at('extensions/g1/art/icons.svg#send') });
  });

  it('clears icons in safe mode and under the kill switch', async () => {
    renderedIcons('art', 'g1', 'send');
    iconOverrides.value = { send: at('extensions/g1/art/icons.svg#send') };
    await loadExtensions(iconStore(['art'], [sprite('art', ['send'])], true));
    expect(iconOverrides.value).toEqual({});

    renderedIcons('art', 'g1', 'send');
    iconOverrides.value = { send: at('extensions/g1/art/icons.svg#send') };
    extensionRuntime.safeMode.value = true;
    await loadExtensions(iconStore(['art'], [sprite('art', ['send'])]));
    expect(iconOverrides.value).toEqual({});
    expect(document.head.querySelector('meta[name="term-llm-extension-icons"]')).toBeNull();
  });

  it('gives extension scripts icons drawn like the app, replacements included', () => {
    document.body.innerHTML = '<div id="root"></div><div id="extension-mount"></div>';
    const store = new AppStore(readInjectedConfig());
    const host = extensionHost(store, 'clock');
    iconOverrides.value = { send: at('extensions/g1/art/icons.svg#send') };
    const send = host.icon('send');
    expect(send.namespaceURI).toBe('http://www.w3.org/2000/svg');
    expect(send.getAttribute('data-icon')).toBe('send');
    expect(send.getAttribute('aria-hidden')).toBe('true');
    expect(send.querySelector('use')?.getAttribute('href')).toBe(
      at('extensions/g1/art/icons.svg#send'),
    );
    expect(host.icon('copy').querySelector('use')?.getAttribute('href')).toMatch(
      /icons[^#]*\.svg#copy$/,
    );
    store.dispose();
  });
});

// Extension icon replacements: icon name -> "<sprite URL>#<symbol id>". The server
// announces each enabled extension sprite with a <meta name="term-llm-extension-icons">
// in <head> (internal/extensions/icons.go), so replacements draw from the first render;
// api/extensions.ts reconciles them with the extension status once the app starts.
import { signal } from '@preact/signals';

export type IconOverrides = Readonly<Record<string, string>>;

export interface IconSprite {
  /** Sprite URL, absolute or relative to the document base. */
  href: string;
  symbols: readonly string[];
}

/** Later sprites (later extensions in the enabled order) win. */
export function iconOverridesFrom(sprites: readonly IconSprite[], base: string): IconOverrides {
  const overrides: Record<string, string> = {};
  for (const sprite of sprites) {
    const href = new URL(sprite.href, base).href;
    for (const name of sprite.symbols) if (name) overrides[name] = `${href}#${name}`;
  }
  return overrides;
}

export function renderedIconSprites(doc: Document = document): IconSprite[] {
  return [
    ...doc.head.querySelectorAll<HTMLMetaElement>('meta[name="term-llm-extension-icons"]'),
  ].map((meta) => ({ href: meta.content, symbols: (meta.dataset.symbols ?? '').split(/\s+/) }));
}

export const iconOverrides = signal<IconOverrides>(
  typeof document === 'undefined' ? {} : iconOverridesFrom(renderedIconSprites(), document.baseURI),
);

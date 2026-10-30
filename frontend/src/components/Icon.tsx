// Icon shapes live in icons.svg, a content-addressed sprite the page preloads, not in
// the startup script. Each IconName is one <symbol> there (Icon.test.tsx checks both
// ways), and an extension's sprite may replace any of them (icon-overrides.ts).
import sprite from './icons.svg?url';
import { iconOverrides } from './icon-overrides';

export type IconName =
  | 'add'
  | 'alert-circle'
  | 'arrow-down'
  | 'arrow-down-left'
  | 'arrow-left'
  | 'arrow-right'
  | 'arrow-up'
  | 'arrow-up-right'
  | 'bot'
  | 'branch'
  | 'camera'
  | 'check'
  | 'chevron-down'
  | 'chevron-left'
  | 'chevron-right'
  | 'chevron-up'
  | 'chevrons-down-up'
  | 'chevrons-up-down'
  | 'clock'
  | 'close'
  | 'compact'
  | 'copy'
  | 'diff'
  | 'dock-bottom'
  | 'dock-right'
  | 'download'
  | 'edit'
  | 'expand'
  | 'file'
  | 'file-archive'
  | 'file-code'
  | 'file-link'
  | 'file-pen'
  | 'file-table'
  | 'file-text'
  | 'folder'
  | 'folder-search'
  | 'fork'
  | 'globe'
  | 'house'
  | 'image'
  | 'info'
  | 'live'
  | 'logout'
  | 'map-pin'
  | 'markdown'
  | 'menu'
  | 'message-circle-question'
  | 'microphone'
  | 'minus'
  | 'more'
  | 'panel'
  | 'play'
  | 'plug'
  | 'restore'
  | 'search'
  | 'send'
  | 'settings'
  | 'share'
  | 'sort'
  | 'sparkles'
  | 'steer'
  | 'stop'
  | 'target'
  | 'terminal'
  | 'text-search'
  | 'trash'
  | 'widgets'
  | 'wrench';

/** The <use> target for an icon: an extension's replacement, else the app's sprite. */
export function iconHref(name: IconName): string {
  return iconOverrides.value[name] ?? `${sprite}#${name}`;
}

/** The drawing <svg>'s attributes, which the sprite's shapes inherit. */
export const iconSVGAttributes = {
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  'stroke-width': '1.9',
  'stroke-linecap': 'round',
  'stroke-linejoin': 'round',
  'aria-hidden': 'true',
  focusable: 'false',
} as const;

/** Markup for an icon inside app-owned HTML strings (never untrusted content). */
export function iconHTML(name: IconName): string {
  const attributes = Object.entries({ 'data-icon': name, ...iconSVGAttributes })
    .map(([attribute, value]) => `${attribute}="${value}"`)
    .join(' ');
  const href = iconHref(name).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');
  return `<svg ${attributes}><use href="${href}"></use></svg>`;
}

/** Points icons drawn outside Preact (iconHTML, iconElement) at the current overrides. */
export function syncDetachedIcons(root: ParentNode = document): void {
  for (const use of root.querySelectorAll<SVGUseElement>('svg[data-icon] > use')) {
    const name = use.parentElement!.getAttribute('data-icon') as IconName;
    const href = iconHref(name);
    if (use.getAttribute('href') !== href) use.setAttribute('href', href);
  }
}

/** A detached icon element, for DOM built outside Preact (extension scripts). */
export function iconElement(name: string): SVGSVGElement {
  const svgNS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(svgNS, 'svg');
  for (const [attribute, value] of Object.entries({ 'data-icon': name, ...iconSVGAttributes }))
    svg.setAttribute(attribute, value);
  const use = document.createElementNS(svgNS, 'use');
  use.setAttribute('href', iconHref(name as IconName));
  svg.append(use);
  return svg;
}

export function Icon({ name, class: className }: { name: IconName; class?: string }) {
  return (
    <svg class={className} data-icon={name} {...iconSVGAttributes}>
      <use href={iconHref(name)} />
    </svg>
  );
}

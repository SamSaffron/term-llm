import { act, render } from '@testing-library/preact';
import { afterEach, describe, expect, it } from 'vitest';
import iconSource from './Icon.tsx?raw';
import spriteSource from './icons.svg?raw';
import { Icon } from './Icon';
import { iconOverrides } from './icon-overrides';

const iconNames = [...iconSource.matchAll(/^ {2}\| '([\w-]+)'/gm)].map((match) => match[1]);

// Every non-test source that can name an icon: files that import Icon or IconName.
const sources = Object.entries(
  import.meta.glob<string>(['../**/*.{ts,tsx}', '!../**/*.test.{ts,tsx}', '!./Icon.tsx'], {
    query: '?raw',
    import: 'default',
    eager: true,
  }),
).filter(([, text]) =>
  /\bimport\b[^;]*\b(Icon|IconName|iconHref|iconHTML)\b[^;]*from '[./]*\/?(components\/)?Icon'/.test(
    text,
  ),
);

describe('Icon', () => {
  afterEach(() => {
    iconOverrides.value = {};
  });

  it('has a sprite symbol for every icon name, and no others', () => {
    const symbols = [...spriteSource.matchAll(/<symbol id="([\w-]+)"/g)]
      .map((match) => match[1])
      .sort();
    expect(iconNames.length).toBeGreaterThan(0);
    expect(symbols).toEqual([...iconNames].sort());
  });

  it('ships only icons the app draws', () => {
    expect(sources.length).toBeGreaterThan(0);
    const unused = iconNames.filter(
      (name) =>
        !sources.some(([, text]) => text.includes(`'${name}'`) || text.includes(`"${name}"`)),
    );
    expect(unused).toEqual([]);
  });

  it('draws the named symbol from the sprite', () => {
    const { container } = render(<Icon name="send" class="arrow" />);
    const svg = container.querySelector('svg');
    expect(svg).toHaveClass('arrow');
    expect(svg).toHaveAttribute('data-icon', 'send');
    expect(svg).toHaveAttribute('aria-hidden', 'true');
    expect(svg?.querySelector('use')?.getAttribute('href')).toMatch(/icons[^#]*\.svg#send$/);
  });

  it('draws an extension replacement when one is set, and the sprite otherwise', () => {
    const { container } = render(
      <>
        <Icon name="send" />
        <Icon name="copy" />
      </>,
    );
    act(() => {
      iconOverrides.value = { send: 'https://example.test/ui/extensions/g/ext/icons.svg#send' };
    });
    const [send, copy] = [...container.querySelectorAll('use')].map((use) =>
      use.getAttribute('href'),
    );
    expect(send).toBe('https://example.test/ui/extensions/g/ext/icons.svg#send');
    expect(copy).toMatch(/icons[^#]*\.svg#copy$/);
  });
});

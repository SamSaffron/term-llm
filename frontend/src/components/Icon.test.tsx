import { render } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import iconSource from './Icon.tsx?raw';
import spriteSource from './icons.svg?raw';
import { Icon } from './Icon';

describe('Icon', () => {
  it('has a sprite symbol for every icon name, and no others', () => {
    const names = [...iconSource.matchAll(/\| '([\w-]+)'/g)].map((match) => match[1]).sort();
    const symbols = [...spriteSource.matchAll(/<symbol id="([\w-]+)"/g)]
      .map((match) => match[1])
      .sort();
    expect(names.length).toBeGreaterThan(0);
    expect(symbols).toEqual(names);
  });

  it('draws the named symbol from the sprite', () => {
    const { container } = render(<Icon name="send" class="arrow" />);
    const svg = container.querySelector('svg');
    expect(svg).toHaveClass('arrow');
    expect(svg).toHaveAttribute('aria-hidden', 'true');
    expect(svg?.querySelector('use')?.getAttribute('href')).toMatch(/icons[^#]*\.svg#send$/);
  });
});

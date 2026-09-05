import { describe, expect, it } from 'vitest';
import { createMessageMediaResolvers } from './media-resolvers';
import type { MarkdownMediaTarget } from './markdown';

describe('per-message media resolution', () => {
  it('invalidates only rows that requested a changed reference', () => {
    const resolverFor = createMessageMediaResolvers();
    const media = new Map<string, MarkdownMediaTarget>();
    const plain = resolverFor('plain', media);
    const missing = resolverFor('missing', media);
    expect(missing('a')).toBeUndefined();
    media.set('b', { url: '/b.png', type: 'image' });
    expect(resolverFor('plain', media)).toBe(plain);
    expect(resolverFor('missing', media)).toBe(missing);
    media.set('a', { url: '/a.png', type: 'image' });
    const resolved = resolverFor('missing', media);
    expect(resolved).not.toBe(missing);
    expect(resolved('a')?.url).toBe('/a.png');
    expect(resolverFor('plain', media)).toBe(plain);
    const equal = new Map(media);
    equal.set('a', { url: '/a.png', type: 'image' });
    expect(resolverFor('missing', equal)).toBe(resolved);
    equal.delete('a');
    expect(resolverFor('missing', equal)).not.toBe(resolved);
  });

  it('lets a stable resolver read new references in growing content from the latest map', () => {
    const resolverFor = createMessageMediaResolvers();
    const resolve = resolverFor('stream', new Map());
    const target: MarkdownMediaTarget = { url: '/new.webm', type: 'video' };
    expect(resolverFor('stream', new Map([['new', target]]))).toBe(resolve);
    expect(resolve('NEW')).toBe(target);
    expect(resolverFor('stream', new Map())).not.toBe(resolve);
  });
});

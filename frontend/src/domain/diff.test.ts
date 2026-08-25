import { describe, expect, it } from 'vitest';
import { fileKind, linesFromHunks } from './diff';

describe('structured diff contracts', () => {
  it('converts server hunks into numbered display lines', () => {
    expect(linesFromHunks([{ old_start: 4, new_start: 7, lines: [
      { t: 'ctx', s: 'same' }, { t: 'del', s: 'old' }, { t: 'add', s: 'new' }, { t: 'ctx', s: 'tail' },
    ] }])).toEqual([
      { kind: 'hunk', content: '@@ -4 +7 @@' },
      { kind: 'context', content: 'same', oldLine: 4, newLine: 7 },
      { kind: 'delete', content: 'old', oldLine: 5 },
      { kind: 'add', content: 'new', newLine: 8 },
      { kind: 'context', content: 'tail', oldLine: 6, newLine: 9 },
    ]);
  });

  it('recognizes the server create kind as an added file', () => {
    expect(fileKind({ path: 'new.go', status: 'create' })).toBe('add');
  });
});

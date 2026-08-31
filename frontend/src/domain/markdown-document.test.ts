import { describe, expect, it } from 'vitest';
import { markdownDocumentBlocks } from './markdown-document';

function byType(blocks: ReturnType<typeof markdownDocumentBlocks>, type: string) {
  return blocks.filter((block) => block.type === type);
}

describe('Markdown document source mapping', () => {
  it('maps repeated and nested top-level blocks to exact CRLF source ranges', () => {
    const source = [
      '# Tïtle 📝',
      '',
      'Repeated paragraph.',
      '',
      'Repeated paragraph.',
      '',
      '> quote',
      '> continued',
      '',
      '- one',
      '  - nested',
      '',
      '| A | B |',
      '| - | - |',
      '| 1 | 2 |',
      '',
      '```ts',
      'const value = 1;',
      '```',
      '',
      '---',
      '',
      '<div>HTML block</div>',
      '',
      '[ref]: https://example.com',
      '',
      'Uses [ref].',
    ].join('\r\n');
    const blocks = markdownDocumentBlocks(source);

    expect(byType(blocks, 'heading')[0]).toMatchObject({ startLine: 1, endLine: 1, anchorLine: 1 });
    expect(byType(blocks, 'paragraph')).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ startLine: 3, endLine: 3, anchorLine: 3 }),
        expect.objectContaining({ startLine: 5, endLine: 5, anchorLine: 5 }),
        expect.objectContaining({ startLine: 27, endLine: 27, anchorLine: 27 }),
      ]),
    );
    expect(byType(blocks, 'blockquote')[0]).toMatchObject({ startLine: 7, endLine: 8 });
    expect(byType(blocks, 'list')[0]).toMatchObject({ startLine: 10, endLine: 11 });
    expect(byType(blocks, 'table')[0]).toMatchObject({ startLine: 13, endLine: 15 });
    expect(byType(blocks, 'code')[0]).toMatchObject({ startLine: 17, endLine: 19 });
    expect(byType(blocks, 'hr')[0]).toMatchObject({ startLine: 21, endLine: 21 });
    expect(byType(blocks, 'html')[0]).toMatchObject({ startLine: 23, endLine: 23 });
    expect(blocks.some((block) => block.startLine === 25)).toBe(false);
    expect(blocks.at(-1)?.html).toContain('href="https://example.com"');
    expect(blocks.every((block) => block.commentable)).toBe(true);
  });

  it('keeps document line breaks, protected math, and sanitization isolated from chat mode', () => {
    const source = [
      'first line',
      'second line',
      '',
      '$$',
      'x_1 + y_2',
      '$$',
      '',
      'after math',
      '',
      '<script>alert(1)</script>',
    ].join('\n');
    const blocks = markdownDocumentBlocks(source);
    expect(blocks[0].html).not.toContain('<br');
    expect(blocks.find((block) => block.startLine === 4)).toMatchObject({ endLine: 6 });
    expect(blocks.find((block) => block.startLine === 8)).toBeTruthy();
    expect(blocks.map((block) => block.html).join('')).not.toContain('<script');
  });
});

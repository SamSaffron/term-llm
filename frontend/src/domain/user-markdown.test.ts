import { describe, expect, it } from 'vitest';
import { renderMarkdown } from './markdown';

describe('user message Markdown', () => {
  it('formats Markdown and autolinks URLs without linking inline code or trailing punctuation', () => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(
      'this is a test `test` https://meta.discourse.org.\n**bold**, *emphasis*, ~~removed~~\n`https://example.com`',
      undefined,
      'user',
    );
    expect(root.querySelector('code')).toHaveTextContent('test');
    expect(root.querySelector('strong')).toHaveTextContent('bold');
    expect(root.querySelector('em')).toHaveTextContent('emphasis');
    expect(root.querySelector('del')).toHaveTextContent('removed');
    expect(root.querySelectorAll('br')).toHaveLength(2);
    const links = root.querySelectorAll('a');
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveAttribute('href', 'https://meta.discourse.org');
    expect(links[0]).toHaveAttribute('target', '_blank');
    expect(links[0]).toHaveAttribute('rel', 'noopener noreferrer');
    expect(root.textContent).toContain('https://meta.discourse.org.');
    expect(root.querySelector('code a')).toBeNull();
  });

  it('renders lists, quotes, and fenced code blocks', () => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(
      '- one\n- two\n\n> quoted\n\n```text\n<tag> & value\n```',
      undefined,
      'user',
    );
    expect(root.querySelectorAll('ul > li')).toHaveLength(2);
    expect(root.querySelector('blockquote')).toHaveTextContent('quoted');
    expect(root.querySelector('pre code')).toHaveTextContent('<tag> & value');
    expect(root.querySelector('tag')).toBeNull();
  });

  it('keeps raw HTML literal and renders images as safe links rather than fetched media', () => {
    const root = document.createElement('div');
    const html = '<img src="https://example.com/tracker" onerror="alert(1)">';
    root.innerHTML = renderMarkdown(
      `${html}\n\n![diagram](https://example.com/diagram.png "A diagram")\n\n<script>alert(1)</script>`,
      undefined,
      'user',
    );
    expect(root.textContent).toContain(html);
    expect(root.textContent).toContain('<script>alert(1)</script>');
    expect(root.querySelector('img, video, audio, script, [onerror]')).toBeNull();
    const link = root.querySelector('a[title="A diagram"]')!;
    expect(link).toHaveTextContent('diagram');
    expect(link).toHaveAttribute('href', 'https://example.com/diagram.png');
    expect(link).toHaveAttribute('title', 'A diagram');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('preserves line breaks and Markdown inside literal XML-style tags', () => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(
      '<instructions>\nDo X\nDo Y\n</instructions>\n\n<rules>\n- one\n- two\n</rules>',
      undefined,
      'user',
    );
    expect(root.querySelector('p')!.querySelectorAll('br')).toHaveLength(3);
    expect(root.querySelector('p')).toHaveTextContent('<instructions>Do XDo Y</instructions>');
    expect(root.querySelectorAll('li')).toHaveLength(2);
    expect(root.querySelector('instructions, rules')).toBeNull();
  });

  it.each(['code', 'pre', 'kbd', 'script'])(
    'does not enter raw HTML mode after a literal <%s> tag',
    (tag) => {
      const root = document.createElement('div');
      const source = `Use <${tag}> then <img src=//tracker.example/x.png title=x`;
      root.innerHTML = renderMarkdown(source, undefined, 'user');
      expect(root.querySelector('img, script, code, pre, kbd')).toBeNull();
      expect(root.textContent).toBe(`${source}\n`);
    },
  );

  it('keeps autolinks working after literal anchor tags', () => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(
      'Use <a href="/"> then https://example.com/one\n\nhttps://example.com/two',
      undefined,
      'user',
    );
    expect([...root.querySelectorAll('a')].map((link) => link.getAttribute('href'))).toEqual([
      'https://example.com/one',
      'https://example.com/two',
    ]);
    expect(root.textContent).toContain('<a href="/">');
  });

  it('keeps linked image labels on their outer link without creating nested anchors', () => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(
      '[![Build](https://img.shields.io/x.svg)](https://ci.example.com)',
      undefined,
      'user',
    );
    expect(root.querySelectorAll('a')).toHaveLength(1);
    expect(root.querySelector('a')).toHaveAttribute('href', 'https://ci.example.com');
    expect(root.querySelector('a')).toHaveTextContent('Build');
    expect(root.querySelector('img')).toBeNull();
  });

  it('uses the URL as the label for images without alt text', () => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown('![](https://example.com/image.png)', undefined, 'user');
    expect(root.querySelector('a')).toHaveTextContent('https://example.com/image.png');
    expect(root.querySelector('img')).toBeNull();
  });

  it('sanitizes unsafe links, including image links', () => {
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(
      '[bad](javascript:alert%281%29) ![bad image](javascript:alert%281%29)',
      undefined,
      'user',
    );
    expect(root.querySelectorAll('a')).toHaveLength(2);
    expect(root.querySelector('[href]')).toBeNull();
    expect(root.querySelector('img')).toBeNull();
  });

  it('does not change the assistant Markdown policy', () => {
    const source = '<strong>HTML</strong>\n\n![image](https://example.com/image.png)';
    renderMarkdown(source, undefined, 'user');
    const root = document.createElement('div');
    root.innerHTML = renderMarkdown(source);
    expect(root.querySelector('strong')).toHaveTextContent('HTML');
    expect(root.querySelector('img')).toHaveAttribute('src', 'https://example.com/image.png');
  });
});

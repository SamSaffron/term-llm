import { describe, expect, it } from 'vitest';
import { decorateRichContent, needsHighlight, renderMarkdown } from './markdown';
import { highlight, highlightDiffLine } from './rich-highlight';

function renderFence(source: string, language = ''): HTMLDivElement {
  const root = document.createElement('div');
  root.innerHTML = renderMarkdown(`\`\`\`${language}\n${source}\n\`\`\``);
  return root;
}

describe('fenced code highlighting', () => {
  it('leaves unlabelled fences readable and unhighlighted', async () => {
    const source = 'const value = "<script>alert(1)</script>";';
    const root = renderFence(source);
    expect(needsHighlight(root)).toBe(false);
    await decorateRichContent(root, '');
    const block = root.querySelector('code')!;
    expect(block.textContent).toBe(`${source}\n`);
    expect(block.children).toHaveLength(0);
    expect(block).not.toHaveClass('hljs');
    expect(root.querySelector('script')).toBeNull();
  });

  it('highlights normal known-labelled fences without changing their text', async () => {
    const source = 'const value = "<tag>";';
    const root = renderFence(source, 'javascript');
    expect(needsHighlight(root)).toBe(true);
    await decorateRichContent(root, '');
    const block = root.querySelector('code')!;
    expect(block).toHaveClass('hljs');
    expect(block.querySelector('.hljs-keyword')).toHaveTextContent('const');
    expect(block.textContent).toBe(`${source}\n`);
    expect(block.dataset.highlighted).toBe('yes');
  });

  it.each([
    ['js', 'javascript', 'const value = 1;'],
    ['ts', 'typescript', 'const value: number = 1;'],
    ['sh', 'bash', 'echo "$HOME"'],
    ['text', 'plaintext', '<plain> & text'],
    ['txt', 'plaintext', '<plain> & text'],
    ['gitattributes', 'plaintext', '*.js text'],
    ['py', 'python', 'def hello(): return True'],
    ['rb', 'ruby', 'def hello; true; end'],
    ['rs', 'rust', 'fn main() {}'],
    ['yml', 'yaml', 'enabled: true'],
    ['md', 'markdown', '# Heading'],
    ['cs', 'csharp', 'public class Example {}'],
    ['html', 'xml', '<p>Hello</p>'],
    ['toml', 'ini', '[section]\nenabled = true'],
  ])('renders %s labels using the %s grammar', async (alias, language, source) => {
    expect(highlight.getLanguage(alias)).toBe(highlight.getLanguage(language));
    expect(highlight.listLanguages()).not.toContain(alias);
    expect(highlightDiffLine(source, alias)).toBe(highlightDiffLine(source, language));
    const aliased = renderFence(source, alias);
    const canonical = renderFence(source, language);
    await decorateRichContent(aliased, '');
    await decorateRichContent(canonical, '');
    const block = aliased.querySelector('code')!;
    expect(block).toHaveClass('hljs');
    expect(block.dataset.highlighted).toBe('yes');
    expect(block.innerHTML).toBe(canonical.querySelector('code')!.innerHTML);
    expect(block.textContent).toBe(`${source}\n`);
  });

  it.each([
    [50_000, true],
    [50_001, false],
  ])('bounds highlighting at %i code characters', async (length, highlighted) => {
    // Include the trailing newline that the Markdown renderer adds to code.
    const prefix = 'const value = "<tag>";';
    const source = prefix + ' '.repeat(length - prefix.length - 1);
    const root = renderFence(source, 'javascript');
    expect(needsHighlight(root)).toBe(highlighted);
    await decorateRichContent(root, '');
    const block = root.querySelector('code')!;
    expect(block.textContent).toBe(`${source}\n`);
    expect(block.classList.contains('hljs')).toBe(highlighted);
    expect(Boolean(block.querySelector('.hljs-keyword'))).toBe(highlighted);
    if (!highlighted) expect(block.children).toHaveLength(0);
  });

  it('highlights eligible code alongside skipped blocks without changing their text', async () => {
    const source = '<script>alert(1)</script>' + 'x'.repeat(50_000);
    const root = renderFence(source, 'javascript');
    root.insertAdjacentHTML('beforeend', renderFence('<img src=x onerror=alert(1)>').innerHTML);
    root.insertAdjacentHTML('beforeend', renderFence('const value = 1;', 'js').innerHTML);
    expect(needsHighlight(root)).toBe(true);
    await decorateRichContent(root, '');
    const [large, unlabelled, labelled] = root.querySelectorAll('code');
    expect(large.textContent).toBe(`${source}\n`);
    expect(large.children).toHaveLength(0);
    expect(unlabelled.textContent).toBe('<img src=x onerror=alert(1)>\n');
    expect(unlabelled.children).toHaveLength(0);
    expect(labelled.querySelector('.hljs-keyword')).toHaveTextContent('const');
    expect(root.querySelector('script,img')).toBeNull();
    expect(needsHighlight(root)).toBe(false);
  });

  it('preserves unknown-language fences without highlighting', async () => {
    const source = 'const value = "<tag>";';
    const root = renderFence(source, 'unknown-fence');
    const before = root.querySelector('code')!.innerHTML;
    await decorateRichContent(root, '');
    const block = root.querySelector('code')!;
    expect(block.innerHTML).toBe(before);
    expect(block).not.toHaveClass('hljs');
    expect(block.dataset.highlighted).toBe('yes');
  });

  it('does not redecorate already-highlighted code', async () => {
    const root = renderFence('const value = 1;', 'javascript');
    await decorateRichContent(root, '');
    const block = root.querySelector('code')!;
    expect(needsHighlight(root)).toBe(false);
    const keyword = block.querySelector('.hljs-keyword')!;
    const before = block.innerHTML;
    await decorateRichContent(root, '');
    expect(block.innerHTML).toBe(before);
    expect(block.querySelector('.hljs-keyword')).toBe(keyword);
  });
});

import { act, render } from '@testing-library/preact';
import { afterEach, expect, it, vi } from 'vitest';
import { useDiffHighlighting } from './useDiffHighlighting';

vi.mock('../domain/rich-highlight', () => new Promise(() => {}));
afterEach(() => vi.useRealTimers());

it('exposes usable plain diff rows if the optional highlighter never loads', async () => {
  vi.useFakeTimers();
  function Preview() {
    const { pending } = useDiffHighlighting(
      [{ kind: 'add', content: 'const answer = 42;', newLine: 1 }],
      'js',
      1,
      true,
    );
    return <div>{pending ? 'Preparing code…' : 'const answer = 42;'}</div>;
  }
  const view = render(<Preview />);
  await act(async () => {});
  expect(view.container).toHaveTextContent('Preparing code…');
  await act(async () => {
    await vi.advanceTimersByTimeAsync(250);
  });
  expect(view.container).toHaveTextContent('const answer = 42;');
});

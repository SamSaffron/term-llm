import { act, fireEvent, render, screen, waitFor } from '@testing-library/preact';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { StoreContext } from '../app/context';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Transcript } from './Transcript';

const stores: AppStore[] = [];
afterEach(() => stores.splice(0).forEach((store) => store.dispose()));

function renderUserMessage(content: string) {
  const store = new AppStore(testConfig);
  stores.push(store);
  store.sessions.value = [
    testSession({ messages: [{ id: 'u1', role: 'user', content, created: 1 }] }),
  ];
  store.activeSessionId.value = 's1';
  store.draftActive.value = false;
  const view = render(
    <StoreContext.Provider value={store}>
      <Transcript />
    </StoreContext.Provider>,
  );
  return { ...view, store };
}

describe('formatted user messages', () => {
  it('renders inline code and autolinks while copying the original Markdown', async () => {
    const source = 'this is a test `test` https://meta.discourse.org\nnext line';
    const descriptor = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    try {
      const { container, store } = renderUserMessage(source);
      const body = container.querySelector('.message.user .markdown-body')!;
      expect(body.querySelector('code')).toHaveTextContent('test');
      expect(body.querySelector('a')).toHaveAttribute('href', 'https://meta.discourse.org');
      expect(body.querySelector('br')).not.toBeNull();
      expect(body).not.toHaveClass('streaming-markdown');
      await act(async () => {
        fireEvent.click(screen.getByRole('button', { name: 'Copy message' }));
      });
      expect(writeText).toHaveBeenCalledWith(source);
      expect(store.sessions.value[0].messages[0].content).toBe(source);
    } finally {
      Object.defineProperty(
        navigator,
        'clipboard',
        descriptor || { configurable: true, value: undefined },
      );
    }
  });

  it('renders lists and code blocks with code-copy controls', async () => {
    const { container } = renderUserMessage('- one\n- two\n\n```text\n<tag> & value\n```');
    await waitFor(() => {
      expect(container.querySelector('.message.user pre code')).toHaveTextContent('<tag> & value');
    });
    expect(container.querySelectorAll('.message.user li')).toHaveLength(2);
    expect(screen.getByRole('button', { name: 'Copy code' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy message' })).toBeInTheDocument();
  });

  it('keeps pasted HTML visible and image URLs clickable without embedding them', () => {
    const source =
      '<img src="https://example.com/tracker">\n\n![plot](https://example.com/plot.png)';
    const { container } = renderUserMessage(source);
    const body = container.querySelector('.message.user .markdown-body')!;
    expect(body).toHaveTextContent('<img src="https://example.com/tracker">');
    expect(body.querySelector('img')).toBeNull();
    expect(body.querySelector('a[href="https://example.com/plot.png"]')).toHaveTextContent('plot');
  });
});

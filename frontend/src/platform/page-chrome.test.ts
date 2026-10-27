import { afterEach, describe, expect, it, vi } from 'vitest';
import { followChatGutter, scrollbarWidth, syncThemeColor } from './page-chrome';

describe('scrollbarWidth', () => {
  it('is the gap between outer and inner widths', () => {
    expect(scrollbarWidth(1000, 990)).toBe(10);
    expect(scrollbarWidth(1000, 983)).toBe(17);
  });

  it('is 0 for floating scrollbars and for numbers that make no sense', () => {
    expect(scrollbarWidth(1000, 1000)).toBe(0);
    expect(scrollbarWidth(1000, 1004)).toBe(0);
    expect(scrollbarWidth(1000, 900)).toBe(0);
    expect(scrollbarWidth(Number.NaN, Number.NaN)).toBe(0);
  });
});

describe('followChatGutter', () => {
  afterEach(() => document.documentElement.style.removeProperty('--chat-gutter'));

  it('puts the chat scrollbar width on the page', () => {
    const chat = document.createElement('section');
    Object.defineProperty(chat, 'offsetWidth', { value: 800 });
    Object.defineProperty(chat, 'clientWidth', { value: 785 });
    const stop = followChatGutter(chat);
    expect(document.documentElement.style.getPropertyValue('--chat-gutter')).toBe('15px');
    stop();
  });

  it('does nothing without a chat', () => {
    followChatGutter(null)();
    expect(document.documentElement.style.getPropertyValue('--chat-gutter')).toBe('');
  });
});

describe('syncThemeColor', () => {
  afterEach(() => {
    document.head.querySelectorAll('meta[name="theme-color"]').forEach((tag) => tag.remove());
    document.documentElement.style.removeProperty('--bg');
    vi.restoreAllMocks();
  });

  it('paints the current scheme’s bar in the page colour and leaves the other', () => {
    document.head.insertAdjacentHTML(
      'beforeend',
      '<meta name="theme-color" content="#212121" media="(prefers-color-scheme: dark)">' +
        '<meta name="theme-color" content="#ffffff" media="(prefers-color-scheme: light)">',
    );
    document.documentElement.style.setProperty('--bg', '#0b191b');
    vi.spyOn(window, 'matchMedia').mockReturnValue({ matches: true } as MediaQueryList);
    syncThemeColor();
    const [dark, light] = document.head.querySelectorAll('meta[name="theme-color"]');
    expect(dark.getAttribute('content')).toBe('#0b191b');
    expect(light.getAttribute('content')).toBe('#ffffff');
  });
});

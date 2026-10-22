import { describe, expect, it } from 'vitest';
import {
  clampSidebarWidth,
  DEFAULT_SIDEBAR_WIDTH,
  MAX_SIDEBAR_WIDTH,
  MIN_SIDEBAR_WIDTH,
} from './sidebar';

describe('clampSidebarWidth', () => {
  it('keeps a width inside the minimum and maximum', () => {
    expect(clampSidebarWidth(300, 1600)).toBe(300);
    expect(clampSidebarWidth(100, 1600)).toBe(MIN_SIDEBAR_WIDTH);
    expect(clampSidebarWidth(9999, 3000)).toBe(MAX_SIDEBAR_WIDTH);
    expect(clampSidebarWidth(430.4, 1600)).toBe(430);
  });

  it('leaves the conversation at least 55% of a narrow window', () => {
    expect(clampSidebarWidth(700, 1000)).toBe(450);
    expect(clampSidebarWidth(700, 300)).toBe(MIN_SIDEBAR_WIDTH);
  });

  it('falls back to the default for anything that is not a number', () => {
    expect(clampSidebarWidth(Number.NaN, 1600)).toBe(DEFAULT_SIDEBAR_WIDTH);
    expect(clampSidebarWidth(undefined as unknown as number, 1600)).toBe(DEFAULT_SIDEBAR_WIDTH);
  });
});

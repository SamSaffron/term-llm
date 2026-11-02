import { beforeEach, describe, expect, it } from 'vitest';
import { ThinkingStore } from './thinking-store';
import { DEFAULT_THINKING, RAW_BLOCKED, THINKING_USAGE } from '../domain/thinking';
import { STORAGE_BASE_KEYS } from '../platform/storage';
import type { Message } from '../domain/types';
const thought: Message = {
  id: 'one',
  role: 'reasoning',
  content: 'body',
  created: 1,
  reasoningKind: 'summary',
};
beforeEach(() => localStorage.clear());
describe('thinking store', () => {
  it('defaults to off before capabilities and when reasoning capabilities are absent', () => {
    const store = new ThinkingStore(localStorage);
    expect(store.display.value).toBe('off');
    expect(store.visible(thought)).toBe(false);
    store.applyCapabilities(undefined);
    expect(store.display.value).toBe('off');
    store.applyCapabilities({ display: 'collapsed' });
    expect(store.visible(thought)).toBe(true);
    store.command('off');
    store.applyCapabilities({ display: 'expanded' });
    expect(store.display.value).toBe('off');
  });
  it.each(['off', 'status', 'collapsed', 'expanded', 'auto'])(
    'handles %s and persists browser preference',
    (mode) => {
      const store = new ThinkingStore(localStorage);
      const expected = mode === 'auto' ? 'collapsed' : mode;
      expect(store.command(mode)).toEqual({
        message: `Reasoning display: ${expected}`,
        kind: 'success',
      });
      expect(new ThinkingStore(localStorage).display.value).toBe(expected);
    },
  );
  it('cycles, warns without changing mode, gates raw, and falls back when the gate changes', () => {
    const store = new ThinkingStore(localStorage);
    expect(store.command().message).toBe('Reasoning display: collapsed');
    expect(store.command().message).toBe('Reasoning display: expanded');
    expect(store.command().message).toBe('Reasoning display: off');
    expect(store.command().message).toBe('Reasoning display: collapsed');
    expect(store.command('bad')).toEqual({ message: THINKING_USAGE, kind: 'error' });
    expect(store.command('raw')).toEqual({ message: RAW_BLOCKED, kind: 'error' });
    expect(store.display.value).toBe('collapsed');
    store.applyCapabilities({ ...DEFAULT_THINKING, raw: true });
    expect(store.command('raw').message).toBe('Reasoning display: raw visible');
    const restored = new ThinkingStore(localStorage);
    expect(restored.display.value).toBe('collapsed');
    restored.applyCapabilities({ ...DEFAULT_THINKING, raw: true });
    expect(restored.display.value).toBe('raw');
  });
  it('uses server default only when unset and keeps Ctrl+E transient, clearing block overrides on mode changes', () => {
    const store = new ThinkingStore(localStorage);
    store.applyCapabilities({ ...DEFAULT_THINKING, display: 'expanded' });
    expect(store.display.value).toBe('expanded');
    store.command('collapsed');
    store.toggleThought('one', false);
    expect(store.expanded(thought)).toBe(true);
    store.toggleDetails();
    expect(store.display.value).toBe('expanded');
    expect(store.overrides.value).toEqual({});
    expect(store.detailsEpoch.value).toBe(1);
    expect(localStorage.getItem(STORAGE_BASE_KEYS.thinkingDisplay)).toBe('collapsed');
    store.toggleDetails();
    expect(store.expanded(thought)).toBe(false);
    store.toggleThought('one', false);
    store.command('collapsed');
    expect(store.overrides.value).toEqual({});
    expect(store.detailMode.value).toBeNull();
  });
  it.each(['off', 'status', 'raw'])('only toggles tools in %s', (mode) => {
    const store = new ThinkingStore(localStorage);
    store.applyCapabilities({ ...DEFAULT_THINKING, raw: true });
    store.command(mode);
    store.toggleDetails();
    expect(store.display.value).toBe(mode);
    expect(store.detailsExpanded.value).toBe(true);
  });
});

it('preserves a clicked override across identical capabilities and clears it when the effective mode changes', () => {
  const store = new ThinkingStore(localStorage);
  store.applyCapabilities({ ...DEFAULT_THINKING, display: 'collapsed' });
  store.toggleThought('one', false);
  store.applyCapabilities({ ...DEFAULT_THINKING, display: 'collapsed' });
  expect(store.expanded(thought)).toBe(true);
  store.applyCapabilities({ ...DEFAULT_THINKING, display: 'off' });
  expect(store.overrides.value).toEqual({});
});

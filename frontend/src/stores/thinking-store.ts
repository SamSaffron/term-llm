import { computed, signal } from '@preact/signals';
import {
  DEFAULT_THINKING,
  RAW_BLOCKED,
  THINKING_USAGE,
  cycleThinking,
  effectiveDisplay,
  historyExpanded,
  historyVisible,
  parseThinkingDisplay,
  thinkingStatus,
  type ThinkingDisplay,
  type ThinkingPolicy,
} from '../domain/thinking';
import type { Message } from '../domain/types';
import { STORAGE_BASE_KEYS } from '../platform/storage';

export class ThinkingStore {
  readonly serverPolicy = signal<ThinkingPolicy>(DEFAULT_THINKING);
  readonly preference = signal<ThinkingDisplay | null>(null);
  readonly detailMode = signal<ThinkingDisplay | null>(null);
  readonly detailsExpanded = signal(false);
  readonly detailsEpoch = signal(0);
  readonly overrides = signal<Record<string, boolean>>({});
  readonly policy = computed(() => ({
    ...this.serverPolicy.value,
    display: this.detailMode.value || this.preference.value || this.serverPolicy.value.display,
  }));
  readonly display = computed(() => effectiveDisplay(this.policy.value));
  constructor(private readonly storage: Storage) {
    try {
      this.preference.value = parseThinkingDisplay(
        storage.getItem(STORAGE_BASE_KEYS.thinkingDisplay) || '',
      );
    } catch {
      /* Storage denial does not block chat. */
    }
  }
  applyCapabilities(value: unknown): void {
    const previousDisplay = this.display.peek();
    const source = value && typeof value === 'object' ? (value as Partial<ThinkingPolicy>) : {};
    this.serverPolicy.value = { ...DEFAULT_THINKING, ...source };
    if (this.display.peek() !== previousDisplay) this.overrides.value = {};
  }
  command(arg = ''): { message: string; kind: 'success' | 'error' } {
    const mode = arg.trim() ? parseThinkingDisplay(arg) : cycleThinking(this.display.value);
    if (!mode) return { message: THINKING_USAGE, kind: 'error' };
    if (mode === 'raw' && !this.serverPolicy.value.raw)
      return { message: RAW_BLOCKED, kind: 'error' };
    this.preference.value = mode;
    this.detailMode.value = null;
    this.overrides.value = {};
    try {
      this.storage.setItem(STORAGE_BASE_KEYS.thinkingDisplay, mode);
    } catch {
      /* Keep the in-memory preference. */
    }
    return {
      message: `Reasoning display: ${mode === 'raw' ? 'raw visible' : mode}`,
      kind: 'success',
    };
  }
  toggleDetails(): void {
    this.detailsExpanded.value = !this.detailsExpanded.value;
    this.detailsEpoch.value++;
    if (['collapsed', 'expanded'].includes(this.display.value)) {
      this.detailMode.value = this.detailsExpanded.value ? 'expanded' : 'collapsed';
      this.overrides.value = {};
    }
  }
  toggleThought(id: string, expanded: boolean): void {
    this.overrides.value = { ...this.overrides.value, [id]: !expanded };
  }
  visible(message: Message): boolean {
    return historyVisible(message.reasoningKind || '', this.policy.value);
  }
  expanded(message: Message): boolean {
    return (
      this.overrides.value[message.id] ??
      historyExpanded(message.reasoningKind || '', this.policy.value)
    );
  }
  statusText(message: Message): string {
    return thinkingStatus(message, this.policy.value);
  }
}

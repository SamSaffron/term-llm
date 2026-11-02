import type { Message } from './types';
import type { Completion } from './completions';

export type ThinkingDisplay = 'off' | 'status' | 'collapsed' | 'expanded' | 'raw';
export interface ThinkingPolicy {
  display: string;
  status: string;
  history: string;
  raw: boolean;
  hidden_label: string;
}
export const DEFAULT_THINKING: ThinkingPolicy = {
  display: 'off',
  status: 'title',
  history: 'collapsed',
  raw: false,
  hidden_label: 'Thinking...',
};
export const THINKING_USAGE = 'Usage: /thinking [off|status|collapsed|expanded|raw]';
export const RAW_BLOCKED =
  'Raw reasoning is disabled. Set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.';
export function parseThinkingDisplay(value: string): ThinkingDisplay | null {
  const mode = value.trim().toLowerCase();
  if (mode === 'auto') return 'collapsed';
  return ['off', 'status', 'collapsed', 'expanded', 'raw'].includes(mode)
    ? (mode as ThinkingDisplay)
    : null;
}
export function effectiveDisplay(cfg: ThinkingPolicy): ThinkingDisplay {
  const mode = parseThinkingDisplay(cfg.display) || 'collapsed';
  return mode === 'raw' && !cfg.raw ? 'collapsed' : mode;
}
export function cycleThinking(mode: ThinkingDisplay): ThinkingDisplay {
  return mode === 'expanded' ? 'off' : mode === 'off' ? 'collapsed' : 'expanded';
}
export function historyMode(cfg: ThinkingPolicy): string {
  const mode = effectiveDisplay(cfg);
  if (mode === 'off' || mode === 'status') return 'none';
  const history = cfg.history.trim().toLowerCase();
  return ['none', 'collapsed', 'expanded', 'transcript_only'].includes(history)
    ? history
    : mode === 'expanded' || mode === 'raw'
      ? 'expanded'
      : 'collapsed';
}
export function historyVisible(kind: string, cfg: ThinkingPolicy): boolean {
  return (
    ['summary', 'raw'].includes(kind.trim().toLowerCase()) &&
    ['collapsed', 'expanded'].includes(historyMode(cfg))
  );
}
export function historyExpanded(kind: string, cfg: ThinkingPolicy): boolean {
  return (
    historyVisible(kind, cfg) &&
    (historyMode(cfg) === 'expanded' ||
      effectiveDisplay(cfg) === 'expanded' ||
      (kind.trim().toLowerCase() === 'raw' && effectiveDisplay(cfg) === 'raw'))
  );
}
export function statusEnabled(cfg: ThinkingPolicy): boolean {
  return effectiveDisplay(cfg) !== 'off' && cfg.status.trim().toLowerCase() !== 'none';
}
export function thinkingStatus(message: Message, cfg: ThinkingPolicy): string {
  const mode = cfg.status.trim().toLowerCase();
  if (!statusEnabled(cfg) || mode === 'generic') return 'Thinking';
  return (
    message.reasoningTitle?.trim() ||
    (mode === 'summary'
      ? Array.from(message.content.replace(/\s+/gu, ' ').trim()).slice(0, 80).join('')
      : '') ||
    'Thinking'
  );
}
const descriptions: Record<ThinkingDisplay, string> = {
  off: 'No thoughts; the status just says "Thinking"',
  status: 'Status line only: what the model is thinking about',
  collapsed: 'Thought rows; click one to read it',
  expanded: 'Every thought shown in full',
  raw: 'Raw provider thinking · needs reasoning.raw: true',
};
export function thinkingCompletions(
  value: string,
  mode: ThinkingDisplay,
  raw: boolean,
): Completion[] | null {
  const match = value.match(/^\/(thinking|reasoning)(?:\s+([^\s]*))?$/i);
  if (!match) return null;
  const start = match[2] === undefined ? value.length : value.length - match[2].length;
  const prefix = (match[2] || '').toLowerCase();
  return (Object.keys(descriptions) as ThinkingDisplay[])
    .filter((entry) => entry.startsWith(prefix))
    .map((entry) => ({
      value: entry,
      label: `/${match[1]} ${entry}`,
      description: descriptions[entry],
      kind: 'slash',
      streamingSafe: true,
      replacement: { start, end: value.length },
      thinking: true,
      current: entry === mode,
      unavailable: entry === 'raw' && !raw,
    }));
}
export function detailsShortcutLabel(): string {
  return /Mac|iPhone|iPad|iPod/i.test(navigator.platform) ? '⌃E' : 'Ctrl+E';
}

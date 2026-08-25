import type { DiffFile, DiffLine } from './types';

export const DIFF_SCOPES = ['last_turn', 'last_3_turns', 'uncommitted', 'unstaged', 'staged'] as const;
export type DiffScope = typeof DIFF_SCOPES[number];
export function normalizeDiffScope(value: unknown): DiffScope {
  const scope = String(value || '').trim().toLowerCase().replaceAll('-', '_');
  return DIFF_SCOPES.includes(scope as DiffScope) ? scope as DiffScope : 'last_turn';
}

export function parseUnifiedPatch(patch: string): DiffLine[] {
  let oldLine = 0; let newLine = 0;
  return String(patch || '').split('\n').map((raw) => {
    if (raw.startsWith('@@')) {
      const match = raw.match(/@@\s+-(\d+)(?:,\d+)?\s+\+(\d+)(?:,\d+)?\s+@@/);
      oldLine = Number(match?.[1] || 0); newLine = Number(match?.[2] || 0);
      return { kind: 'hunk', content: raw };
    }
    if (raw.startsWith('+') && !raw.startsWith('+++')) return { kind: 'add', content: raw.slice(1), newLine: newLine++ };
    if (raw.startsWith('-') && !raw.startsWith('---')) return { kind: 'delete', content: raw.slice(1), oldLine: oldLine++ };
    if (raw.startsWith('\\ No newline')) return { kind: 'context', content: raw };
    return { kind: 'context', content: raw.replace(/^ /, ''), oldLine: oldLine++, newLine: newLine++ };
  });
}

export function sortDiffFiles(files: DiffFile[]): DiffFile[] {
  return [...files].sort((left, right) => Number(right.lastChangedAt || 0) - Number(left.lastChangedAt || 0) || left.path.localeCompare(right.path));
}

function codePoints(value: string): string[] { return Array.from(value); }
export function inlineEmphasis(oldText: string, newText: string): { old: [number, number]; new: [number, number] } {
  const oldPoints = codePoints(oldText); const newPoints = codePoints(newText);
  let prefix = 0;
  while (prefix < oldPoints.length && prefix < newPoints.length && oldPoints[prefix] === newPoints[prefix]) prefix += 1;
  let suffix = 0;
  while (suffix < oldPoints.length - prefix && suffix < newPoints.length - prefix && oldPoints[oldPoints.length - 1 - suffix] === newPoints[newPoints.length - 1 - suffix]) suffix += 1;
  const unitOffset = (points: string[], count: number) => points.slice(0, count).join('').length;
  return {
    old: [unitOffset(oldPoints, prefix), unitOffset(oldPoints, oldPoints.length - suffix)],
    new: [unitOffset(newPoints, prefix), unitOffset(newPoints, newPoints.length - suffix)],
  };
}

export function unifiedPatchForFile(file: DiffFile): string {
  if (file.patch) return String(file.patch);
  return (file.lines || []).map((line) => line.kind === 'hunk' ? line.content : `${line.kind === 'add' ? '+' : line.kind === 'delete' ? '-' : ' '}${line.content}`).join('\n');
}

export function clampDiffWidth(value: number, viewport: number): number {
  return Math.max(320, Math.min(Math.max(320, viewport - 320), Number(value) || 420));
}

export function fileKind(file: DiffFile): 'add' | 'delete' | 'modify' | 'rename' {
  const value = String(file.status || '').toLowerCase();
  if (['a', 'add', 'added', 'created'].includes(value)) return 'add';
  if (['d', 'delete', 'deleted', 'removed'].includes(value)) return 'delete';
  if (['r', 'rename', 'renamed'].includes(value)) return 'rename';
  return 'modify';
}

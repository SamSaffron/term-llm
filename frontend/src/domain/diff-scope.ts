export const DIFF_SCOPES = [
  'last_turn',
  'last_3_turns',
  'uncommitted',
  'unstaged',
  'staged',
] as const;

export type DiffScope = (typeof DIFF_SCOPES)[number];

/**
 * Scope normalization is shared by eager review queueing and deferred diff
 * parsing. Keep it in this dependency-free module so review-store.ts can
 * normalize synchronously without pulling domain/diff.ts parsers into the
 * initial bundle.
 */
export function normalizeDiffScope(value: unknown): DiffScope {
  const scope = String(value || '')
    .trim()
    .toLowerCase()
    .replaceAll('-', '_');
  return DIFF_SCOPES.includes(scope as DiffScope) ? (scope as DiffScope) : 'last_turn';
}

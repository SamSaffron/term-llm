import { describe, expect, it } from 'vitest';
import { fileKind, isMarkdownPath, linesFromHunks, parseUnifiedDiffFiles } from './diff';

describe('structured diff contracts', () => {
  it('recognizes only Markdown document extensions', () => {
    expect(['README.md', 'PLAN.MD', 'notes.markdown'].map(isMarkdownPath)).toEqual([
      true,
      true,
      true,
    ]);
    expect(['notes.mdx', 'notes.md.bak', 'markdown', ''].map(isMarkdownPath)).toEqual([
      false,
      false,
      false,
      false,
    ]);
  });

  it('converts server hunks into numbered display lines', () => {
    expect(
      linesFromHunks([
        {
          old_start: 4,
          new_start: 7,
          lines: [
            { t: 'ctx', s: 'same' },
            { t: 'del', s: 'old' },
            { t: 'add', s: 'new' },
            { t: 'ctx', s: 'tail' },
          ],
        },
      ]),
    ).toEqual([
      { kind: 'hunk', content: '@@ -4 +7 @@' },
      { kind: 'context', content: 'same', oldLine: 4, newLine: 7 },
      { kind: 'delete', content: 'old', oldLine: 5 },
      { kind: 'add', content: 'new', newLine: 8 },
      { kind: 'context', content: 'tail', oldLine: 6, newLine: 9 },
    ]);
  });

  it('splits a worktree patch into highlighted file models', () => {
    const files = parseUnifiedDiffFiles(`diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -1 +1 @@
-const oldValue = 1;
+const newValue = 2;
diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1 @@
+package main
`);
    expect(files).toHaveLength(2);
    expect(files[0]).toMatchObject({
      path: 'app.ts',
      status: 'modify',
      additions: 1,
      deletions: 1,
      lang: 'ts',
      expanded: true,
    });
    expect(files[0].lines).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ kind: 'delete', content: 'const oldValue = 1;' }),
        expect.objectContaining({ kind: 'add', content: 'const newValue = 2;' }),
      ]),
    );
    expect(files[1]).toMatchObject({
      path: 'new.go',
      status: 'create',
      additions: 1,
      expanded: false,
    });
  });

  it('preserves rename provenance in worktree patches', () => {
    const files = parseUnifiedDiffFiles(`diff --git a/old-name.ts b/new-name.ts
similarity index 100%
rename from old-name.ts
rename to new-name.ts
`);
    expect(files).toEqual([
      expect.objectContaining({
        path: 'new-name.ts',
        old_path: 'old-name.ts',
        status: 'rename',
      }),
    ]);
  });

  it('recognizes the server create kind as an added file', () => {
    expect(fileKind({ path: 'new.go', status: 'create' })).toBe('add');
  });
});

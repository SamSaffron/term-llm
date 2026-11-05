import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterAll, describe, expect, it } from 'vitest';

const frontend = process.cwd();
const scratch = resolve(frontend, '../tmp');
mkdirSync(scratch, { recursive: true });
const fixtures = mkdtempSync(resolve(scratch, 'oxlint-'));
const cli = resolve(frontend, 'node_modules/oxlint/bin/oxlint');
afterAll(() => rmSync(fixtures, { recursive: true, force: true }));

describe('Oxlint configuration', () => {
  it.each([
    {
      name: 'conditional Preact hooks',
      source: `import { useState } from 'preact/hooks';
export function Component({ enabled }: { enabled: boolean }) {
  if (enabled) useState(0);
  return null;
}`,
      rule: 'rules-of-hooks',
    },
    {
      name: 'missing Preact hook dependencies',
      source: `import { useEffect } from 'preact/hooks';
export function Component({ value }: { value: string }) {
  useEffect(() => { console.log(value); }, []);
  return null;
}`,
      rule: 'exhaustive-deps',
    },
    { name: 'unused variables', source: 'const unused = 1;', rule: 'no-unused-vars' },
    {
      name: 'unused expressions',
      source: 'export function run() { 42; }',
      rule: 'no-unused-expressions',
    },
    {
      name: 'undefined JavaScript names',
      extension: 'js',
      source: 'missingFunction();',
      rule: 'no-undef',
    },
  ])('rejects $name', ({ source, rule, extension = 'tsx' }) => {
    const file = resolve(fixtures, `${rule}.${extension}`);
    writeFileSync(file, source);
    const result = spawnSync(
      process.execPath,
      [cli, '--config', '.oxlintrc.json', '--deny-warnings', file],
      {
        cwd: frontend,
        encoding: 'utf8',
      },
    );
    expect(result.error).toBeUndefined();
    expect(result.status).toBe(1);
    expect(result.stdout + result.stderr).toContain(rule);
  });

  it('allows type-only usage, explicit any, ignored parameters and caught errors', () => {
    const file = resolve(fixtures, 'allowed.ts');
    writeFileSync(
      file,
      `type Value = string;
export function run(_unused: any, value: Value): Value {
  try { return value; } catch (_error) { return ''; }
}`,
    );
    const result = spawnSync(
      process.execPath,
      [cli, '--config', '.oxlintrc.json', '--deny-warnings', file],
      {
        cwd: frontend,
        encoding: 'utf8',
      },
    );
    expect(result.error).toBeUndefined();
    expect(result.stdout + result.stderr).toContain('0 errors');
    expect(result.status).toBe(0);
  });
});

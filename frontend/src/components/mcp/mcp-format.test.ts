import { describe, expect, it } from 'vitest';
import type { MCPServer } from '../../domain/types';
import { mcpServerMeta, mcpServerTone, parseMCPPairs } from './mcp-format';

const server = (patch: Partial<MCPServer>): MCPServer => ({
  name: 'demo',
  configured: true,
  enabled: false,
  status: 'stopped',
  error: '',
  refreshWarning: '',
  tools: 0,
  active: 0,
  deferred: 0,
  loadingMode: '',
  ...patch,
});

describe('mcp-format', () => {
  it.each([
    [{ status: 'ready', tools: 1 }, 'ready', '1 tool'],
    [{ status: 'ready', tools: 12, deferred: 8 }, 'ready', '12 tools · 8 deferred'],
    [{ status: 'starting' }, 'starting', 'starting…'],
    [{ status: 'auth_required' }, 'auth', 'sign-in needed'],
    [{ status: 'failed', error: 'exit 1\nstack…' }, 'failed', 'exit 1'],
    [{ status: 'failed' }, 'failed', 'failed to start'],
    [{ configured: false, status: 'failed' }, 'failed', 'not in mcp.json'],
    [{ status: 'stopped' }, 'stopped', ''],
  ] as const)('summarizes %o', (patch, tone, meta) => {
    expect(mcpServerTone(server(patch))).toBe(tone);
    expect(mcpServerMeta(server(patch))).toBe(meta);
  });

  it('parses header and environment lines', () => {
    expect(parseMCPPairs('Authorization: Bearer a:b\n\n# skip\nX-Y:1', ':')).toEqual({
      values: { Authorization: 'Bearer a:b', 'X-Y': '1' },
      error: '',
    });
    expect(parseMCPPairs('TOKEN=a=b', '=')).toEqual({ values: { TOKEN: 'a=b' }, error: '' });
    expect(parseMCPPairs('ok=1\nbroken', '=').error).toBe('Line 2: expected NAME=value');
    expect(parseMCPPairs('=value', '=').error).toBe('Line 1: expected NAME=value');
  });
});

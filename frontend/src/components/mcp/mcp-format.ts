import type { MCPServer } from '../../domain/types';

export type MCPTone = 'ready' | 'starting' | 'auth' | 'failed' | 'stopped';

/** Collapses a server's lifecycle into the single state carried by its dot. */
export function mcpServerTone(server: MCPServer): MCPTone {
  if (!server.configured) return 'failed';
  const status = server.status.toLocaleLowerCase();
  if (status === 'ready') return 'ready';
  if (status === 'starting') return 'starting';
  if (status === 'auth_required') return 'auth';
  if (status === 'failed' || server.error) return 'failed';
  return 'stopped';
}

const firstLine = (value: string): string => value.split('\n', 1)[0].trim();

/** The one muted line shown beside a server name; empty when nothing matters. */
export function mcpServerMeta(server: MCPServer): string {
  if (!server.configured) return 'not in mcp.json';
  switch (mcpServerTone(server)) {
    case 'ready': {
      const tools = `${server.tools} tool${server.tools === 1 ? '' : 's'}`;
      return server.deferred ? `${tools} · ${server.deferred} deferred` : tools;
    }
    case 'starting':
      return 'starting…';
    case 'auth':
      return 'sign-in needed';
    case 'failed':
      return server.error ? firstLine(server.error) : 'failed to start';
    default:
      return '';
  }
}

/**
 * Parses one `key<separator>value` pair per line (blank lines and `#` comments
 * are ignored), as typed into the headers and environment fields.
 */
export function parseMCPPairs(
  text: string,
  separator: ':' | '=',
): { values: Record<string, string>; error: string } {
  const values: Record<string, string> = {};
  const lines = text.split('\n');
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index].trim();
    if (!line || line.startsWith('#')) continue;
    const at = line.indexOf(separator);
    const key = at > 0 ? line.slice(0, at).trim() : '';
    if (!key) {
      const example = separator === ':' ? 'Name: value' : 'NAME=value';
      return { values: {}, error: `Line ${index + 1}: expected ${example}` };
    }
    values[key] = line.slice(at + 1).trim();
  }
  return { values, error: '' };
}

import type { Message, PendingToolCall, ToolCall } from './types';

/**
 * Client tools are functions the browser page offers the model (via WebMCP)
 * and executes itself. The server forwards them as Responses API
 * "passthrough" tools: a call ends the run, and the page continues it with a
 * `function_call_output` item.
 */

/** Distinguishes page tools from server tools in the shared tool namespace. */
export const CLIENT_TOOL_PREFIX = 'webmcp__';

// Provider tool names allow at most 64 characters from this alphabet.
const TOOL_NAME = /^[A-Za-z0-9_-]+$/;
const MAX_TOOL_NAME = 64 - CLIENT_TOOL_PREFIX.length;

export interface ClientTool {
  name: string;
  title: string;
  description: string;
  inputSchema: Record<string, unknown>;
  readOnly: boolean;
}

export interface ClientToolDefinition {
  type: 'function';
  name: string;
  description: string;
  parameters: Record<string, unknown>;
}

export interface PendingClientCall {
  callId: string;
  /** The page's own tool name, without {@link CLIENT_TOOL_PREFIX}. */
  name: string;
  arguments: string;
}

export interface ClientToolOutputItem {
  type: 'function_call_output';
  call_id: string;
  output: string;
}

export function isClientToolName(name: string): boolean {
  return name.startsWith(CLIENT_TOOL_PREFIX);
}

export function isValidClientToolName(name: string): boolean {
  return name.length > 0 && name.length <= MAX_TOOL_NAME && TOOL_NAME.test(name);
}

/** Tool definitions for a Responses request. Invalid names are skipped. */
export function clientToolDefinitions(
  tools: ClientTool[],
  provider: string,
): ClientToolDefinition[] {
  const seen = new Set<string>();
  return tools.flatMap((tool) => {
    if (!isValidClientToolName(tool.name) || seen.has(tool.name)) return [];
    seen.add(tool.name);
    const where = provider ? ` Runs on the user's ${provider}.` : '';
    return [
      {
        type: 'function' as const,
        name: CLIENT_TOOL_PREFIX + tool.name,
        description: `${tool.description || tool.title || tool.name}${where}`.trim(),
        parameters: tool.inputSchema,
      },
    ];
  });
}

/**
 * Parses the server's `pending_client_calls` list. Undefined when the payload
 * has no list, so callers can tell an older server from "nothing pending".
 */
export function parsePendingToolCalls(value: unknown): PendingToolCall[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.flatMap((entry) => {
    if (!entry || typeof entry !== 'object') return [];
    const record = entry as Record<string, unknown>;
    const callId = typeof record.call_id === 'string' ? record.call_id : '';
    const name = typeof record.name === 'string' ? record.name : '';
    if (!callId || !name) return [];
    const args = typeof record.arguments === 'string' ? record.arguments : '';
    return [{ callId, name, arguments: args || '{}' }];
  });
}

/**
 * Client calls the server handed back when `responseId` ended.
 *
 * Servers report them explicitly (`reported`); that list is authoritative even
 * when empty. Older servers do not, so fall back to the transcript: the server
 * runs its own tools and keeps looping, so only calls after the last group that
 * contains a server tool can still be waiting. (Client calls made in the same
 * turn as server tools are dropped by the server, and must not be answered.)
 * That fallback misfires when tool-only provider turns share one group.
 */
export function pendingClientCalls(
  messages: Message[],
  responseId: string,
  reported?: PendingToolCall[],
): PendingClientCall[] {
  if (reported)
    return reported
      .filter((call) => isClientToolName(call.name))
      .map((call) => ({
        callId: call.callId,
        name: call.name.slice(CLIENT_TOOL_PREFIX.length),
        arguments: call.arguments || '{}',
      }));
  const groups = messages.filter(
    (message) => message.role === 'tool-group' && message.responseId === responseId,
  );
  const firstPending = groups.reduce(
    (start, group, index) =>
      (group.tools || []).some((tool) => !isClientToolName(tool.name)) ? index + 1 : start,
    0,
  );
  return groups
    .slice(firstPending)
    .flatMap((group) => group.tools || [])
    .filter((tool) => isClientToolName(tool.name))
    .map((tool) => ({
      callId: tool.id,
      name: tool.name.slice(CLIENT_TOOL_PREFIX.length),
      arguments: tool.arguments || '{}',
    }));
}

/** Parses model-supplied arguments; throws unless they form a JSON object. */
export function parseToolArguments(raw: string): Record<string, unknown> {
  const value: unknown = JSON.parse(raw.trim() || '{}');
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw new Error('Tool arguments must be a JSON object');
  return value as Record<string, unknown>;
}

export function stringifyToolResult(value: unknown): string {
  if (typeof value === 'string') return value;
  if (value === undefined || value === null) return '';
  try {
    // Functions and symbols have no JSON form.
    return JSON.stringify(value) ?? String(value);
  } catch {
    return String(value);
  }
}

/** Most characters of a tool's result kept; a truncation note follows when cut. */
export const MAX_CLIENT_TOOL_OUTPUT = 100_000;

/** Caps a page-supplied result so one tool can't overwhelm the request or transcript. */
export function limitToolOutput(output: string): string {
  if (output.length <= MAX_CLIENT_TOOL_OUTPUT) return output;
  const dropped = output.length - MAX_CLIENT_TOOL_OUTPUT;
  return `${output.slice(0, MAX_CLIENT_TOOL_OUTPUT)}\n[truncated ${dropped} characters]`;
}

export function toolOutputItem(callId: string, output: string): ClientToolOutputItem {
  return { type: 'function_call_output', call_id: callId, output };
}

/** Applies `patch` to the listed tool calls wherever they appear. */
export function patchToolCalls(
  messages: Message[],
  callIds: ReadonlySet<string>,
  patch: Partial<ToolCall> | ((tool: ToolCall) => Partial<ToolCall>),
): Message[] {
  return messages.map((message) =>
    message.role !== 'tool-group' || !message.tools?.some((tool) => callIds.has(tool.id))
      ? message
      : {
          ...message,
          tools: message.tools.map((tool) =>
            callIds.has(tool.id)
              ? { ...tool, ...(typeof patch === 'function' ? patch(tool) : patch) }
              : tool,
          ),
        },
  );
}

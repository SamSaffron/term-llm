import type { ToolCall } from './types';

export function isCodeModeTool(name: string): boolean {
  return [
    'code_mode',
    'exec',
    'wait',
    'cancel',
    'spawn_script',
    'wait_script',
    'cancel_script',
  ].includes(name);
}

/** Keep machine state/audit metadata out of the user-facing script result. */
export function codeModeDisplayTool(tool: ToolCall): ToolCall {
  if (!isCodeModeTool(tool.name)) return tool;
  const parts = tool.contentParts;
  const last = parts?.at(-1);
  // Live tool-end events may carry ordered parts without a legacy result string.
  const raw = last?.type === 'text' ? last.text : tool.result;
  if (!raw) return tool;
  let envelope: Record<string, unknown>;
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (
      !parsed ||
      typeof parsed.cell_id !== 'string' ||
      (tool.name === 'code_mode' ? parsed.cell_id !== tool.id : parsed.tool_call_id !== tool.id) ||
      typeof parsed.state_committed !== 'boolean'
    )
      return tool;
    envelope = parsed as Record<string, unknown>;
  } catch {
    return tool;
  }
  const contentParts = parts?.length
    ? last?.type === 'text'
      ? parts.slice(0, -1)
      : parts
    : Array.isArray(envelope.output)
      ? envelope.output.flatMap((item: unknown) => {
          if (
            !item ||
            typeof item !== 'object' ||
            (item as Record<string, unknown>).kind !== 'text' ||
            !('value' in item)
          )
            return [];
          return [
            {
              type: 'text' as const,
              text: typeof item.value === 'string' ? item.value : JSON.stringify(item.value),
            },
          ];
        })
      : [];
  const notices: string[] = [];
  if (tool.name !== 'code_mode') {
    const status = envelope.status;
    const notice =
      status === 'running' || status === 'yielded'
        ? 'Cell is still running.'
        : status === 'cancelling'
          ? 'Cell cancellation is pending.'
          : status === 'cancelled'
            ? 'Cell was cancelled.'
            : '';
    if (notice) notices.push(notice);
    if (envelope.has_more_output === true) notices.push('More output is available.');
    if (envelope.output_truncated === true) notices.push('Output was truncated.');
  }
  return {
    ...tool,
    contentParts: [...contentParts, ...notices.map((text) => ({ type: 'text' as const, text }))],
    result: typeof envelope.error === 'string' ? envelope.error : undefined,
  };
}

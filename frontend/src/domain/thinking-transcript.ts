import { historyVisible, type ThinkingPolicy } from './thinking';
import type { Message } from './types';

/** Presentation only: retain canonical reasoning/tool boundaries for recovery,
 * but join tool groups separated solely by thoughts that have no visible row. */
export function thinkingTranscript(messages: Message[], policy: ThinkingPolicy): Message[] {
  let hiddenThought = false;
  let changed = false;
  const output: Message[] = [];
  for (const message of messages) {
    if (
      message.role === 'reasoning' &&
      (!message.content || !historyVisible(message.reasoningKind || '', policy))
    ) {
      hiddenThought = true;
      changed = true;
      continue;
    }
    const previous = output.at(-1);
    if (
      hiddenThought &&
      previous?.role === 'tool-group' &&
      message.role === 'tool-group' &&
      previous.responseId === message.responseId
    ) {
      const tools = [...(previous.tools || [])];
      for (const tool of message.tools || []) {
        const index = tools.findIndex((entry) => entry.id === tool.id);
        if (index < 0) tools.push(tool);
        else tools[index] = { ...tools[index], ...tool };
      }
      output[output.length - 1] = {
        ...previous,
        tools,
        status: tools.some((tool) => tool.status === 'running') ? 'running' : 'done',
        toolGroupClosed: message.toolGroupClosed,
        durableSourceHead: message.durableSourceHead || previous.durableSourceHead,
        durableSourceRowIds: [
          ...new Set([
            ...((previous.durableSourceRowIds as Array<string | number>) ||
              (previous.durableRowId ? [previous.durableRowId] : [])),
            ...((message.durableSourceRowIds as Array<string | number>) ||
              (message.durableRowId ? [message.durableRowId] : [])),
          ]),
        ],
      };
    } else output.push(message);
    hiddenThought = false;
  }
  return changed ? output : messages;
}

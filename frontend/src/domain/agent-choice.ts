/** Encodes explicit agent intent independently of resolved server defaults. */
export const AGENT_CHAT_VALUE = 'chat';
export const AGENT_AUTO_VALUE = 'auto';
const AGENT_VALUE_PREFIX = 'agent:';

export type AgentChoice = { kind: 'auto' } | { kind: 'chat' } | { kind: 'agent'; name: string };

export function agentChoiceValue(name: string): string {
  return `${AGENT_VALUE_PREFIX}${name}`;
}

export function agentNameFromValue(value: string): string {
  return value.startsWith(AGENT_VALUE_PREFIX) ? value.slice(AGENT_VALUE_PREFIX.length) : '';
}

export function agentChoiceFromValue(value: string): AgentChoice {
  if (value === AGENT_CHAT_VALUE) return { kind: 'chat' };
  const name = agentNameFromValue(value);
  return name ? { kind: 'agent', name } : { kind: 'auto' };
}

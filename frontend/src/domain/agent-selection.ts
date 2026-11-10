/**
 * Shared new-chat agent policy.
 *
 * The Web UI keeps explicit agent choices separate from automatic defaults:
 * an automatic project conversation resolves the configured project agent, and
 * an automatic no-project conversation means "no agent". Only an explicit
 * choice is persisted. Values used by the picker encode that distinction so a
 * custom agent literally named `chat` can never be confused with Chat mode.
 */

import { AGENT_CHAT_VALUE, agentChoiceValue } from './agent-choice';

/** Matches the server-side stock block for a shell without the injected list. */
export const DEFAULT_AGENT_PICKER_COMMON = [
  'developer',
  'sysadmin',
  'extension-builder',
  'widget-builder',
];

/** An omitted injected list means stock defaults, not "no split". */
export function agentPickerCommon(configured: readonly string[] | undefined): string[] {
  return configured === undefined ? [...DEFAULT_AGENT_PICKER_COMMON] : [...configured];
}

export interface AgentMenuOption {
  value: string;
  label: string;
  /**
   * Block identity used for one plain divider between visible blocks. Filtering
   * out an earlier member must not hide the divider for later members.
   */
  group?: number;
}

export interface AgentMenu {
  options: AgentMenuOption[];
}

export interface AgentMenuInput {
  agentNames: string[];
  common: string[];
  projectActive: boolean;
}

/** Registered names without duplicates, preserving the first occurrence. */
export function uniqueAgentNames(names: readonly string[]): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const raw of names) {
    const name = raw.trim();
    if (!name || seen.has(name)) continue;
    seen.add(name);
    result.push(name);
  }
  return result;
}

/**
 * Builds the minimal picker: Chat (only without a project), then the configured
 * common agents, one separator, then the alphabetical remainder. Unavailable
 * common entries are skipped; an empty common list keeps everything
 * alphabetical with Chat first and no split.
 */
export function buildAgentMenu(input: AgentMenuInput): AgentMenu {
  const registered = uniqueAgentNames(input.agentNames);
  const present = new Set(registered);
  const common = uniqueAgentNames(input.common).filter((name) => present.has(name));
  const commonSet = new Set(common);
  const remaining = registered
    .filter((name) => !commonSet.has(name))
    .sort((a, b) => a.localeCompare(b));
  const options: AgentMenuOption[] = [];
  // Chat is the no-agent choice. It is only offered without a project; its
  // approval relevance comes from the server capability map, never its label.
  if (!input.projectActive) options.push({ value: AGENT_CHAT_VALUE, label: 'Chat', group: 0 });
  for (const name of common) options.push({ value: agentChoiceValue(name), label: name, group: 0 });
  // One split only when both blocks actually exist.
  const remainingGroup = common.length ? 1 : 0;
  for (const name of remaining)
    options.push({ value: agentChoiceValue(name), label: name, group: remainingGroup });
  return { options };
}

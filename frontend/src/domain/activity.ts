import { DEFAULT_THINKING, thinkingStatus, type ThinkingPolicy } from './thinking';
import type { ResponseProjection } from './response';
import type { CurrentPlan, RunStatus, ToolCall } from './types';

export interface ResponseActivity {
  text: string;
  kind: 'working' | 'plan' | 'tool' | 'retrying' | 'stopping' | 'thinking';
}

const TOOL_ACTIVITY: Record<string, string> = {
  activate_skill: 'Loading guidance',
  ask_user: 'Preparing a question',
  edit_file: 'Editing files',
  glob: 'Finding files',
  grep: 'Searching code',
  manage_workspace: 'Preparing the workspace',
  read_file: 'Reading files',
  read_url: 'Reading a webpage',
  shell: 'Running a command',
  spawn_agent: 'Delegating work',
  update_plan: 'Updating the plan',
  view_image: 'Inspecting an image',
  web_search: 'Searching the web',
  write_file: 'Writing files',
};

function toolActivity(tool: ToolCall): string {
  const name = tool.name.split(/[.:/]/).at(-1)?.toLowerCase() || '';
  return TOOL_ACTIVITY[name] || 'Working';
}

export function responseActivity(
  projection: Pick<ResponseProjection, 'messages' | 'retry'> | null,
  plan: CurrentPlan | null,
  status: RunStatus,
  thinking: ThinkingPolicy = DEFAULT_THINKING,
): ResponseActivity {
  if (status === 'cancelling') return { text: 'Stopping', kind: 'stopping' };
  if (projection?.retry)
    return {
      text: `Retrying provider${projection.retry.attempt ? ` · attempt ${projection.retry.attempt}` : ''}`,
      kind: 'retrying',
    };

  const newest = projection?.messages.at(-1);
  const toolsRunning = projection?.messages.some((message) =>
    message.tools?.some((tool) => tool.status === 'running'),
  );
  if (status === 'streaming' && newest?.role === 'reasoning' && !toolsRunning)
    return { text: thinkingStatus(newest, thinking), kind: 'thinking' };

  const activeStep = plan?.plan.find((step) => step.status === 'in_progress')?.step.trim();
  if (activeStep) return { text: activeStep, kind: 'plan' };

  const runningTools =
    projection?.messages
      .flatMap((message) => message.tools || [])
      .filter((tool) => tool.status === 'running') || [];
  if (runningTools.length === 1) return { text: toolActivity(runningTools[0]), kind: 'tool' };
  if (runningTools.length > 1) {
    const activities = new Set(runningTools.map(toolActivity));
    if (activities.size === 1)
      return { text: activities.values().next().value || 'Working', kind: 'tool' };
    return { text: `Running ${runningTools.length} tools`, kind: 'tool' };
  }

  return { text: 'Working', kind: 'working' };
}

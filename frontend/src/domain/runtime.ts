export const EFFORTS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'] as const;
export type Effort = typeof EFFORTS[number];

export interface ModelOption {
  id: string;
  name: string;
  provider?: string;
  efforts?: string[];
  reasoning_modes?: string[];
  default_effort?: string;
  [key: string]: unknown;
}

const NATURAL_SUFFIX_MODELS = new Set(['o3-mini-high', 'o4-mini-high']);
export function splitModelEffort(model: string, separateEffort = ''): { model: string; effort: string } {
  const value = String(model || '').trim();
  const separate = String(separateEffort || '').trim().toLowerCase();
  for (const effort of [...EFFORTS].sort((left, right) => right.length - left.length)) {
    const match = value.match(new RegExp(`^(.*?)[-_]${effort}$`, 'i'));
    if (!match || !match[1] || NATURAL_SUFFIX_MODELS.has(value.toLowerCase())) continue;
    if (!separate || separate === effort || EFFORTS.includes(separate as Effort)) return { model: match[1], effort: separate || effort };
  }
  return { model: value, effort: separate };
}

export function compactModelLabel(value: string): string {
  return String(value || '').replace(/^(?:openai|anthropic|google|xai|ollama|openrouter)[/:]/i, '').replace(/-\d{4}-\d{2}-\d{2}$/i, '');
}

export function supportedEfforts(model: ModelOption | undefined): string[] {
  const values = model?.efforts || (Array.isArray(model?.reasoning_efforts) ? model.reasoning_efforts as string[] : []);
  return ['', ...new Set(values.map(String).filter(Boolean))];
}

export function supportsReasoningMode(model: ModelOption | undefined, mode: string): boolean {
  return mode === 'standard' || Boolean(model?.reasoning_modes?.includes(mode));
}

export interface RuntimeSelection { provider: string; model: string; effort: string; reasoningMode: string }
export function runtimeDiffers(active: Partial<RuntimeSelection>, selected: RuntimeSelection): boolean {
  return Boolean(
    (selected.provider && selected.provider !== (active.provider || ''))
    || (selected.model && splitModelEffort(selected.model, selected.effort).model !== splitModelEffort(active.model || '', active.effort || '').model)
    || (selected.effort && selected.effort !== (active.effort || ''))
  );
}

export function applyRuntimeToRequest(body: Record<string, unknown>, active: Partial<RuntimeSelection>, selected: RuntimeSelection, modelInfo?: ModelOption): void {
  const split = splitModelEffort(selected.model, selected.effort);
  if (split.model) body.model = split.model;
  const differs = runtimeDiffers(active, { ...selected, model: split.model, effort: split.effort });
  if (differs) {
    body.provider = selected.provider || active.provider || undefined;
    if (split.effort) body.reasoning_effort = split.effort;
    body.model_swap = { mode: 'auto', fallback: 'handover' };
  } else {
    if (active.provider || selected.provider) body.provider = active.provider || selected.provider;
    if (split.effort || active.effort) body.reasoning_effort = split.effort || active.effort;
  }
  if (supportsReasoningMode(modelInfo, selected.reasoningMode)) body.reasoning = { mode: selected.reasoningMode === 'pro' ? 'pro' : 'standard' };
}

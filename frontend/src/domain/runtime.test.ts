import { describe, expect, it } from 'vitest';
import { applyRuntimeToRequest, supportsReasoningMode } from './runtime';

describe('runtime request selection', () => {
  it('omits reasoning.mode unless the selected model advertises support', () => {
    const body: Record<string, unknown> = {};

    applyRuntimeToRequest(
      body,
      {},
      {
        provider: 'chatgpt',
        model: 'gpt-5.6-sol',
        effort: 'high',
        reasoningMode: 'standard',
      },
      {
        id: 'gpt-5.6-sol',
        name: 'gpt-5.6-sol',
        provider: 'chatgpt',
        efforts: ['low', 'medium', 'high'],
      },
    );

    expect(body).not.toHaveProperty('reasoning');
  });

  it('sends reasoning.mode when model metadata explicitly advertises it', () => {
    const model = {
      id: 'gpt-5.6-sol',
      name: 'gpt-5.6-sol',
      provider: 'openai',
      reasoning_modes: ['standard', 'pro'],
    };
    const body: Record<string, unknown> = {};

    expect(supportsReasoningMode(model, 'standard')).toBe(true);
    applyRuntimeToRequest(
      body,
      {},
      {
        provider: 'openai',
        model: 'gpt-5.6-sol',
        effort: 'high',
        reasoningMode: 'standard',
      },
      model,
    );

    expect(body.reasoning).toEqual({ mode: 'standard' });
  });
});

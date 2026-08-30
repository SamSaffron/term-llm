import { describe, expect, it } from 'vitest';
import { applyRuntimeToRequest, supportsFastMode, supportsReasoningMode } from './runtime';

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
        fast: false,
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
        fast: false,
      },
      model,
    );

    expect(body.reasoning).toEqual({ mode: 'standard' });
  });
  it('sends priority service tier only for models that advertise fast mode', () => {
    const model = {
      id: 'gpt-5.6-sol',
      name: 'gpt-5.6-sol',
      service_tiers: [{ id: 'priority', name: 'fast' }],
    };
    const body: Record<string, unknown> = {};

    expect(supportsFastMode(model)).toBe(true);
    applyRuntimeToRequest(
      body,
      {},
      {
        provider: 'chatgpt',
        model: model.id,
        effort: 'medium',
        reasoningMode: 'standard',
        fast: true,
      },
      model,
    );

    expect(body.service_tier).toBe('priority');
  });

  it('sends an explicit service-tier clear when fast mode is off', () => {
    const model = {
      id: 'gpt-5.6-sol',
      name: 'gpt-5.6-sol',
      additional_speed_tiers: ['fast'],
    };
    const body: Record<string, unknown> = {};

    applyRuntimeToRequest(
      body,
      {},
      {
        provider: 'chatgpt',
        model: model.id,
        effort: 'medium',
        reasoningMode: 'standard',
        fast: false,
      },
      model,
    );

    expect(body.service_tier).toBe('');
  });

  it('inherits provider-enforced fast mode without sending an override', () => {
    const model = {
      id: 'gpt-5.6-sol',
      name: 'gpt-5.6-sol',
      service_tiers: [{ id: 'priority' }],
    };
    const body: Record<string, unknown> = {};

    applyRuntimeToRequest(
      body,
      {},
      {
        provider: 'chatgpt',
        model: model.id,
        effort: 'medium',
        reasoningMode: 'standard',
        fast: false,
      },
      model,
      { id: 'chatgpt', name: 'chatgpt', service_tier: 'fast' },
    );

    expect(body).not.toHaveProperty('service_tier');
  });
});

import { describe, expect, it } from 'vitest';
import { planSummary } from './plan';
import type { CurrentPlan } from './types';

const plan = (statuses: CurrentPlan['plan'][number]['status'][]): CurrentPlan => ({
  explanation: 'Ship the polished experience',
  plan: statuses.map((status, index) => ({ step: `Step ${index + 1}`, status })),
});

describe('planSummary', () => {
  it('reports the active plan position instead of only the completed count', () => {
    expect(planSummary(plan(['completed', 'in_progress', 'pending']))).toMatchObject({
      completed: 1,
      total: 3,
      position: 2,
      complete: false,
      activeStep: 'Step 2',
    });
  });

  it('handles all-pending and completed plans', () => {
    expect(planSummary(plan(['pending', 'pending']))).toMatchObject({
      completed: 0,
      position: 1,
      complete: false,
    });
    expect(planSummary(plan(['completed', 'completed']))).toMatchObject({
      completed: 2,
      position: 2,
      complete: true,
    });
  });

  it('changes its signature when plan content or status changes', () => {
    const initial = plan(['pending']);
    const changedStatus = plan(['in_progress']);
    const changedText = { ...initial, plan: [{ ...initial.plan[0], step: 'A better step' }] };

    expect(planSummary(initial).signature).not.toBe(planSummary(changedStatus).signature);
    expect(planSummary(initial).signature).not.toBe(planSummary(changedText).signature);
    expect(planSummary(initial).signature).toBe(planSummary({ ...initial }).signature);
  });
});

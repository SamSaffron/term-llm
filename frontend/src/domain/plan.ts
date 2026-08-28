import type { CurrentPlan } from './types';

export interface PlanSummary {
  total: number;
  completed: number;
  position: number;
  complete: boolean;
  activeStep: string;
  signature: string;
}

export function planSummary(plan: CurrentPlan | null): PlanSummary {
  const steps = plan?.plan || [];
  const completed = steps.filter((step) => step.status === 'completed').length;
  const complete = steps.length > 0 && completed === steps.length;
  const activeIndex = steps.findIndex((step) => step.status === 'in_progress');
  const nextIndex = steps.findIndex((step) => step.status !== 'completed');
  const position = steps.length
    ? complete
      ? steps.length
      : (activeIndex >= 0 ? activeIndex : Math.max(0, nextIndex)) + 1
    : 0;
  const signature = JSON.stringify([
    plan?.explanation || '',
    ...steps.flatMap((step) => [step.step, step.status]),
  ]);

  return {
    total: steps.length,
    completed,
    position,
    complete,
    activeStep: activeIndex >= 0 ? steps[activeIndex].step : '',
    signature,
  };
}

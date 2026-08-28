import { computed, signal } from '@preact/signals';
import type { CurrentPlan } from '../domain/types';
import { planSummary } from '../domain/plan';

/** Owns plan presentation state independently of response transport. */
export class PlanStore {
  readonly current = signal<CurrentPlan | null>(null);
  readonly openState = signal(false);
  readonly seen = signal<string | null>(null);
  readonly visible = computed(() => this.openState.value && Boolean(this.current.value));

  constructor(private readonly closeReview: () => void) {}

  update(plan: CurrentPlan | null): void {
    this.current.value = plan;
    if (!plan) {
      this.openState.value = false;
      this.seen.value = '';
    }
  }

  open(): void {
    const plan = this.current.peek();
    if (!plan) return;
    this.closeReview();
    this.seen.value = planSummary(plan).signature;
    this.openState.value = true;
  }

  close(): void {
    this.openState.value = false;
  }
}

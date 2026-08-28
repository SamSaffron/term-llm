import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { Goal, Session } from '../domain/types';
import type { Modal } from './store-types';
import type { AppStoreServices } from './app-store-services';

/** Owns the active session goal and goal commands. */
export class GoalStore {
  readonly state = signal<Goal | null>(null);

  constructor(
    private readonly services: AppStoreServices,
    private readonly activeSession: ReadonlySignal<Session | null>,
    private readonly modal: Signal<Modal>,
  ) {}

  async save(goal: Goal | { action: string }): Promise<void> {
    const session = this.activeSession.value;
    if (!session) return;
    const request =
      'objective' in goal
        ? { action: 'set', objective: goal.objective, token_budget: goal.token_budget }
        : goal;
    await this.services.endpoints.goal(session.id, request);
    if ('objective' in goal) this.state.value = goal;
    else if (goal.action === 'clear') this.state.value = null;
    else if (this.state.value)
      this.state.value = {
        ...this.state.value,
        status: goal.action === 'pause' ? 'paused' : 'active',
      };
    this.modal.value = '';
  }
}

import { APIError } from '../api/client';
import { errorMessage } from '../domain/text';
import { rushActive, type RushOperation } from '../domain/steering';
import { uuid } from './store-utils';
import type { PendingSteering, SendOptions } from './store-types';
import type { RunEngine, RunEngineHost } from './run-engine';
import type { SessionStore } from './session-store';
import type { ComposerStore } from './composer-store';
import type { AppStoreServices } from './app-store-services';

export interface SteeringActions {
  sessionStore: SessionStore;
  composer: ComposerStore;
  services: AppStoreServices;
  host: Pick<RunEngineHost, 'publishSessionChange' | 'resumeResponse' | 'reconcile'>;
  runs: RunEngine['runs'];
  steering: RunEngine['steering'];
  steeringCapabilities: RunEngine['steeringCapabilities'];
  activeRush: RunEngine['activeRush'];
  setSteering(entries: PendingSteering[]): void;
  isDisposed?(): boolean;
}

export async function steer(
  actions: SteeringActions,
  content: string,
  options: SendOptions = {},
): Promise<void> {
  const session = actions.sessionStore.activeSession.value;
  const value = (options.inputText ?? content).trim();
  const displayContent = (options.displayContent ?? value).trim();
  const attachments = options.contentParts ? [] : [...actions.composer.attachments.value];
  if (!session || (!value && !attachments.length && !options.contentParts?.length)) return;
  const blockedAttachment = attachments.find(
    (attachment) => attachment.status && attachment.status !== 'ready',
  );
  if (blockedAttachment) {
    actions.services.toast(
      blockedAttachment.error || `${blockedAttachment.name} is still being prepared.`,
      'error',
    );
    return;
  }
  const id = uuid();
  const entry: PendingSteering = {
    id,
    sessionId: session.id,
    content: displayContent || attachments.map((attachment) => attachment.name).join(', '),
    state: 'sending',
  };
  actions.setSteering([...actions.steering.value, entry]);
  try {
    const attachmentParts = await Promise.all(
      attachments.map((attachment) => actions.composer.attachmentInput(attachment)),
    );
    const contentParts = options.contentParts?.length
      ? [...options.contentParts, ...(value ? [{ type: 'input_text', text: value }] : [])]
      : [...attachmentParts, ...(value ? [{ type: 'input_text', text: value }] : [])];
    await actions.services.endpoints.interrupt(
      session.id,
      {
        message: displayContent,
        ...(options.contentParts?.length || attachmentParts.length
          ? { content: contentParts }
          : {}),
        steering_id: id,
        client_message_id: id,
        ...(actions.runs.peek()[session.id]?.run.responseId
          ? {
              expected_response_id: actions.runs.peek()[session.id].run.responseId,
              expected_run_epoch: actions.runs.peek()[session.id].run.epoch,
            }
          : {}),
        delivery: 'steer',
      },
      id,
      actions.steeringCapabilities.peek()[session.id]?.protocol === 1,
    );
    // Admission invalidates an earlier empty-queue capability snapshot. Do not
    // wait for a state poll to make the newly accepted guidance actionable.
    const capability = actions.steeringCapabilities.peek()[session.id];
    if (capability?.protocol === 1 && capability.unavailable_reason === 'no_user_steering')
      actions.steeringCapabilities.value = {
        ...actions.steeringCapabilities.peek(),
        [session.id]: { ...capability, can_rush: true, unavailable_reason: undefined },
      };
    options.onTransportStarted?.();
    actions.composer.releaseResources(attachments, true);
    actions.setSteering(
      actions.steering.value.map((candidate) =>
        candidate.id === id ? { ...candidate, state: 'pending' } : candidate,
      ),
    );
    actions.host.publishSessionChange(
      'run-changed',
      session.id,
      actions.runs.peek()[session.id]?.run.responseId || '',
      undefined,
      id,
    );
    if (options.preserveComposer) return;
    actions.composer.clearSubmitted(session.id, value, attachments);
  } catch (error) {
    actions.setSteering(
      actions.steering.value.map((candidate) =>
        candidate.id === id ? { ...candidate, state: 'failed' } : candidate,
      ),
    );
    if (options.onTransportFailed) options.onTransportFailed(error);
    else actions.services.toast(error, 'error');
  }
}

const observers = new WeakMap<object, Map<string, Promise<RushOperation>>>();

// One observer per operation, shared by the initiating tab and state hydration.
// Navigation never cancels the server-owned operation; disposal only stops polling.
export function observeRush(
  actions: SteeringActions,
  initial: RushOperation,
): Promise<RushOperation> {
  let active = observers.get(actions.activeRush);
  if (!active) {
    active = new Map();
    observers.set(actions.activeRush, active);
  }
  const key = `${initial.session_id}/${initial.rush_id}`;
  const existing = active.get(key);
  if (existing) return existing;
  const follow = async () => {
    let op = initial;
    while (!actions.isDisposed?.()) {
      const current = actions.activeRush.peek();
      if (current?.rush_id === op.rush_id && current.revision > op.revision) op = current;
      else if (
        !current ||
        current.rush_id === op.rush_id ||
        (!rushActive(current) && current.source_run_epoch <= op.source_run_epoch)
      )
        actions.activeRush.value = op;
      if (!rushActive(op)) break;
      await new Promise((resolve) => setTimeout(resolve, 350));
      if (actions.isDisposed?.()) return op;
      op = await actions.services.endpoints.rushState(op.session_id, op.rush_id);
    }
    if (actions.isDisposed?.()) return op;
    if (op.status === 'started' && op.replacement_response_id) {
      const admitted = new Set(
        op.steering_ids || op.entries?.map((entry) => entry.steering.id) || [],
      );
      actions.setSteering(
        actions.steering
          .peek()
          .filter((entry) => entry.sessionId !== op.session_id || !admitted.has(entry.id)),
      );
      const run = actions.runs?.peek()[op.session_id]?.run;
      if (
        !run ||
        run.responseId === op.source_response_id ||
        run.responseId === op.replacement_response_id ||
        run.epoch <= op.source_run_epoch
      )
        await actions.host.resumeResponse(op.session_id, op.replacement_response_id);
    }
    return op;
  };
  const promise = follow().finally(() => active!.delete(key));
  active.set(key, promise);
  return promise;
}

export async function continueRush(
  actions: SteeringActions,
  sessionId: string,
  requestId: string,
  body: Record<string, unknown>,
): Promise<void> {
  let rejected = false;
  try {
    let op: RushOperation;
    try {
      op = await actions.services.endpoints.rush(sessionId, body);
    } catch (error) {
      rejected = error instanceof APIError && error.status >= 400 && error.status < 500;
      if (rejected) throw error;
      // A lost acceptance never becomes an independent fallback response.
      try {
        op = await actions.services.endpoints.rushState(sessionId, requestId);
      } catch (lookupError) {
        if (!(lookupError instanceof APIError) || lookupError.status !== 404) throw lookupError;
        try {
          op = await actions.services.endpoints.rush(sessionId, body);
        } catch (retryError) {
          rejected =
            retryError instanceof APIError && retryError.status >= 400 && retryError.status < 500;
          throw retryError;
        }
      }
    }
    op = await observeRush(actions, op);
    if (op.status !== 'started' && op.reason) actions.services.toast(op.reason, 'error');
  } catch (error) {
    actions.services.toast(`Could not confirm rush: ${errorMessage(error)}`, 'error');
    if (rejected && actions.activeRush.peek()?.rush_id === requestId)
      actions.activeRush.value = null;
    void actions.host.reconcile('rush', true).catch(() => undefined);
  }
}

export async function rush(
  actions: SteeringActions,
  run: import('../domain/types').ActiveRun,
): Promise<void> {
  const sessionId = run.sessionId;
  if (
    rushActive(actions.activeRush.peek()) ||
    !actions.steeringCapabilities.peek()[sessionId]?.can_rush
  )
    return;
  const requestId = uuid();
  const body = {
    request_id: requestId,
    expected_response_id: run.responseId,
    expected_run_epoch: run.epoch,
  };
  // Capture intent synchronously so repeated Escape cannot stop a handoff.
  actions.activeRush.value = {
    rush_id: requestId,
    session_id: sessionId,
    source_response_id: run.responseId,
    source_run_epoch: run.epoch,
    status: 'interrupting',
    revision: 0,
  };
  await continueRush(actions, sessionId, requestId, body);
}

export async function cancelSteering(actions: SteeringActions, id: string): Promise<void> {
  const entry = actions.steering.value.find((candidate) => candidate.id === id);
  if (!entry) return;
  try {
    if (actions.steeringCapabilities.peek()[entry.sessionId]?.protocol === 1)
      await actions.services.endpoints.deleteInterrupt(
        entry.sessionId,
        id,
        actions.runs.peek()[entry.sessionId]?.run,
        true,
      );
    else await actions.services.endpoints.deleteInterrupt(entry.sessionId, id);
    actions.setSteering(actions.steering.value.filter((candidate) => candidate.id !== id));
  } catch (error) {
    actions.services.toast(error, 'error');
  }
}

export function reconcileRush(
  actions: SteeringActions,
  rush: RushOperation,
): Promise<RushOperation> | undefined {
  if (actions.isDisposed?.()) return;
  const current = actions.activeRush.peek();
  if (
    !current ||
    (current.rush_id === rush.rush_id && current.revision <= rush.revision) ||
    (!rushActive(current) && current.source_run_epoch <= rush.source_run_epoch)
  )
    actions.activeRush.value = rush;
  if (
    rushActive(rush) ||
    (rush.status === 'started' &&
      actions.runs.peek()[rush.session_id]?.run.responseId === rush.source_response_id)
  )
    return observeRush(actions, rush);
}

// The only browser ingress shim for the one-release live compatibility window
// and long-lived historical events. Never rewrite message text or attachments.
export function normalizeSteering<T extends Record<string, unknown>>(input: T): T {
  const result: Record<string, unknown> = { ...input };
  if (result.type === 'response.interjection') result.type = 'response.steering';
  if (!Object.hasOwn(result, 'steering_id') && Object.hasOwn(result, 'interjection_id'))
    result.steering_id = result.interjection_id;
  if (!Object.hasOwn(result, 'pending_steering')) {
    if (Object.hasOwn(result, 'pending_interjections'))
      result.pending_steering = result.pending_interjections;
    else if (Object.hasOwn(result, 'pending_interjection'))
      result.pending_steering = result.pending_interjection ? [result.pending_interjection] : [];
  }
  return result as T;
}

export interface RushOperation {
  rush_id: string;
  session_id: string;
  source_response_id: string;
  source_run_epoch: number;
  status: string;
  revision: number;
  replacement_response_id?: string;
  reason?: string;
  steering_ids?: string[];
  entries?: {
    steering: { id: string; display_text: string; attachment_summary?: string };
    disposition: string;
  }[];
}
export const rushActive = (op: RushOperation | null): boolean =>
  Boolean(op && ['interrupting', 'waiting_for_settlement', 'starting'].includes(op.status));

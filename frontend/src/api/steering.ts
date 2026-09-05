import type { APIClient } from './client';
import type { RushOperation } from '../domain/steering';

const encoded = encodeURIComponent;
const root = (sessionId: string) => `/v1/sessions/${encoded(sessionId)}/steering`;

export const rush = (api: APIClient, sessionId: string, body: Record<string, unknown>) =>
  api.post<RushOperation>(`${root(sessionId)}/rush`, body, 'idempotent-mutation', {
    'Idempotency-Key': String(body.request_id),
  });
export const rushState = (api: APIClient, sessionId: string, id: string) =>
  api.get<RushOperation>(`${root(sessionId)}/rush/${encoded(id)}`);
export const cancelRush = (api: APIClient, sessionId: string, id: string) =>
  api.post<RushOperation>(
    `${root(sessionId)}/rush/${encoded(id)}/cancel`,
    {},
    'idempotent-mutation',
    {
      'Idempotency-Key': `stop_rush_${id}`,
    },
  );
export const deleteSteering = (
  api: APIClient,
  sessionId: string,
  id: string,
  run?: { responseId: string; epoch: number },
  canonical = false,
) => {
  if (!canonical)
    return api.delete(`/v1/sessions/${encoded(sessionId)}/interjections/${encoded(id)}`);
  const query = new URLSearchParams();
  if (run) {
    query.set('expected_response_id', run.responseId);
    query.set('expected_run_epoch', String(run.epoch));
  }
  return api.delete(`${root(sessionId)}/${encoded(id)}?${query}`);
};

export function legacySteeringBody(body: unknown): unknown {
  if (!body || typeof body !== 'object') return body;
  const result = { ...body } as Record<string, unknown>;
  if (result.steering_id) {
    result.interjection_id = result.steering_id;
    delete result.steering_id;
  }
  return result;
}

export const steer = (
  api: APIClient,
  sessionId: string,
  body: unknown,
  steeringId: string,
  canonical = false,
) =>
  api.json(
    `/v1/sessions/${encoded(sessionId)}/${canonical ? 'steering' : 'interrupt'}`,
    {
      method: 'POST',
      headers: { 'Idempotency-Key': `interrupt_${steeringId}` },
      body: JSON.stringify(canonical ? body : legacySteeringBody(body)),
    },
    { policy: 'idempotent-mutation', auth: 'session' },
  );

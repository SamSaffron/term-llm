import type { APIClient } from './client';
import type {
  LiveSessionStartResponse,
  LiveSessionStopResponse,
  LiveSessionSwitchResponse,
  LiveToolCallResult,
} from './endpoints';

const encoded = encodeURIComponent;

const reportLiveDiagnostics = (
  api: APIClient,
  liveId: string,
  report: unknown,
  signal: AbortSignal,
) =>
  api.request(
    `/v1/live/sessions/${encoded(liveId)}/diagnostics`,
    {
      method: 'POST',
      signal,
      headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
      body: JSON.stringify(report),
    },
    { policy: 'mutation', auth: 'session', retries: 0, timeoutMs: 5_000 },
  );

// Keep voice-only transport configuration out of the eager application shell.
export const liveEndpoints = (api: APIClient) => ({
  // clientTools are the page's own tools, declared with the start so the
  // call's first delegated turn can already use them.
  liveStart: (
    sdp: string,
    sessionId: string,
    audioTransport?: 'websocket_pcm' | 'http_pcm',
    clientTools?: readonly unknown[],
  ) =>
    api.json<LiveSessionStartResponse>(
      '/v1/live/sessions',
      {
        method: 'POST',
        body: JSON.stringify({
          sdp,
          session_id: sessionId,
          ...(audioTransport ? { audio_transport: audioTransport } : {}),
          ...(clientTools?.length ? { client_tools: clientTools } : {}),
        }),
      },
      { policy: 'mutation', auth: 'session', retries: 0, timeoutMs: 0 },
    ),
  liveStop: (liveId: string) =>
    api.delete<LiveSessionStopResponse>(`/v1/live/sessions/${encoded(liveId)}`),
  liveAudioOutput: (liveId: string, capability: string, signal: AbortSignal) =>
    api.request(
      `/v1/live/sessions/${encoded(liveId)}/audio/output`,
      {
        signal,
        headers: {
          Accept: 'text/event-stream',
          'X-Term-LLM-Live-Audio-Capability': capability,
        },
      },
      { policy: 'stream', auth: 'session', retries: 0, timeoutMs: 0 },
    ),
  liveAudioInput: (liveId: string, pcm: ArrayBuffer, signal: AbortSignal, diagnostics?: unknown) =>
    diagnostics === undefined
      ? api.request(
          `/v1/live/sessions/${encoded(liveId)}/audio/input`,
          {
            method: 'POST',
            signal,
            headers: { Accept: 'application/json', 'Content-Type': 'application/octet-stream' },
            body: pcm,
          },
          { policy: 'mutation', auth: 'session', retries: 0, timeoutMs: 15_000 },
        )
      : reportLiveDiagnostics(api, liveId, diagnostics, signal),
  liveText: (liveId: string, text: string) =>
    api.json<{ ok: true }>(
      `/v1/live/sessions/${encoded(liveId)}/text`,
      { method: 'POST', body: JSON.stringify({ text }) },
      { policy: 'mutation', auth: 'session', retries: 0 },
    ),
  liveSwitchSession: (liveId: string, sessionId: string) =>
    api.json<LiveSessionSwitchResponse>(
      `/v1/live/sessions/${encoded(liveId)}/session`,
      { method: 'POST', body: JSON.stringify({ session_id: sessionId }) },
      { policy: 'mutation', auth: 'session', retries: 0 },
    ),
  liveClientTools: (liveId: string, tools: readonly unknown[]) =>
    api.json<{ ok: true; tools: number }>(
      `/v1/live/sessions/${encoded(liveId)}/client_tools`,
      { method: 'POST', body: JSON.stringify({ tools }) },
      { policy: 'mutation', auth: 'session', retries: 0 },
    ),
  liveToolResult: (liveId: string, requestId: string, result: LiveToolCallResult) =>
    api.json<{ ok: true; duplicate?: boolean }>(
      `/v1/live/sessions/${encoded(liveId)}/tool_calls/${encoded(requestId)}/result`,
      { method: 'POST', body: JSON.stringify(result) },
      { policy: 'mutation', auth: 'session', retries: 0 },
    ),
  liveEvents: (liveId: string, after: number, signal: AbortSignal) =>
    api.request(
      `/v1/live/sessions/${encoded(liveId)}/events${after > 0 ? `?after=${after}` : ''}`,
      { signal, headers: { Accept: 'text/event-stream' } },
      { policy: 'stream', retries: 0, timeoutMs: 0, auth: 'session' },
    ),
});

export type LiveEndpoints = ReturnType<typeof liveEndpoints>;

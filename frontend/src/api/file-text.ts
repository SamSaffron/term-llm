import { APIError, type APIClient } from './client';

const encoded = (value: string): string => encodeURIComponent(value);

export async function fileText(
  api: APIClient,
  id: string,
  path: string,
  scope: string,
  side: 'before' | 'after',
  snapshotSeq = 0,
  signal?: AbortSignal,
): Promise<string> {
  // A Hub may source this raw, authenticated resource from the session's bound
  // node. Its asset hash is not the shell host's asset hash and must not force a
  // page refresh; dynamic UI chunks still come from the shell host.
  const response = await api.request(
    `/v1/sessions/${encoded(id)}/file-changes/content?path=${encoded(path)}&scope=${encoded(scope)}&side=${side}${snapshotSeq ? `&snapshot_seq=${snapshotSeq}` : ''}`,
    { signal, headers: { Accept: 'text/plain' } },
    { policy: 'safe-read', auth: 'session', versionCheck: false },
  );
  if (!response.ok) {
    const body = await response.text();
    throw new APIError(
      response.status === 404
        ? 'Markdown source is unavailable.'
        : `File content request returned ${response.status}`,
      response.status,
      body,
    );
  }
  return response.text();
}

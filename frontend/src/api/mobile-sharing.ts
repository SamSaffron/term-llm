import type { APIClient } from './client';
import type { MobileshareStatus } from './generated/types';

export type MobileSharingState =
  'unavailable' | 'off' | 'inviting' | 'pending' | 'sharing' | 'reconnecting' | 'stopped';

export type MobileSharingStatus = Omit<MobileshareStatus, 'state'> & {
  state: MobileSharingState;
};

const root = '/v1/mobile-sharing';

export function getMobileSharing(
  api: APIClient,
  signal?: AbortSignal,
): Promise<MobileSharingStatus> {
  return api.get<MobileSharingStatus>(root, signal, { auth: 'session' });
}

export function createMobileSharingInvitation(api: APIClient): Promise<MobileSharingStatus> {
  return api.post<MobileSharingStatus>(`${root}/invitations`, {});
}

export function cancelMobileSharingInvitation(api: APIClient): Promise<void> {
  return api.delete<void>(`${root}/invitations/current`);
}

export function approveMobileSharingRequest(
  api: APIClient,
  id: string,
  code: string,
): Promise<void> {
  return api.post<void>(`${root}/requests/${encodeURIComponent(id)}/approve`, { code });
}

export function denyMobileSharingRequest(api: APIClient, id: string): Promise<void> {
  return api.post<void>(`${root}/requests/${encodeURIComponent(id)}/deny`, {});
}

export function revokeMobileSharingDevice(api: APIClient, id: string): Promise<void> {
  return api.delete<void>(`${root}/devices/${encodeURIComponent(id)}`);
}

export function stopMobileSharing(api: APIClient): Promise<void> {
  return api.post<void>(`${root}/stop`, {});
}

export function resumeMobileSharing(api: APIClient): Promise<void> {
  return api.post<void>(`${root}/resume`, {});
}

export function resetMobileSharing(api: APIClient): Promise<void> {
  return api.post<void>(`${root}/reset`, {});
}

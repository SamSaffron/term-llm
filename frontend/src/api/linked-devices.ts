import type { APIClient } from './client';
import type { LinkedDevicesStatus as GeneratedLinkedDevicesStatus } from './generated/types';

export type LinkedDevicesState =
  'unavailable' | 'off' | 'inviting' | 'pending' | 'sharing' | 'reconnecting' | 'stopped';

export type LinkedDevicesStatus = Omit<GeneratedLinkedDevicesStatus, 'state'> & {
  state: LinkedDevicesState;
};

const root = '/v1/linked-devices';

export function getLinkedDevices(
  api: APIClient,
  signal?: AbortSignal,
): Promise<LinkedDevicesStatus> {
  return api.get<LinkedDevicesStatus>(root, signal, { auth: 'session' });
}

export function createLinkedDevicesInvitation(api: APIClient): Promise<LinkedDevicesStatus> {
  return api.post<LinkedDevicesStatus>(`${root}/invitations`, {});
}

export function cancelLinkedDevicesInvitation(api: APIClient): Promise<void> {
  return api.delete<void>(`${root}/invitations/current`);
}

export function approveLinkedDevicesRequest(
  api: APIClient,
  id: string,
  code: string,
): Promise<void> {
  return api.post<void>(`${root}/requests/${encodeURIComponent(id)}/approve`, { code });
}

export function denyLinkedDevicesRequest(api: APIClient, id: string): Promise<void> {
  return api.post<void>(`${root}/requests/${encodeURIComponent(id)}/deny`, {});
}

export function revokeLinkedDevicesDevice(api: APIClient, id: string): Promise<void> {
  return api.delete<void>(`${root}/devices/${encodeURIComponent(id)}`);
}

export function stopLinkedDevices(api: APIClient): Promise<void> {
  return api.post<void>(`${root}/stop`, {});
}

export function resumeLinkedDevices(api: APIClient): Promise<void> {
  return api.post<void>(`${root}/resume`, {});
}

export function resetLinkedDevices(api: APIClient): Promise<void> {
  return api.post<void>(`${root}/reset`, {});
}

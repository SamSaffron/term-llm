import type { AppConfig } from '../app/config';
import type { Session } from '../domain/types';

export const testConfig: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: ['jarvis'],
  title: '',
  locationSharing: true,
  worktrees: true,
  hub: null,
  vapidKey: '',
  webRTC: false,
  signalingURL: '',
};

export const testSession = (patch: Partial<Session> = {}): Session => ({
  id: 's1',
  title: 'Test',
  name: '',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: 1,
  lastMessageAt: 1,
  messages: [],
  ...patch,
});

import type { ApprovalMode, Project } from '../domain/types';

declare global {
  interface Window {
    TERM_LLM_UI_PREFIX?: string;
    TERM_LLM_UI_VERSION?: string;
    TERM_LLM_SIDEBAR_SESSIONS?: string[] | string;
    TERM_LLM_AGENT_NAME?: string;
    TERM_LLM_AGENT_NAMES?: string[];
    TERM_LLM_UI_TITLE?: string;
    TERM_LLM_LOCATION_SHARING_ENABLED?: boolean;
    TERM_LLM_WORKTREES_ENABLED?: boolean;
    TERM_LLM_APPROVALS_ENABLED?: boolean;
    TERM_LLM_APPROVAL_MODE?: string;
    TERM_LLM_HUB?: HubContext;
    TERM_LLM_VAPID_PUBLIC_KEY?: string;
    TERM_LLM_PUSH_SUPPORTED?: boolean;
    TERM_LLM_WEBRTC_ENABLED?: boolean;
    TERM_LLM_WEBRTC_SIGNALING_URL?: string;
    __WEBRTC_ENABLED__?: boolean;
    __WEBRTC_SIGNALING_URL__?: string;
    __WEBRTC_DIAGNOSTICS__?: boolean;
    __TERM_LLM_WEBRTC_TESTING__?: boolean;
    __TERM_LLM_WEBRTC_TEST_HOOKS__?: unknown;
    __TERM_LLM_ENABLE_TEST_BRIDGE__?: boolean;
    __TERM_LLM_TEST__?: Record<string, unknown>;
  }
}

export interface HubContext {
  url?: string;
  nodeId?: string;
  nodeName?: string;
  nodeBasePath?: string;
  apiURL?: string;
}

export interface AppConfig {
  prefix: string;
  version: string;
  sidebarCategories: string[];
  agentName: string;
  agentNames: string[];
  title: string;
  locationSharing: boolean;
  worktrees: boolean;
  approvals?: boolean;
  approvalMode?: ApprovalMode;
  hub: HubContext | null;
  vapidKey: string;
  pushSupported?: boolean;
  webRTC: boolean;
  signalingURL: string;
  initialProject?: Project;
}

export function parseSidebarCategories(raw: unknown): string[] {
  const values = Array.isArray(raw) ? raw : String(raw || 'all').split(',');
  const result = [
    ...new Set(values.map((value) => String(value).trim().toLowerCase()).filter(Boolean)),
  ];
  return !result.length || result.includes('all') ? ['all'] : result;
}

/** Read only after the deferred bootstrap, so a Hub proxy can inject/rebase context first. */
export function readInjectedConfig(target: Window = window): AppConfig {
  const hub =
    target.TERM_LLM_HUB && typeof target.TERM_LLM_HUB === 'object'
      ? { ...target.TERM_LLM_HUB }
      : null;
  const prefix = String(target.TERM_LLM_UI_PREFIX || '/ui').replace(/\/$/, '') || '/ui';
  const injectedApprovalMode = String(target.TERM_LLM_APPROVAL_MODE || 'prompt');
  const approvalMode: ApprovalMode =
    injectedApprovalMode === 'auto' || injectedApprovalMode === 'yolo'
      ? injectedApprovalMode
      : 'prompt';
  return {
    prefix,
    version: String(target.TERM_LLM_UI_VERSION || ''),
    sidebarCategories: parseSidebarCategories(target.TERM_LLM_SIDEBAR_SESSIONS),
    agentName: String(target.TERM_LLM_AGENT_NAME || ''),
    agentNames: Array.isArray(target.TERM_LLM_AGENT_NAMES)
      ? [...new Set(target.TERM_LLM_AGENT_NAMES.map(String).filter(Boolean))]
      : [],
    title: String(target.TERM_LLM_UI_TITLE || ''),
    locationSharing: target.TERM_LLM_LOCATION_SHARING_ENABLED !== false,
    worktrees: target.TERM_LLM_WORKTREES_ENABLED === true,
    approvals: target.TERM_LLM_APPROVALS_ENABLED !== false,
    approvalMode,
    hub,
    vapidKey: String(target.TERM_LLM_VAPID_PUBLIC_KEY || ''),
    pushSupported: target.TERM_LLM_PUSH_SUPPORTED === true,
    webRTC: target.TERM_LLM_WEBRTC_ENABLED === true || target.__WEBRTC_ENABLED__ === true,
    signalingURL: String(
      target.TERM_LLM_WEBRTC_SIGNALING_URL || target.__WEBRTC_SIGNALING_URL__ || '',
    ),
  };
}

export function displayName(value: string): string {
  const cleaned = value.trim();
  if (!cleaned) return 'Chat';
  return cleaned
    .split(/[-_\s]+/)
    .filter(Boolean)
    .map((part) => part[0].toUpperCase() + part.slice(1))
    .join(' ');
}

export function rebaseHubAssetURL(config: AppConfig, value: string): string {
  if (!value || !config.hub?.nodeBasePath) return value;
  try {
    const url = new URL(value, location.href);
    if (url.origin !== location.origin || !url.pathname.startsWith('/')) return value;
    const mount = config.prefix.replace(/\/$/, '');
    if (url.pathname === mount || url.pathname.startsWith(`${mount}/`)) return url.href;
    const nodeBase = config.hub.nodeBasePath.replace(/\/$/, '');
    const nodePath =
      url.pathname === nodeBase
        ? '/'
        : url.pathname.startsWith(`${nodeBase}/`)
          ? url.pathname.slice(nodeBase.length)
          : url.pathname;
    url.pathname = `${mount}${nodePath.startsWith('/') ? nodePath : `/${nodePath}`}`;
    return url.href;
  } catch {
    return value;
  }
}

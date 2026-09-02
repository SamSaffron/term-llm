export type HubPageKind = 'dashboard' | 'passkey-auth' | 'bearer-login';
export type HubAuthMode = 'none' | 'bearer' | 'passkey';
export type HubPasskeyMode = 'setup' | 'login' | 'recover';

export interface HubConfig {
  page: HubPageKind;
  authMode: HubAuthMode;
  basePath: string;
  canAddNodes: boolean;
  passkeyAuth: boolean;
  invalidToken: boolean;
  formAction: string;
  passkey?: {
    mode: HubPasskeyMode;
    title: string;
    heading: string;
    description: string;
    button: string;
    needsCode: boolean;
    needsName: boolean;
    defaultName: string;
  };
}

const pageKinds = new Set<HubPageKind>(['dashboard', 'passkey-auth', 'bearer-login']);
const authModes = new Set<HubAuthMode>(['none', 'bearer', 'passkey']);
const passkeyModes = new Set<HubPasskeyMode>(['setup', 'login', 'recover']);

export function normalizeHubBasePath(value: unknown): string {
  const raw = String(value ?? '').trim();
  if (!raw || raw === '/') return '';
  if (!raw.startsWith('/') || raw.includes('?') || raw.includes('#') || raw.includes('..')) {
    throw new Error('Hub base path is invalid.');
  }
  return raw.replace(/\/+$/, '');
}

export function hubPath(basePath: string, value: string): string {
  const path = value.startsWith('/') ? value : `/${value}`;
  return `${normalizeHubBasePath(basePath)}${path}` || '/';
}

function bool(value: unknown): boolean {
  return value === true;
}

export function parseHubConfig(value: unknown): HubConfig {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('Hub configuration is missing.');
  }
  const raw = value as Record<string, unknown>;
  if (!pageKinds.has(raw.page as HubPageKind)) throw new Error('Hub page kind is invalid.');
  if (!authModes.has(raw.authMode as HubAuthMode))
    throw new Error('Hub authentication mode is invalid.');
  const config: HubConfig = {
    page: raw.page as HubPageKind,
    authMode: raw.authMode as HubAuthMode,
    basePath: normalizeHubBasePath(raw.basePath),
    canAddNodes: bool(raw.canAddNodes),
    passkeyAuth: bool(raw.passkeyAuth),
    invalidToken: bool(raw.invalidToken),
    formAction: String(raw.formAction || ''),
  };
  if (config.page === 'passkey-auth') {
    const passkey = raw.passkey;
    if (!passkey || typeof passkey !== 'object' || Array.isArray(passkey)) {
      throw new Error('Hub passkey page configuration is missing.');
    }
    const values = passkey as Record<string, unknown>;
    if (!passkeyModes.has(values.mode as HubPasskeyMode)) {
      throw new Error('Hub passkey page mode is invalid.');
    }
    config.passkey = {
      mode: values.mode as HubPasskeyMode,
      title: String(values.title || ''),
      heading: String(values.heading || ''),
      description: String(values.description || ''),
      button: String(values.button || ''),
      needsCode: bool(values.needsCode),
      needsName: bool(values.needsName),
      defaultName: String(values.defaultName || ''),
    };
  }
  if (
    config.page === 'bearer-login' &&
    (!config.formAction.startsWith('/') ||
      config.formAction.startsWith('//') ||
      config.formAction.includes('\\'))
  ) {
    throw new Error('Hub bearer form action is invalid.');
  }
  return config;
}

export function readHubConfig(root: HTMLElement): HubConfig {
  const encoded = root.dataset.hubConfig;
  if (!encoded) throw new Error('Hub configuration is missing.');
  try {
    return parseHubConfig(JSON.parse(encoded));
  } catch (error) {
    if (error instanceof SyntaxError)
      throw new Error('Hub configuration is malformed.', { cause: error });
    throw error;
  }
}

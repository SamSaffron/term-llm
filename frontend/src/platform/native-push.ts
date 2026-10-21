// Versioned host API. Scopes are server/node prefixes, not service-worker URLs.
export interface NativePushSubscription {
  endpoint: string;
  keys: { p256dh: string; auth: string };
  applicationServerKey: string;
}

export interface NativePushBridge {
  version: 1;
  permission(scope: string): Promise<NotificationPermission>;
  requestPermission(scope: string): Promise<NotificationPermission>;
  getSubscription(scope: string): Promise<NativePushSubscription | null>;
  subscribe(scope: string, applicationServerKey: string): Promise<NativePushSubscription>;
  unsubscribe(scope: string): Promise<unknown>;
  disable(scope: string): Promise<unknown>;
}

declare global {
  interface Window {
    __termLLMNativePush?: NativePushBridge;
  }
}

export function nativePushBridge(): NativePushBridge | undefined {
  const bridge = window.__termLLMNativePush;
  return bridge?.version === 1 &&
    typeof bridge.permission === 'function' &&
    typeof bridge.requestPermission === 'function' &&
    typeof bridge.getSubscription === 'function' &&
    typeof bridge.subscribe === 'function' &&
    typeof bridge.unsubscribe === 'function' &&
    typeof bridge.disable === 'function'
    ? bridge
    : undefined;
}

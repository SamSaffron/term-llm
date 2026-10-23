import { describe, expect, it, vi } from 'vitest';
import type { AppConfig } from '../app/config';
import type { Session } from '../domain/types';
import type { TermLLMNativePush } from '../platform/push-platform';
import { AppStore } from './app-store';

const config: AppConfig = {
  prefix: '/ui',
  version: 'v1',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: [],
  title: '',
  locationSharing: false,
  worktrees: false,
  hub: null,
  vapidKey: 'AQID',
  pushSupported: true,
  notificationVersion: 2,
};

const session: Session = {
  id: 's1',
  title: 'Skills',
  name: '',
  mode: 'chat',
  origin: 'web',
  archived: false,
  pinned: false,
  created: 1,
  lastMessageAt: 1,
  messages: [],
};

// Exercise the real store -> endpoint -> APIClient -> notification controller path,
// mocking only the native platform and HTTP boundary, not the captured headers.
describe('skill notification ownership integration', () => {
  it.each(['inline', 'isolated'] as const)(
    'captures only a main-human skill origin and keeps it stable on %s retries',
    async (execution) => {
      let capability = {
        version: 2 as const,
        window_id: 'initiating-window',
        document_id: 'initiating-document',
      };
      const subscription = {
        endpoint: 'https://push.example/skills',
        keys: { p256dh: 'public-key', auth: 'auth-key' },
        applicationServerKey: config.vapidKey,
      };
      const bridge: TermLLMNativePush = {
        version: 1,
        permission: async () => 'granted',
        requestPermission: async () => 'granted',
        getSubscription: async () => subscription,
        subscribe: async () => subscription,
        unsubscribe: async () => true,
        disable: async () => true,
        notificationCapabilities: async () => capability,
        notificationPresence: async () => {},
        notificationRead: async () => {},
        notificationEnrollment: async () => {},
      };
      const previousBridge = Object.getOwnPropertyDescriptor(window, '__termLLMNativePush');
      Object.defineProperty(window, '__termLLMNativePush', {
        configurable: true,
        value: bridge,
      });
      let subscriptionId = 'initiating-subscription';
      const attempts: RequestInit[] = [];
      const presence: Record<string, unknown>[] = [];
      const request = vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith('/push/subscribe'))
          return Response.json({ id: subscriptionId, state: 'active', vapid_key_id: 'key' });
        if (url.endsWith('/push/presence')) {
          presence.push(JSON.parse(String(init.body)) as Record<string, unknown>);
          return Response.json({ ok: true, version: 2 });
        }
        if (url.endsWith('/skills/invoke')) {
          attempts.push({ ...init, headers: new Headers(init.headers) });
          if (attempts.length === 1) return new Response('Unavailable', { status: 503 });
          return Response.json(
            execution === 'inline'
              ? { execution, response_id: 'skill-response', run_epoch: 1 }
              : { execution, run_id: 'skill-run', status: 'running' },
          );
        }
        if (url.includes('/skill-runs/skill-run/events'))
          return new Response(
            'data: {"sequence":1,"type":"skill_run.completed","data":{"status":"completed","output":"done"}}\n\n',
            { headers: { 'Content-Type': 'text/event-stream' } },
          );
        throw new Error(`Unexpected request: ${url}`);
      });
      vi.stubGlobal('fetch', request);
      const store = new AppStore(config);
      store.sessions.value = [{ ...session, messages: [] }];
      store.activeSessionId.value = session.id;
      store.draftActive.value = false;
      store.skills.value = [{ name: 'review', execution }];
      store.streamResponse = vi.fn(async () => undefined);
      store.runEngine.refreshSessionMessages = vi.fn(async () => undefined);
      try {
        expect(await store.notificationController.reconcile()).toMatchObject({
          status: 'subscribed',
          verified: true,
          subscriptionId,
        });
        await vi.waitFor(() => expect(presence).toHaveLength(1));
        const enrolledPresenceCount = presence.length;
        const capture = vi.spyOn(store.notificationController, 'originHeaders');
        vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
        const invoking = store.invokeSkill('review', 'changes');
        await vi.waitFor(() => expect(attempts).toHaveLength(1));
        expect(presence).toHaveLength(enrolledPresenceCount + (execution === 'inline' ? 1 : 0));

        // Enrollment and the platform window change while the first POST's
        // outcome is unknown. Its retry must not adopt this new destination.
        capability = {
          version: 2,
          window_id: 'replacement-window',
          document_id: 'replacement-document',
        };
        subscriptionId = 'replacement-subscription';
        expect(await store.notificationController.reconcile()).toMatchObject({ subscriptionId });
        await vi.advanceTimersByTimeAsync(1_000);
        await invoking;

        expect(attempts).toHaveLength(2);
        expect(capture).toHaveBeenCalledTimes(execution === 'inline' ? 1 : 0);
        if (execution === 'inline')
          expect(presence[0]).toMatchObject({
            version: 2,
            window_id: 'initiating-window',
            document_id: 'initiating-document',
            subscription_id: 'initiating-subscription',
            source: config.prefix,
          });
        const expectedOrigin = {
          'X-Term-LLM-Notification-Version': '2',
          'X-Term-LLM-Window-ID': 'initiating-window',
          'X-Term-LLM-Document-ID': 'initiating-document',
          'X-Term-LLM-Push-Subscription-ID': 'initiating-subscription',
          'X-Term-LLM-Notification-Source': config.prefix,
        };
        for (const attempt of attempts) {
          const headers = attempt.headers as Headers;
          for (const [name, value] of Object.entries(expectedOrigin))
            expect(headers.get(name)).toBe(execution === 'inline' ? value : null);
          expect(headers.get('Idempotency-Key')).toBe(
            `skill_${JSON.parse(String(attempt.body)).client_message_id}`,
          );
          expect(headers.get('X-Term-LLM-Session-ID')).toBe(session.id);
        }
        expect(attempts[1].body).toBe(attempts[0].body);
        if (execution === 'inline') {
          expect(store.runs.value.s1?.run.responseId).toBe('skill-response');
          expect(store.streamResponse).toHaveBeenCalledWith('skill-response', session.id, 0);
        } else {
          expect(store.runs.value.s1).toBeUndefined();
          expect(store.streamResponse).not.toHaveBeenCalled();
        }
      } finally {
        store.dispose();
        vi.useRealTimers();
        vi.unstubAllGlobals();
        if (previousBridge) Object.defineProperty(window, '__termLLMNativePush', previousBridge);
        else Reflect.deleteProperty(window, '__termLLMNativePush');
      }
    },
  );
});

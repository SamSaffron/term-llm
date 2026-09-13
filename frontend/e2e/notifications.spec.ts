import { expect, test } from '@playwright/test';

for (const route of ['', 'chat/9']) {
  test(`service worker keeps one tagged completion notification for duplicate local events at ${route || 'root'}`, async ({
    context,
    page,
    baseURL,
  }, testInfo) => {
    test.skip(
      testInfo.project.name === 'iphone',
      'non-standalone iPhone notification enrollment is intentionally unsupported',
    );
    const origin = new URL(baseURL!).origin;
    const expectedURL = new URL('chat/e2e-session', baseURL).href;
    await context.grantPermissions(['notifications'], { origin });
    await page.goto('./');
    const result = await page.evaluate(async (route) => {
      const registration = await navigator.serviceWorker.ready;
      // Force both sides of the startup routing race without waiting for automatic
      // session selection. Notification links must not depend on the current route.
      history.replaceState(null, '', new URL(route, registration.scope));
      const eventID = 'completion:e2e-response:e2e-subscription';
      const payload = {
        version: 1,
        event_id: eventID,
        response_id: 'e2e-response',
        session_id: 'e2e-session',
        outcome: 'completed',
        title: 'Response complete',
        body: 'Ready',
        url: new URL('chat/e2e-session', registration.scope).href,
        created_at: new Date().toISOString(),
      };
      const active = registration.active;
      if (!active) throw new Error('Service worker has no active instance');
      const sendNotification = () =>
        new Promise<void>((resolve, reject) => {
          const channel = new MessageChannel();
          const timeout = setTimeout(
            () => reject(new Error('Service worker did not acknowledge the notification')),
            5_000,
          );
          channel.port1.onmessage = (event) => {
            const data = event.data as { type?: string; tag?: string } | null;
            if (data?.type !== 'completion-notification-handled') return;
            clearTimeout(timeout);
            channel.port1.close();
            if (data.tag === `term-llm-completion:${eventID}`) resolve();
            else
              reject(new Error(`Service worker acknowledged an unexpected tag: ${data.tag || ''}`));
          };
          active.postMessage({ type: 'completion-notification', payload }, [channel.port2]);
        });
      // Wait for each event to finish so the second delivery exercises the worker's
      // existing-notification path instead of racing the first delivery.
      await sendNotification();
      await sendNotification();
      const tag = `term-llm-completion:${eventID}`;
      const notifications = await registration.getNotifications({ tag });
      const summary = notifications.map((notification) => ({
        tag: notification.tag,
        renotify: (notification as Notification & { renotify?: boolean }).renotify,
        url: String((notification.data as { url?: string } | null)?.url || ''),
      }));
      notifications.forEach((notification) => notification.close());
      return summary;
    }, route);
    expect(result).toEqual([
      expect.objectContaining({
        tag: 'term-llm-completion:completion:e2e-response:e2e-subscription',
        renotify: false,
      }),
    ]);
    expect(result[0]?.url).toBe(expectedURL);
  });
}

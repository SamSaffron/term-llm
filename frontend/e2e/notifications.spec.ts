import { expect, test } from '@playwright/test';

test('service worker keeps one tagged completion notification for duplicate local events', async ({
  context,
  page,
}) => {
  const origin = new URL(process.env.TERM_LLM_SMOKE_URL || 'http://127.0.0.1:18080/ui/').origin;
  await context.grantPermissions(['notifications'], { origin });
  await page.goto('./');
  const result = await page.evaluate(async () => {
    const registration = await navigator.serviceWorker.ready;
    const eventID = 'completion:e2e-response:e2e-subscription';
    const payload = {
      version: 1,
      event_id: eventID,
      response_id: 'e2e-response',
      session_id: 'e2e-session',
      outcome: 'completed',
      title: 'Response complete',
      body: 'Ready',
      url: `${location.pathname.replace(/\/$/, '')}/chat/e2e-session`,
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
  });
  expect(result).toEqual([
    expect.objectContaining({
      tag: 'term-llm-completion:completion:e2e-response:e2e-subscription',
      renotify: false,
    }),
  ]);
  expect(result[0]?.url).toContain('/ui/chat/e2e-session');
});

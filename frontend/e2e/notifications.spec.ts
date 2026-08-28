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
    registration.active?.postMessage({ type: 'completion-notification', payload });
    registration.active?.postMessage({ type: 'completion-notification', payload });
    const tag = `term-llm-completion:${eventID}`;
    for (let attempt = 0; attempt < 50; attempt += 1) {
      const notifications = await registration.getNotifications({ tag });
      if (notifications.length) {
        const summary = notifications.map((notification) => ({
          tag: notification.tag,
          renotify: notification.renotify,
          url: String((notification.data as { url?: string } | null)?.url || ''),
        }));
        notifications.forEach((notification) => notification.close());
        return summary;
      }
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    return [];
  });
  expect(result).toEqual([
    expect.objectContaining({
      tag: 'term-llm-completion:completion:e2e-response:e2e-subscription',
      renotify: false,
    }),
  ]);
  expect(result[0]?.url).toContain('/ui/chat/e2e-session');
});

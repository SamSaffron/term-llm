import { expect, test } from '@playwright/test';

test('records, transcribes, and leaves voice text in the draft', async ({ page }) => {
  await page.addInitScript(() => {
    const mediaTrack = Object.assign(new EventTarget(), { stop() {} });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: async () => ({ getTracks: () => [mediaTrack] }) },
    });
    class Recorder extends EventTarget {
      static isTypeSupported(type: string) {
        return type.startsWith('audio/mp4') || type.startsWith('audio/webm');
      }
      state: RecordingState = 'inactive';
      mimeType: string;
      constructor(_: MediaStream, options?: MediaRecorderOptions) {
        super();
        this.mimeType = options?.mimeType || 'audio/mp4';
      }
      start() {
        this.state = 'recording';
      }
      requestData() {
        this.dispatchEvent(
          new BlobEvent('dataavailable', {
            data: new Blob(['voice'], { type: this.mimeType }),
          }),
        );
      }
      stop() {
        this.state = 'inactive';
        setTimeout(() => this.dispatchEvent(new Event('stop')), 0);
      }
    }
    Object.defineProperty(window, 'MediaRecorder', { configurable: true, value: Recorder });
  });
  await page.route('**/v1/transcribe', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: '{"text":"webkit voice"}',
    }),
  );
  await page.goto('./');
  // The mocked recorder test exercises the control handler, not browser hit-testing.
  // WebKit can leave the startup overlay present while its WebRTC fallback settles, which
  // stalls Playwright's actionability checks even though the composer is already mounted.
  await page.waitForFunction(() => document.getElementById('voiceBtn'));
  await page.evaluate(() => {
    document
      .getElementById('voiceBtn')
      ?.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
  });
  await expect(page.locator('#voiceStatus')).toContainText('Recording');
  await page.getByRole('button', { name: 'Stop', exact: true }).click();
  await expect(page.locator('#voiceStatus')).toContainText('Transcription inserted');
  await expect(page.locator('#promptInput')).toHaveValue('webkit voice');
});

import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results',
  reporter: 'line',
  workers: 1,
  use: {
    baseURL: process.env.TERM_LLM_SMOKE_URL || 'http://127.0.0.1:18080/ui/',
    browserName: 'chromium',
    headless: true,
    serviceWorkers: 'allow',
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    launchOptions: {
      executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined,
      // The isolated browser-to-Go smoke uses host ICE candidates without STUN.
      // CI runners cannot reliably resolve Chromium's multicast .local names.
      // Keep real ICE/DTLS/SCTP coverage without depending on multicast routing.
      args: process.env.TERM_LLM_WEBRTC_RELAY_URL
        ? ['--disable-features=WebRtcHideLocalIpsWithMdns']
        : [],
    },
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 7'] } },
  ],
});

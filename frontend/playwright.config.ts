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
    ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE
      ? { launchOptions: { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } }
      : {}),
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 7'] } },
    {
      name: 'webkit-voice',
      testMatch: /voice\.spec\.ts/,
      use: { ...devices['Desktop Safari'], browserName: 'webkit', launchOptions: {} },
    },
  ],
});

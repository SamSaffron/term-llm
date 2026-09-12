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
    },
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 7'] } },
    {
      name: 'iphone',
      // iPhone viewport/touch layout in the CI-supported browser, not Safari emulation.
      use: { ...devices['iPhone 13'], browserName: 'chromium' },
    },
  ],
});

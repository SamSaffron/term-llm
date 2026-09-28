import { existsSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { defineConfig, devices } from '@playwright/test';

const MAC_CHROME = 'Google Chrome.app/Contents/MacOS/Google Chrome';

/**
 * The Chromium-family browser specs run in, in order of preference:
 * 1. PLAYWRIGHT_CHROMIUM_EXECUTABLE (the smoke scripts set it from a
 *    `chromium` on PATH);
 * 2. on macOS, an installed Google Chrome, so specs run without
 *    `playwright install`;
 * 3. Playwright's pinned Chromium, which CI installs.
 */
function chromiumExecutable(): string | undefined {
  const explicit = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE;
  if (explicit) return explicit;
  if (process.platform !== 'darwin') return undefined;
  return [join('/Applications', MAC_CHROME), join(homedir(), 'Applications', MAC_CHROME)].find(
    (path) => existsSync(path),
  );
}

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
      executablePath: chromiumExecutable(),
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

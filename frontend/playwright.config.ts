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

/**
 * Traces and videos are recorded for every test and kept only on failure, so
 * they cost CPU even on green runs. Smoke scripts that delete their output set
 * TERM_LLM_E2E_ARTIFACTS=off; screenshots on failure stay because they are cheap.
 */
const recordArtifacts = process.env.TERM_LLM_E2E_ARTIFACTS !== 'off';

/**
 * Specs that drive real provider runs on the shared fixture server. Under CPU
 * contention they race other workers, so the chat smoke runs them in a separate
 * single-worker phase (TERM_LLM_E2E_PHASE=serial) after the parallel phase.
 */
const serialSpecs = ['**/real-server.spec.ts', '**/webmcp.spec.ts'];
const phase = process.env.TERM_LLM_E2E_PHASE;

export default defineConfig({
  testDir: './e2e',
  ...(phase === 'parallel' ? { testIgnore: serialSpecs } : {}),
  ...(phase === 'serial' ? { testMatch: serialSpecs } : {}),
  outputDir: './test-results',
  reporter: 'line',
  workers: 1,
  use: {
    baseURL: process.env.TERM_LLM_SMOKE_URL || 'http://127.0.0.1:18080/ui/',
    browserName: 'chromium',
    headless: true,
    serviceWorkers: 'allow',
    ignoreHTTPSErrors: true,
    trace: recordArtifacts ? 'retain-on-failure' : 'off',
    screenshot: 'only-on-failure',
    video: recordArtifacts ? 'retain-on-failure' : 'off',
    launchOptions: {
      executablePath: chromiumExecutable(),
      // Headless Chromium still forwards notifications to the desktop's native
      // service (D-Bus on Linux, Notification Center on macOS), so test
      // notifications would pop up on the developer's machine. Keep them in
      // Chromium's own notification center, as on CI runners without a desktop.
      args: ['--disable-features=NativeNotifications,SystemNotifications'],
    },
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 7'] } },
    {
      name: 'iphone',
      // iPhone viewport/touch layout in the CI-supported browser, not Safari emulation.
      // `mobile` already covers generic touch layouts; this project only runs
      // specs that target the iPhone screen, so it does not repeat that suite.
      use: { ...devices['iPhone 13'], browserName: 'chromium' },
      grep: /iPhone|pinch/,
    },
  ],
});

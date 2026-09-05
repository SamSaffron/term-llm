// Captures the real browser interface with deterministic, illustrative API data.
// Start an isolated local `term-llm serve web` first; no model calls are made.
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const base = process.env.DOCS_PRODUCT_URL || "http://127.0.0.1:18765/ui/";
const parsed = new URL(base);
if (!["localhost", "127.0.0.1", "[::1]"].includes(parsed.hostname)) {
  throw new Error("Product capture must use an isolated loopback server.");
}
const output = fileURLToPath(new URL("../static/images/", import.meta.url));
await mkdir(output, { recursive: true });
const session = (id, title, number) => ({
  id, title, name: title, number, mode: "chat", origin: "web", agent: "reviewer",
  provider: "chatgpt", model: "gpt-6-astra-fast",
  created_at: 1_788_609_600, last_message_at: 1_788_609_660,
  pinned: number === 1, archived: false,
  file_change_summary: { file_count: 1, adds: 5, dels: 1, git: true },
});
const sessions = [
  session("review", "Review the retry handler", 1),
  session("architecture", "How does authentication work?", 2),
  session("tests", "Add tests for the cache", 3),
  session("research", "Research deployment options", 4),
];
const messages = [
  { id: 1, sequence: 0, role: "user", parts: [{ type: "text", text: "Review the staged changes to the retry handler. Look for edge cases and keep the review focused." }] },
  { id: 2, sequence: 1, role: "assistant", parts: [{ type: "text", text: "## One issue worth fixing\n\nThe new retry loop is clearer, but it **waits even after the request has been cancelled**. A caller can cancel the context and still be blocked for the full backoff interval.\n\n### Make the wait cancellable\n\nIn `retry.go`, replace the unconditional sleep with a timer that also watches `ctx.Done()`:\n\n```go\nselect {\ncase <-ctx.Done():\n    return ctx.Err()\ncase <-time.After(backoff):\n    // Continue to the next attempt.\n}\n```\n\n### What to test\n\n- Cancel during the backoff: the call should return promptly.\n- Cancel before the first attempt: no request should be sent.\n- Exhaust the retries: preserve the last meaningful error.\n\nThe rest of the change keeps the retry count bounded and preserves the existing error handling. **No files were modified by this review.**" }] },
];
const browser = await chromium.launch();
try {
  for (const theme of ["light", "dark"]) {
    const context = await browser.newContext({ viewport: { width: 1440, height: 940 }, colorScheme: theme, deviceScaleFactor: 1, serviceWorkers: "block" });
    const page = await context.newPage();
    await page.route("**/v1/**", async (route) => {
      const url = new URL(route.request().url());
      const path = url.pathname;
      const json = (value) => route.fulfill({ contentType: "application/json", body: JSON.stringify(value) });
      if (path.endsWith("/capabilities")) return json({ projects: { enabled: true }, shell: { enabled: true, version: 1, transport: "http_sse" } });
      if (path.endsWith("/providers")) return json({ object: "list", data: [{ name: "chatgpt", configured: true, is_default: true, default_model: "gpt-6-astra-fast", models: ["gpt-6-astra-fast"] }] });
      if (path.endsWith("/models")) return json({ object: "list", data: [{ id: "gpt-6-astra-fast", owned_by: "chatgpt" }] });
      if (path.endsWith("/sidebar")) return json({ sessions, recent_sessions: sessions });
      if (path.endsWith("/sessions/status")) return json({ sessions: [] });
      if (path.endsWith("/sessions")) return json({ sessions, selected_session: sessions[0], selected_transcript: { bodies: { messages } } });
      if (path.endsWith("/state")) return json({ session: sessions[0] });
      if (path.endsWith("/transcript")) return json({ messages });
      if (path.includes("/file-changes/diff")) return json({ diff: "@@ -18,5 +18,9 @@\n     if err == nil {\n         return nil\n     }\n-    time.Sleep(backoff)\n+    select {\n+    case <-ctx.Done():\n+        return ctx.Err()\n+    case <-time.After(backoff):\n+    }\n }" });
      if (path.includes("/file-changes")) return json({ files: [{ path: "retry.go", additions: 5, deletions: 1, status: "modified" }] });
      return json({});
    });
    await page.goto(new URL("chat/1", base).href);
    await page.getByText("One issue worth fixing", { exact: true }).waitFor({ timeout: 15000 });
    await page.getByText("gpt-6-astra-fast", { exact: true }).waitFor();
    // Open and expand the real diff component, rather than drawing a mock UI.
    await page.getByRole("button", { name: /Toggle file changes/ }).click();
    const file = page.locator('.diff-file-row[data-path="retry.go"]');
    await file.waitFor();
    await file.click();
    await page.locator(".diff-row.add").first().waitFor();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: `${output}/web-workspace-${theme}.png` });
    console.log(`Captured browser workspace (${theme}).`);
    await context.close();
  }
} finally {
  await browser.close();
}

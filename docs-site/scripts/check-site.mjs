// Run after `npm run build`. Uses only local static content; no model/API calls.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile, readdir, mkdir, stat } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import AxeBuilder from "@axe-core/playwright";

const site = path.resolve(fileURLToPath(new URL("../../.cache/docs-site/", import.meta.url)));
const results = fileURLToPath(new URL("../test-results/", import.meta.url));
await mkdir(results, { recursive: true });
const types = { ".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".json": "application/json", ".png": "image/png", ".svg": "image/svg+xml", ".wasm": "application/wasm" };
const server = createServer(async (request, response) => {
  try {
    let pathname = decodeURIComponent(new URL(request.url, "http://localhost").pathname);
    if (pathname.endsWith("/")) pathname += "index.html";
    const filename = path.resolve(site, `.${pathname}`);
    if (!filename.startsWith(`${site}${path.sep}`) && filename !== path.join(site, "index.html")) throw new Error("Invalid path");
    const data = await readFile(filename);
    response.writeHead(200, { "Content-Type": types[path.extname(filename)] || "application/octet-stream" });
    response.end(data);
  } catch (_) { response.writeHead(404); response.end("Not found"); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch();
const errors = [];
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, colorScheme: "light", permissions: ["clipboard-read", "clipboard-write"] });
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (error) => errors.push(error.message));

  // Audit every generated document and all local links/fragments/assets.
  async function walk(dir) {
    const entries = await readdir(dir, { withFileTypes: true });
    const nested = await Promise.all(entries.map((entry) => entry.isDirectory() ? walk(path.join(dir, entry.name)) : path.join(dir, entry.name)));
    return nested.flat();
  }
  const htmlFiles = (await walk(site)).filter((file) => file.endsWith(".html"));
  const documents = new Map();
  for (const filename of htmlFiles) {
    const html = await readFile(filename, "utf8");
    const parsed = await page.evaluate((source) => {
      const doc = new DOMParser().parseFromString(source, "text/html");
      return {
        ids: [...doc.querySelectorAll("[id]")].map((node) => node.id),
        links: [...doc.querySelectorAll("a[href]")].map((node) => node.getAttribute("href")),
        assets: [...doc.querySelectorAll("img[src], script[src], link[rel='stylesheet'], source[srcset]")].map((node) => node.getAttribute("src") || node.getAttribute("href") || node.getAttribute("srcset")),
        headings: doc.querySelectorAll("h1").length,
        article: !!doc.querySelector("article.markdown"),
        currentNav: doc.querySelector('.docs-navigation [aria-current="page"]')?.getAttribute("href"),
        canonical: doc.querySelector('link[rel="canonical"]')?.getAttribute("href"),
        image: doc.querySelector('meta[property="og:image"]')?.content,
        alias: !!doc.querySelector('meta[http-equiv="refresh"]'),
      };
    }, html);
    assert.equal(new Set(parsed.ids).size, parsed.ids.length, `Duplicate IDs: ${filename}`);
    if (!parsed.alias) {
      assert.equal(parsed.headings, 1, `Expected one h1: ${filename}`);
      assert.ok(parsed.canonical?.startsWith("https://term-llm.com/"), `Missing canonical: ${filename}`);
      assert.equal(parsed.image, "https://term-llm.com/images/social-card.png");
    }
    if (parsed.article) assert.ok(parsed.currentNav, `Article missing from documentation navigation: ${filename}`);
    documents.set(filename, parsed);
  }
  const broken = [];
  for (const [filename, doc] of documents) {
    const base = new URL(path.relative(site, filename).replace(/index\.html$/, ""), "https://term-llm.com/");
    for (const link of [...doc.links, ...doc.assets]) {
      if (!link || /^(mailto:|tel:|data:)/.test(link)) continue;
      const url = new URL(link, base);
      if (url.origin !== "https://term-llm.com") continue;
      let target = path.join(site, decodeURIComponent(url.pathname));
      if (url.pathname.endsWith("/")) target = path.join(target, "index.html");
      try { await stat(target); } catch (_) { broken.push(`${path.relative(site, filename)} → ${link}`); continue; }
      if (url.hash && documents.has(target) && !documents.get(target).ids.includes(decodeURIComponent(url.hash.slice(1)))) broken.push(`${path.relative(site, filename)} → missing fragment ${link}`);
    }
  }
  assert.deepEqual(broken, [], `Broken local links:\n${broken.join("\n")}`);
  assert.ok((await readFile(path.join(site, "sitemap.xml"), "utf8")).includes("https://term-llm.com/"));
  console.log(`✓ ${htmlFiles.length} documents: links, fragments, assets, headings, metadata, and sitemap`);

  async function noOverflow() {
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `Horizontal page overflow: ${page.url()}`);
  }
  async function accessible(label) {
    const scan = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa"]).analyze();
    const violations = scan.violations.map((item) => ({ id: item.id, nodes: item.nodes.map((node) => ({ target: node.target, summary: node.failureSummary })) }));
    assert.deepEqual(violations, [], `Accessibility: ${label}\n${JSON.stringify(violations, null, 2)}`);
  }
  await page.goto(origin);
  await noOverflow();
  assert.equal(await page.locator("html").evaluate((el) => getComputedStyle(el).colorScheme), "light");
  await accessible("light homepage");
  await page.screenshot({ path: path.join(results, "home-light.png"), fullPage: true });
  await page.screenshot({ path: path.join(results, "home-desktop.png") });
  await page.emulateMedia({ colorScheme: "dark" });
  assert.equal(await page.locator("html").evaluate((el) => getComputedStyle(el).colorScheme), "dark");
  await accessible("dark homepage");
  await page.screenshot({ path: path.join(results, "home-dark.png"), fullPage: true });
  await page.getByLabel("Color theme").selectOption("light");
  await page.reload();
  assert.equal(await page.locator("html").evaluate((el) => getComputedStyle(el).colorScheme), "light");
  assert.ok((await page.locator(".product-shot img").evaluate((img) => img.currentSrc)).endsWith("web-workspace-light.png"));
  await page.getByLabel("Color theme").selectOption("system");
  assert.equal(await page.locator("html").evaluate((el) => getComputedStyle(el).colorScheme), "dark");
  await page.emulateMedia({ colorScheme: "light" });
  console.log("✓ OS theme, explicit override, persistence, and matching product images");

  const firstCopy = page.locator("[data-copy-text]").first();
  await firstCopy.click();
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), "term-llm serve web");
  await page.getByRole("tab", { name: "Homebrew · macOS" }).focus();
  await page.keyboard.press("ArrowRight");
  assert.equal(await page.getByRole("tab", { name: "Shell installer" }).getAttribute("aria-selected"), "true");
  assert.ok(await page.locator("#install-shell").isVisible());
  await page.keyboard.press("Home");
  assert.equal(await page.getByRole("tab", { name: "Homebrew · macOS" }).getAttribute("aria-selected"), "true");
  console.log("✓ Clipboard copy and keyboard-operated installation tabs");

  await page.locator("[data-open-search]").click();
  const search = page.locator("#search-modal-ui input");
  await search.fill("worktrees");
  await page.locator("#search-modal-ui .pagefind-ui__result-link").first().waitFor();
  await accessible("search dialog");
  for (let i = 0; i < 15; i++) {
    await page.keyboard.press("Tab");
    assert.ok(await page.evaluate(() => document.activeElement === document.body || !!document.activeElement.closest("#search-modal")), "Focus escaped the modal");
  }
  await page.keyboard.press("Escape");
  assert.ok(await page.locator("[data-open-search]").evaluate((el) => el === document.activeElement));
  await page.keyboard.press("/");
  assert.ok(await page.locator("#search-modal").isVisible());
  await page.keyboard.press("Escape");
  await page.goto(`${origin}/search/`);
  await page.locator("#search-page-ui input").fill("providers");
  await page.locator("#search-page-ui .pagefind-ui__result-link").first().waitFor();
  await accessible("standalone search");
  console.log("✓ Real Pagefind results, shortcuts, dialog focus, and standalone search");

  for (const pathname of ["/getting-started/quickstart/", "/guides/", "/guides/web-ui-and-api/", "/reference/provider-setup-details/"]) {
    await page.goto(`${origin}${pathname}`);
    await noOverflow();
    await accessible(pathname);
  }
  await page.goto(`${origin}/getting-started/providers-and-setup/#option-10-use-local-llms-ollama-lm-studio`);
  assert.ok(await page.locator(".legacy-provider-links").getAttribute("open") !== null);
  await page.goto(`${origin}/getting-started/quickstart/`);
  await page.screenshot({ path: path.join(results, "docs-desktop.png") });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  assert.equal(await page.locator("html").evaluate((el) => getComputedStyle(el).scrollBehavior), "auto");
  await accessible("dark documentation");
  await page.screenshot({ path: path.join(results, "docs-dark.png") });
  console.log("✓ Article layouts, guide navigation, legacy fragments, dark code, reduced motion");

  await page.emulateMedia({ colorScheme: "light" });
  for (const width of [320, 390, 768, 1024]) {
    await page.setViewportSize({ width, height: 844 });
    for (const pathname of ["/", "/getting-started/quickstart/", "/guides/", "/reference/provider-setup-details/"]) {
      await page.goto(`${origin}${pathname}`);
      await noOverflow();
    }
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(origin);
  await accessible("mobile homepage");
  await page.screenshot({ path: path.join(results, "home-mobile.png"), fullPage: true });
  await page.locator(".mobile-menu summary").click();
  assert.ok(await page.getByRole("navigation", { name: "Mobile primary" }).isVisible());
  await page.getByRole("navigation", { name: "Mobile primary" }).getByRole("link", { name: "Docs", exact: true }).click();
  await page.locator(".docs-navigation summary").click();
  assert.ok(await page.getByRole("navigation", { name: "Documentation", exact: true }).isVisible());
  await page.locator(".docs-navigation summary").click();
  await accessible("mobile documentation");
  await page.screenshot({ path: path.join(results, "docs-mobile.png") });
  console.log("✓ Mobile and tablet widths, menu, and collapsible documentation navigation");

  const fallback = await browser.newContext({ javaScriptEnabled: false, viewport: { width: 390, height: 844 }, colorScheme: "light" });
  const noJS = await fallback.newPage();
  await noJS.goto(origin);
  assert.ok(await noJS.locator("#install-homebrew").isVisible());
  assert.ok(await noJS.locator("#install-shell").isVisible());
  assert.equal(await noJS.locator(".copy-button:visible").count(), 0);
  await noJS.goto(`${origin}/getting-started/quickstart/`);
  assert.ok(await noJS.getByRole("navigation", { name: "Documentation", exact: true }).isVisible());
  await fallback.close();

  const blocked = await browser.newContext();
  await blocked.addInitScript(() => Object.defineProperty(window, "localStorage", { get() { throw new Error("Storage blocked"); } }));
  const blockedPage = await blocked.newPage();
  blockedPage.on("pageerror", (error) => errors.push(error.message));
  await blockedPage.goto(origin);
  await blockedPage.getByLabel("Color theme").selectOption("dark");
  assert.equal(await blockedPage.locator("html").evaluate((el) => getComputedStyle(el).colorScheme), "dark");
  await blocked.close();

  const failures = await browser.newContext();
  await failures.addInitScript(() => Object.defineProperty(navigator, "clipboard", { value: { writeText: () => Promise.reject(new Error("Clipboard denied")) } }));
  const failurePage = await failures.newPage();
  failurePage.on("pageerror", (error) => errors.push(error.message));
  await failurePage.route("**/pagefind/pagefind-ui.js", (route) => route.abort());
  await failurePage.goto(origin);
  await failurePage.locator("[data-copy-text]").first().click();
  assert.equal(await failurePage.evaluate(() => window.getSelection().toString()), "term-llm serve web");
  await failurePage.locator("[data-open-search]").click();
  const retry = failurePage.getByRole("button", { name: "Try again", exact: true });
  await retry.waitFor();
  await failurePage.unroute("**/pagefind/pagefind-ui.js");
  await retry.click();
  await failurePage.locator("#search-modal-ui input").waitFor();
  await failures.close();
  console.log("✓ No-JavaScript navigation/install, blocked storage/clipboard, and search retry");
  assert.deepEqual(errors, [], "Browser JavaScript errors");
  await context.close();
  console.log(`All checks passed. Screenshots: ${path.relative(process.cwd(), results)}`);
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}

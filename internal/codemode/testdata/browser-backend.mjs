#!/usr/bin/env node
/** Trusted, isolated MCP browser fixture, NOT a term-llm runtime dependency.
 * Run from a checkout with frontend dependencies installed. Set
 * PLAYWRIGHT_CHROMIUM_EXECUTABLE to an already-installed browser when HOME/XDG
 * are isolated. Never connects to a shared/authenticated browser. --log-path
 * writes JSONL call/action/session evidence; stdin EOF closes the owned profile.
 * A single backend-owned lane serializes direct AND code-mode browser calls.
 * Cancellation retains the lane until actual completion and requires explicit
 * observe/screenshot reconciliation before more input. No effectful retries.
 */
import { appendFileSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { createRequire } from "node:module";
import { randomUUID } from "node:crypto";

const argv = process.argv.slice(2);
const option = (name) => {
  const i = argv.indexOf(name);
  return i < 0 ? undefined : argv[i + 1];
};
const logPath = option("--log-path") || process.env.BROWSER_BACKEND_LOG;
const sessionId = randomUUID();
const req = createRequire(import.meta.url);
const playwright = req(
  process.env.PLAYWRIGHT_MODULE || "../../../frontend/node_modules/playwright",
);
function log(event, fields = {}) {
  const line =
    JSON.stringify({
      time: new Date().toISOString(),
      event,
      sessionId,
      ...fields,
    }) + "\n";
  if (logPath) appendFileSync(logPath, line);
  else process.stderr.write(line);
}
function send(id, result) {
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id, result }) + "\n");
}
function error(id, code, message) {
  process.stdout.write(
    JSON.stringify({ jsonrpc: "2.0", id, error: { code, message } }) + "\n",
  );
}
function notify(method) {
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", method }) + "\n");
}
const object = (properties) => ({
  type: "object",
  properties,
  additionalProperties: false,
});
const number = { type: "number" },
  string = { type: "string" },
  integer = { type: "integer" };
let schemaGeneration = 1,
  detailRemoved = false;
function schemas() {
  return [
    {
      name: "click",
      inputSchema: {
        ...object({ x: number, y: number }),
        required: ["x", "y"],
      },
    },
    {
      name: "type",
      inputSchema: {
        ...object({ text: string, delay: number }),
        required: ["text"],
      },
    },
    {
      name: "observe",
      inputSchema: object({ selector: string, maxElements: integer }),
    },
    {
      name: "wait",
      inputSchema: object({ selector: string, ms: number, timeout: number }),
    },
    { name: "screenshot", inputSchema: object({}) },
    {
      name: "navigate",
      inputSchema: { ...object({ url: string }), required: ["url"] },
    },
    { name: "search", inputSchema: object({ page: integer }) },
    ...(!detailRemoved
      ? [
          {
            name: "detail",
            inputSchema: {
              ...object({
                id: integer,
                ...(schemaGeneration > 1 ? { generation: integer } : {}),
              }),
              required: ["id"],
            },
          },
        ]
      : []),
    { name: "text_only", inputSchema: object({}) },
    { name: "fixture_error", inputSchema: object({}) },
    {
      name: "refresh_schema",
      inputSchema: object({ remove: { type: "boolean" } }),
    },
  ].map((t) => ({
    ...t,
    description: `Owned fixture ${t.name}; never replay unknown actions.`,
  }));
}
const html = `<!doctype html><meta charset="utf-8"><title>Code mode isolated page</title>
<style>body{font:16px sans-serif;background:#f4f7fa}input{position:absolute;left:60px;top:75px;width:200px;height:30px;box-sizing:border-box}button{position:absolute;left:30px;top:124px;width:120px;height:32px}#result{position:absolute;left:20px;top:172px}</style>
<h1>Code mode isolated browser</h1><input id="q" placeholder="type here"><button id="submit">Submit</button><div id="result"></div>
<script>document.querySelector('#submit').onclick=()=>document.querySelector('#result').textContent='submitted:'+document.querySelector('#q').value</script>`;
let context,
  page,
  profile,
  ready,
  initialized = false,
  closing = false;
function allowedURL(url) {
  try {
    const u = new URL(url);
    return (
      u.protocol === "data:" ||
      url === "about:blank" ||
      (["http:", "https:"].includes(u.protocol) &&
        ["127.0.0.1", "localhost", "[::1]"].includes(u.hostname))
    );
  } catch {
    return false;
  }
}
async function init() {
  profile = await mkdtemp(join(tmpdir(), "codemode-browser-"));
  log("session_start", { profile });
  try {
    context = await playwright.chromium.launchPersistentContext(profile, {
      headless: !argv.includes("--headed"),
      executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE,
      viewport: { width: 1280, height: 720 },
      args: ["--no-sandbox", "--disable-dev-shm-usage", "--disable-extensions"],
    });
    // Even a data: page cannot silently fetch effectful external URLs.
    await context.route("**/*", (r) =>
      allowedURL(r.request().url()) ? r.continue() : r.abort(),
    );
    page = context.pages()[0] || (await context.newPage());
    const url = option("--url");
    if (url) {
      if (!allowedURL(url)) throw Error("only local URLs allowed");
      await page.goto(url, { timeout: 10000 });
    } else await page.setContent(html);
    log("session_ready", { url: page.url(), viewport: page.viewportSize() });
  } catch (e) {
    await context?.close().catch(() => {});
    await rm(profile, { recursive: true, force: true });
    log("init_failed", { error: e.message });
    throw e;
  }
}
let lane = Promise.resolve(),
  quarantined = false,
  ticket = 0;
const pending = new Set();
const reconcile = new Set(["observe", "screenshot"]);
async function action(id, name, fn) {
  if (quarantined && !reconcile.has(name))
    throw Object.assign(Error("reconciliation required before input"), {
      outcome_unknown: true,
    });
  const n = ++ticket;
  const current = lane.then(async () => {
    if (quarantined && !reconcile.has(name))
      throw Object.assign(Error("cancelled before dispatch; reconcile"), {
        outcome_unknown: true,
      });
    log("action_start", { id, name, ticket: n });
    try {
      const value = await fn();
      if (reconcile.has(name)) quarantined = false;
      log("action_end", { id, name, ticket: n, ok: true });
      return value;
    } catch (e) {
      quarantined = true;
      log("action_end", { id, name, ticket: n, ok: false, error: e.message });
      throw Object.assign(e, { outcome_unknown: true });
    }
  });
  lane = current.catch(() => {});
  return current;
}
const rows = Array.from({ length: 8 }, (_, i) => ({
  id: i + 1,
  score: (i + 1) * 10,
  keep: i % 2 === 1,
  price: (i + 1) * 1.25,
  title: `Fixture item ${i + 1}`,
}));
async function call(id, name, args) {
  log("call", { id, name, args });
  if (name === "search") {
    const p = args.page ?? 1;
    if (!Number.isInteger(p) || p < 1 || p > 2) throw Error("invalid page");
    const items = rows.slice((p - 1) * 4, p * 4);
    return {
      ids: items.map((x) => x.id),
      items,
      page: p,
      nextPage: p === 1 ? 2 : null,
      sessionId,
    };
  }
  if (name === "detail") {
    if (detailRemoved) throw Error("tool removed");
    const row = rows.find((x) => x.id === args.id);
    if (!row) throw Error("invalid id");
    return { ...row, sessionId, padding: "raw-private-fixture-".repeat(3000) };
  }
  if (name === "refresh_schema") {
    schemaGeneration++;
    detailRemoved = !!args.remove;
    notify("notifications/tools/list_changed");
    return { schemaGeneration, sessionId };
  }
  if (name === "text_only") return { fixtureText: true };
  if (name === "fixture_error") throw Error("intentional fixture error");
  await ready;
  return action(id, name, async () => {
    switch (name) {
      case "click":
        if (
          !Number.isFinite(args.x) ||
          !Number.isFinite(args.y) ||
          args.x < 0 ||
          args.y < 0 ||
          args.x > 1280 ||
          args.y > 720
        )
          throw Error("invalid coordinates");
        await page.mouse.click(args.x, args.y);
        return { ok: true, sessionId };
      case "type":
        if (
          typeof args.text !== "string" ||
          args.text.length > 4096 ||
          !Number.isFinite(args.delay ?? 0) ||
          (args.delay ?? 0) < 0 ||
          (args.delay ?? 0) * args.text.length > 5000
        )
          throw Error("invalid bounded text/delay");
        await page.keyboard.type(args.text, { delay: args.delay ?? 0 });
        return { ok: true, sessionId };
      case "wait": {
        const timeout = args.timeout ?? 5000;
        if (!Number.isFinite(timeout) || timeout < 100 || timeout > 30000)
          throw Error("invalid timeout");
        if (args.selector)
          await page.waitForSelector(args.selector, { timeout });
        else {
          const ms = args.ms ?? 1000;
          if (!Number.isFinite(ms) || ms < 0 || ms > 30000)
            throw Error("invalid wait");
          await new Promise((r) => setTimeout(r, Math.min(ms, timeout)));
        }
        return { ok: true, sessionId };
      }
      case "navigate":
        if (!allowedURL(args.url)) throw Error("only local URLs allowed");
        await page.goto(args.url, {
          waitUntil: "domcontentloaded",
          timeout: 10000,
        });
        return { ok: true, url: page.url(), sessionId };
      case "observe": {
        const max = args.maxElements ?? 64;
        if (!Number.isInteger(max) || max < 1 || max > 128)
          throw Error("invalid maxElements");
        const elements = await page
          .locator(args.selector || "input,button,a,#result")
          .evaluateAll(
            (els, max) =>
              els.slice(0, max).map((el) => {
                const r = el.getBoundingClientRect();
                return {
                  tag: el.tagName.toLowerCase(),
                  name: el.textContent.trim(),
                  value: el.value ?? null,
                  bounds: { x: r.x, y: r.y, width: r.width, height: r.height },
                };
              }),
            max,
          );
        return {
          sessionId,
          url: page.url(),
          title: await page.title(),
          elements,
        };
      }
      case "screenshot": {
        const data = await page.screenshot({ type: "png", timeout: 10000 });
        const dimensions = page.viewportSize();
        return {
          data: data.toString("base64"),
          mimeType: "image/png",
          dimensions,
          sessionId,
          frameId: randomUUID(),
          coordinateSpace: "viewport-css",
          crop: { x: 0, y: 0, ...dimensions },
          scale: 1,
        };
      }
      default:
        throw Error("unknown tool");
    }
  });
}
async function handle(m) {
  if (m.id == null) {
    if (
      m.method === "notifications/cancelled" &&
      pending.has(m.params?.requestId)
    ) {
      quarantined = true;
      log("cancelled", { id: m.params.requestId, outcome_unknown: true });
    }
    return;
  }
  const { id, method, params } = m;
  if (method === "initialize") {
    if (initialized) {
      error(id, -32600, "already initialized");
      return;
    }
    initialized = true;
    ready = init();
    ready.catch(() => {});
    send(id, {
      protocolVersion: params?.protocolVersion || "2025-03-26",
      capabilities: { tools: { listChanged: true } },
      serverInfo: { name: "computer", version: "1.0.0" },
    });
    return;
  }
  if (!initialized) {
    error(id, -32002, "not initialized");
    return;
  }
  if (method === "tools/list") {
    send(id, { tools: schemas() });
    return;
  }
  if (method === "ping") {
    send(id, {});
    return;
  }
  if (method !== "tools/call") {
    error(id, -32601, "unknown method");
    return;
  }
  const name = params?.name,
    args = params?.arguments || {};
  if (!schemas().some((x) => x.name === name)) {
    error(id, -32602, "unknown tool");
    return;
  }
  pending.add(id);
  try {
    const value = await call(id, name, args);
    if (name === "text_only") {
      send(id, {
        content: [{ type: "text", text: "plain text, not parsed JSON" }],
      });
      return;
    }
    if (name === "screenshot") {
      const { data, ...meta } = value;
      send(id, {
        content: [{ type: "image", data, mimeType: "image/png" }],
        structuredContent: meta,
      });
      return;
    }
    send(id, {
      content: [
        {
          type: "text",
          text: name === "detail" ? `detail ${args.id}` : JSON.stringify(value),
        },
      ],
      structuredContent: value,
    });
  } catch (e) {
    send(id, {
      isError: true,
      content: [{ type: "text", text: e.message }],
      structuredContent: {
        error: e.message,
        outcome_unknown: !!e.outcome_unknown,
        sessionId,
      },
    });
  } finally {
    pending.delete(id);
    log("call_end", { id, name });
  }
}
async function cleanup() {
  if (closing) return;
  closing = true;
  await ready?.catch(() => {});
  await context?.close().catch(() => {});
  if (profile) await rm(profile, { recursive: true, force: true });
  log("session_end", { profile });
}
const input = createInterface({ input: process.stdin, crlfDelay: Infinity });
input.on("line", (line) => {
  try {
    const m = JSON.parse(line);
    handle(m).catch((e) => log("handler_error", { error: e.message }));
  } catch {
    error(null, -32700, "invalid JSON");
  }
});
input.on("close", () => cleanup().then(() => process.exit(0)));
for (const signal of ["SIGTERM", "SIGINT"])
  process.on(signal, () => cleanup().then(() => process.exit(0)));
log("process_start", { pid: process.pid });

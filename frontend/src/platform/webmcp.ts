import type { ClientTool } from '../domain/client-tools';

/**
 * Adapter for WebMCP (`document.modelContext`): tools the page itself offers.
 * Chrome ships WebMCP natively; the term-llm iOS app injects a compatible
 * implementation carrying device tools. This module is the only place the
 * chat touches that API.
 */
export interface PageToolHost {
  list(): Promise<ClientTool[]>;
  /** Resolves with the tool's result, or rejects with the tool's error. */
  execute(name: string, input: Record<string, unknown>, signal: AbortSignal): Promise<unknown>;
  /** Calls `listener` whenever the page's tools change; returns an unsubscribe. */
  subscribe(listener: () => void): () => void;
  /** Where the tools run, e.g. "iPhone" for the native app; empty when unknown. */
  provider(): string;
}

interface ModelContextTool {
  name?: unknown;
  title?: unknown;
  description?: unknown;
  inputSchema?: unknown;
  annotations?: { readOnlyHint?: unknown } | null;
}

interface ModelContext extends EventTarget {
  getTools(): Promise<ModelContextTool[]>;
  executeTool(
    tool: ModelContextTool,
    input: Record<string, unknown>,
    options?: { signal?: AbortSignal },
  ): Promise<unknown>;
}

/** Set by the native app alongside the tools it injects. */
interface DeviceToolsMarker {
  device?: unknown;
}

type WebMCPWindow = Window & { __termLLMDeviceTools?: DeviceToolsMarker };

function modelContext(doc: Document): ModelContext | null {
  const context =
    (doc as Document & { modelContext?: ModelContext }).modelContext ??
    (doc.defaultView?.navigator as (Navigator & { modelContext?: ModelContext }) | undefined)
      ?.modelContext;
  return context && typeof context.getTools === 'function' ? context : null;
}

function schema(value: unknown): Record<string, unknown> {
  let parsed = value;
  // Early Chrome builds report the schema as a JSON string.
  if (typeof value === 'string') {
    try {
      parsed = JSON.parse(value);
    } catch {
      parsed = null;
    }
  }
  return parsed && typeof parsed === 'object' && !Array.isArray(parsed)
    ? (parsed as Record<string, unknown>)
    : { type: 'object', properties: {} };
}

function clientTool(raw: ModelContextTool): ClientTool | null {
  if (typeof raw.name !== 'string' || !raw.name) return null;
  return {
    name: raw.name,
    title: typeof raw.title === 'string' ? raw.title : '',
    description: typeof raw.description === 'string' ? raw.description : '',
    inputSchema: schema(raw.inputSchema),
    readOnly: raw.annotations?.readOnlyHint === true,
  };
}

const MAX_PROVIDER_LABEL = 40;

/** A short single-line label: it is shown in the UI and quoted to the model. */
function providerLabel(value: unknown): string {
  if (typeof value !== 'string') return '';
  // Control and format characters (including bidi overrides) become spaces.
  return value
    .replace(/[\p{Cc}\p{Cf}]+/gu, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, MAX_PROVIDER_LABEL);
}

export function pageToolHost(doc: Document = document): PageToolHost {
  // executeTool expects the descriptor getTools returned, so keep the latest
  // listing. When names repeat, the first wins everywhere, so the tool the
  // model was shown is the one that runs.
  let descriptors = new Map<string, ModelContextTool>();
  // An older listing may finish last; only the newest may replace descriptors.
  let listing = 0;
  return {
    async list() {
      const context = modelContext(doc);
      if (!context) return [];
      const current = ++listing;
      const next = new Map<string, ModelContextTool>();
      for (const tool of await context.getTools())
        if (tool && typeof tool.name === 'string' && tool.name && !next.has(tool.name))
          next.set(tool.name, tool);
      if (current === listing) descriptors = next;
      return [...next.values()].map(clientTool).filter((tool): tool is ClientTool => !!tool);
    },
    async execute(name, input, signal) {
      const context = modelContext(doc);
      if (!context) throw new Error('This page no longer provides tools');
      const descriptor = descriptors.get(name);
      if (!descriptor) throw new Error(`This page does not provide the tool ${name}`);
      return await context.executeTool(descriptor, input, { signal });
    },
    subscribe(listener) {
      let context = modelContext(doc);
      context?.addEventListener('toolchange', listener);
      // A native host may inject the API after this script ran; look again
      // once the page has loaded.
      const view = doc.defaultView;
      const lateAttach = () => {
        if (context) return;
        context = modelContext(doc);
        if (!context) return;
        context.addEventListener('toolchange', listener);
        listener();
      };
      const waiting = !context && doc.readyState !== 'complete';
      if (waiting) view?.addEventListener('load', lateAttach, { once: true });
      return () => {
        if (waiting) view?.removeEventListener('load', lateAttach);
        context?.removeEventListener('toolchange', listener);
      };
    },
    provider() {
      return providerLabel((doc.defaultView as WebMCPWindow | null)?.__termLLMDeviceTools?.device);
    },
  };
}

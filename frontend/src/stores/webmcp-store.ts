import { computed, signal, type ReadonlySignal, type Signal } from '@preact/signals';
import {
  clientToolDefinitions,
  isValidClientToolName,
  limitToolOutput,
  parseToolArguments,
  stringifyToolResult,
  type ClientTool,
  type ClientToolDefinition,
  type PendingClientCall,
} from '../domain/client-tools';
import { errorMessage } from '../domain/text';
import { readJSON, writeJSON } from '../platform/storage';
import { pageToolHost, type PageToolHost } from '../platform/webmcp';
import type { AppStoreServices } from './app-store-services';

function rejectOnAbort(signal: AbortSignal): Promise<never> {
  if (signal.aborted) return Promise.reject(signal.reason);
  return new Promise((_, reject) =>
    signal.addEventListener('abort', () => reject(signal.reason), { once: true }),
  );
}

/** A tool must answer within this window so a stalled device can't wedge the chat. */
export const CLIENT_TOOL_TIMEOUT_MS = 60_000;
const REMEMBERED_SESSIONS = 200;

export interface ClientToolResult {
  ok: boolean;
  /** Text sent back to the model: the result, or a description of the failure. */
  output: string;
}

/** What the run engine needs from whoever provides client tools. */
export interface ClientToolBridge {
  /** Definitions to offer the model in `sessionId`; empty when off or unavailable. */
  definitions(sessionId: string): ClientToolDefinition[];
  /** Short label for status text, e.g. "iPhone". */
  provider(): string;
  /** Runs one call. Never rejects: failures become an unsuccessful result. */
  run(call: PendingClientCall, signal: AbortSignal): Promise<ClientToolResult>;
}

/**
 * Page-provided (WebMCP) tools. They appear in the MCP dialog as one more
 * server, switched per conversation like the configured ones.
 *
 * Trust model: whoever installs `document.modelContext` on this page (the
 * term-llm iOS app, or the browser) is trusted like a configured MCP server.
 * Its tools are on by default and run without per-call confirmation, because
 * a page only has tools when its host opted in. Their names, descriptions,
 * schemas, and results reach the model as-is apart from length limits.
 */
export class WebMCPStore implements ClientToolBridge {
  /** The page's tools whose names can be offered to a model. */
  readonly tools = signal<ClientTool[]>([]);
  readonly providerName = signal('');
  readonly available: ReadonlySignal<boolean> = computed(() => this.tools.value.length > 0);
  private readonly disabledSessions: Signal<string[]>;
  private unsubscribe: () => void = () => undefined;
  /** Only the newest listing may land; an older one can finish last. */
  private listing = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: PageToolHost = pageToolHost(),
  ) {
    this.disabledSessions = signal(this.storedDisabledSessions());
  }

  /** Loads the page's tools and follows later changes. */
  start(): void {
    this.unsubscribe();
    this.unsubscribe = this.host.subscribe(() => void this.refresh());
    void this.refresh();
  }

  dispose(): void {
    this.unsubscribe();
    this.unsubscribe = () => undefined;
    this.listing += 1;
  }

  async refresh(): Promise<void> {
    const listing = ++this.listing;
    let tools: ClientTool[] = [];
    try {
      tools = (await this.host.list()).filter((tool) => isValidClientToolName(tool.name));
    } catch {
      /* A failing page has no tools. */
    }
    if (listing !== this.listing) return;
    this.tools.value = tools;
    this.providerName.value = tools.length ? this.host.provider() : '';
  }

  /** Another tab changed the per-conversation choices. */
  reloadSettings(): void {
    this.disabledSessions.value = this.storedDisabledSessions();
  }

  provider(): string {
    return this.providerName.value || 'browser';
  }

  enabledFor(sessionId: string): boolean {
    return !this.disabledSessions.value.includes(sessionId);
  }

  setEnabled(sessionId: string, enabled: boolean): void {
    if (!sessionId) return;
    // Start from storage so another tab's recent choice isn't overwritten.
    const others = this.storedDisabledSessions().filter((id) => id !== sessionId);
    this.persist(enabled ? others : [...others, sessionId]);
  }

  /** A draft conversation became durable: carry its choice over. */
  rekey(oldId: string, newId: string): void {
    if (!oldId || !newId) return;
    const stored = this.storedDisabledSessions();
    if (!stored.includes(oldId)) return;
    this.persist([...stored.filter((id) => id !== oldId && id !== newId), newId]);
  }

  /** Reactive: an effect reading this follows tool and per-conversation changes. */
  definitions(sessionId: string): ClientToolDefinition[] {
    return this.enabledFor(sessionId)
      ? clientToolDefinitions(this.tools.value, this.providerName.value)
      : [];
  }

  async run(call: PendingClientCall, signal: AbortSignal): Promise<ClientToolResult> {
    // Run only what the page offers now; the call's name comes from the model.
    if (!this.tools.peek().some((tool) => tool.name === call.name))
      return { ok: false, output: `Error: this page no longer provides the tool ${call.name}` };
    let input: Record<string, unknown>;
    try {
      input = parseToolArguments(call.arguments);
    } catch (error) {
      return { ok: false, output: `Error: ${errorMessage(error)}` };
    }
    // The page may ignore the signal, so stopping and the timeout win the race.
    const controller = new AbortController();
    const stop = () => controller.abort(signal.reason);
    signal.addEventListener('abort', stop, { once: true });
    const timer = window.setTimeout(
      () => controller.abort(new Error(`timed out after ${CLIENT_TOOL_TIMEOUT_MS / 1000}s`)),
      CLIENT_TOOL_TIMEOUT_MS,
    );
    try {
      signal.throwIfAborted();
      const result = await Promise.race([
        this.host.execute(call.name, input, controller.signal),
        rejectOnAbort(controller.signal),
      ]);
      return { ok: true, output: limitToolOutput(stringifyToolResult(result)) };
    } catch (error) {
      return { ok: false, output: limitToolOutput(`Error: ${errorMessage(error)}`) };
    } finally {
      window.clearTimeout(timer);
      signal.removeEventListener('abort', stop);
    }
  }

  private storedDisabledSessions(): string[] {
    const stored = readJSON<unknown>(
      this.services.storage,
      this.services.keys.webMCPDisabledSessions,
      [],
    );
    return Array.isArray(stored) ? stored.filter((id): id is string => typeof id === 'string') : [];
  }

  private persist(sessions: string[]): void {
    const kept = sessions.slice(-REMEMBERED_SESSIONS);
    this.disabledSessions.value = kept;
    writeJSON(this.services.storage, this.services.keys.webMCPDisabledSessions, kept);
  }
}

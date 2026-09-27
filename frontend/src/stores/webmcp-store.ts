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

/** A conversation's own page-tools choice, oldest first. */
type SessionChoice = [sessionId: string, enabled: boolean];

/**
 * Page-provided (WebMCP) tools. They appear in the MCP dialog as one more
 * server, switched per conversation like the configured ones.
 *
 * Trust model: whoever installs `document.modelContext` on this page (the
 * term-llm iOS app, or the browser) is trusted like a configured MCP server.
 * Its tools run without per-call confirmation, because a page only has tools
 * when its host opted in. Their names, descriptions, schemas, and results
 * reach the model as-is apart from length limits.
 *
 * They are off until turned on. The last choice made in any conversation
 * becomes the default for new ones, remembered per browser and per Hub node.
 * A conversation keeps the choice it first sent with, so changing the default
 * never adds or removes tools in the middle of an older conversation.
 */
export class WebMCPStore implements ClientToolBridge {
  /** The page's tools whose names can be offered to a model. */
  readonly tools = signal<ClientTool[]>([]);
  readonly providerName = signal('');
  readonly available: ReadonlySignal<boolean> = computed(() => this.tools.value.length > 0);
  private readonly choices: Signal<SessionChoice[]>;
  private readonly defaultOn: Signal<boolean>;
  private unsubscribe: () => void = () => undefined;
  /** Only the newest listing may land; an older one can finish last. */
  private listing = 0;

  constructor(
    private readonly services: AppStoreServices,
    private readonly host: PageToolHost = pageToolHost(),
  ) {
    this.migrateLegacyOptOuts();
    this.choices = signal(this.storedChoices());
    this.defaultOn = signal(this.storedDefault());
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

  /** Another tab changed the page-tools choices. */
  reloadSettings(): void {
    this.choices.value = this.storedChoices();
    this.defaultOn.value = this.storedDefault();
  }

  provider(): string {
    return this.providerName.value || 'browser';
  }

  enabledFor(sessionId: string): boolean {
    const choice = this.choices.value.find(([id]) => id === sessionId);
    return choice ? choice[1] : this.defaultOn.value;
  }

  /** Sets this conversation's choice; it also becomes the default for new ones. */
  setEnabled(sessionId: string, enabled: boolean): void {
    if (!sessionId) return;
    this.persistDefault(enabled);
    this.remember(sessionId, enabled);
  }

  /** A draft conversation became durable: carry its choice over. */
  rekey(oldId: string, newId: string): void {
    if (!oldId || !newId) return;
    const stored = this.storedChoices();
    const choice = stored.find(([id]) => id === oldId);
    if (!choice) return;
    this.persistChoices([
      ...stored.filter(([id]) => id !== oldId && id !== newId),
      [newId, choice[1]],
    ]);
  }

  /**
   * Definitions for the next request in `sessionId`. Offering the page's tools
   * pins the conversation to its current choice, so a later default change
   * elsewhere can't alter what an existing conversation was started with.
   */
  definitions(sessionId: string): ClientToolDefinition[] {
    if (!this.available.peek()) return [];
    const enabled = this.enabledFor(sessionId);
    if (sessionId && !this.storedChoices().some(([id]) => id === sessionId))
      this.remember(sessionId, enabled);
    return enabled ? clientToolDefinitions(this.tools.peek(), this.providerName.peek()) : [];
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

  /** Records one conversation's choice, starting from storage so another tab's isn't lost. */
  private remember(sessionId: string, enabled: boolean): void {
    this.persistChoices([
      ...this.storedChoices().filter(([id]) => id !== sessionId),
      [sessionId, enabled],
    ]);
  }

  private storedChoices(): SessionChoice[] {
    const stored = readJSON<unknown>(this.services.storage, this.services.keys.webMCPSessions, []);
    if (!Array.isArray(stored)) return [];
    return stored.filter(
      (entry): entry is SessionChoice =>
        Array.isArray(entry) &&
        entry.length === 2 &&
        typeof entry[0] === 'string' &&
        typeof entry[1] === 'boolean',
    );
  }

  private storedDefault(): boolean {
    return (
      readJSON<unknown>(this.services.storage, this.services.keys.webMCPDefault, false) === true
    );
  }

  private persistChoices(choices: SessionChoice[]): void {
    const kept = choices.slice(-REMEMBERED_SESSIONS);
    this.choices.value = kept;
    writeJSON(this.services.storage, this.services.keys.webMCPSessions, kept);
  }

  private persistDefault(enabled: boolean): void {
    this.defaultOn.value = enabled;
    writeJSON(this.services.storage, this.services.keys.webMCPDefault, enabled);
  }

  /** Earlier builds stored only opt-outs; keep those conversations off. */
  private migrateLegacyOptOuts(): void {
    const { storage, keys } = this.services;
    const legacy = readJSON<unknown>(storage, keys.webMCPLegacyDisabledSessions, null);
    if (legacy === null) return;
    storage.removeItem(keys.webMCPLegacyDisabledSessions);
    if (!Array.isArray(legacy) || storage.getItem(keys.webMCPSessions) !== null) return;
    const optedOut = legacy.filter((id): id is string => typeof id === 'string');
    writeJSON(
      storage,
      keys.webMCPSessions,
      optedOut.slice(-REMEMBERED_SESSIONS).map((id): SessionChoice => [id, false]),
    );
  }
}

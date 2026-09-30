import { PersistentCache, type CacheLimits } from '../../platform/persistent-cache';
import { nodeResumePath } from '../domain/formatting';
import type { HubConfig } from '../config';
import type {
  HubNode,
  HubInputRequiredItem,
  HubAttentionInboxItem,
  HubDelegation,
} from '../domain/types';

export const HUB_CACHE_SCHEMA = 1;
export const HUB_CACHE_MAX_AGE = 7 * 24 * 60 * 60 * 1_000;
export const HUB_CACHE_LIMITS: CacheLimits = {
  maxEntries: 3,
  maxBytes: 384 * 1_024,
  maxRecordBytes: 128 * 1_024,
};
export interface HubCacheData {
  nodes: HubNode[];
  attention: {
    inputRequired: HubInputRequiredItem[];
    inbox: HubAttentionInboxItem[];
    totalInputRequired: number;
    totalUnseen: number;
    hasMore: boolean;
  };
  delegations: HubDelegation[];
}
export type HubCacheSection = keyof HubCacheData;
export interface HubCacheRecord<T> {
  schema: number;
  timestamp: number;
  data: T;
}

/** Only retain an innocuous navigation parameter; signed queries/userinfo/fragments never persist. */
export function cacheNavigation(value: string): string {
  if (!value) return '';
  try {
    const url = new URL(value, window.location.origin);
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) return '';
    const newChat = url.searchParams.get('new') === '1';
    url.search = newChat ? '?new=1' : '';
    url.hash = '';
    return value.startsWith('/') && !value.startsWith('//')
      ? `${url.pathname}${url.search}`
      : url.href;
  } catch {
    return '';
  }
}
const text = (value: unknown) => (typeof value === 'string' ? value.slice(0, 512) : '');

/** Explicit allowlists: no tokens, diagnostics, prompts, responses, artifacts or auth state. */
export function cacheNodes(nodes: HubNode[]): HubNode[] {
  return nodes.slice(0, 200).map((node) => ({
    id: text(node.id),
    name: text(node.name),
    source: text(node.source),
    connection: text(node.connection),
    url: cacheNavigation(node.url),
    base_path: cacheNavigation(node.base_path),
    proxy_path: cacheNavigation(node.proxy_path),
    new_session_path: cacheNavigation(node.new_session_path),
    has_token: false,
    sessions: node.sessions
      ? {
          count_label: '',
          resume_path: cacheNavigation(nodeResumePath(node)),
        }
      : undefined,
    status: {
      reachable: Boolean(node.status?.reachable),
      state: 'last known',
      latency_ms: 0,
    },
  }));
}
export function cacheAttention(data: HubCacheData['attention']): HubCacheData['attention'] {
  const summary = (item: HubAttentionInboxItem | HubInputRequiredItem) => ({
    node_id: text(item.node_id),
    node_name: text(item.node_name),
    session_id: text(item.session_id),
    session_number: item.session_number,
    title: text(item.title),
    resume_path: cacheNavigation(item.resume_path),
  });
  return {
    inputRequired: data.inputRequired.slice(0, 50).map((item) => ({
      ...summary(item),
      pending_interaction_count: item.pending_interaction_count,
      required_since: item.required_since,
      stale: true,
    })),
    inbox: data.inbox.slice(0, 50).map((item) => ({
      ...summary(item),
      outcome: text(item.outcome),
      attention_seq: item.attention_seq,
      terminal_at: item.terminal_at,
    })),
    totalInputRequired: data.totalInputRequired,
    totalUnseen: data.totalUnseen,
    hasMore: data.hasMore || data.inputRequired.length > 50 || data.inbox.length > 50,
  };
}
export function cacheDelegations(items: HubDelegation[]): HubDelegation[] {
  return items.slice(0, 50).map((item) => ({
    id: text(item.id),
    origin_node: text(item.origin_node),
    target_node: text(item.target_node),
    agent_name: text(item.agent_name),
    status: text(item.status),
    depth: item.depth,
    created_at: item.created_at,
    updated_at: item.updated_at,
  }));
}
function record(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}
function validItems(
  value: unknown,
  keys: string[],
  limit: number,
  numbers: string[] = [],
): boolean {
  return (
    Array.isArray(value) &&
    value.length <= limit &&
    value.every(
      (item: unknown) =>
        record(item) &&
        keys.every((key) => typeof item[key] === 'string') &&
        numbers.every((key) => typeof item[key] === 'number' && Number.isFinite(item[key])),
    )
  );
}
function validData(section: HubCacheSection, data: unknown): boolean {
  if (section === 'nodes') {
    return (
      validItems(data, ['id', 'name', 'url', 'proxy_path', 'new_session_path', 'base_path'], 200) &&
      (data as HubNode[]).every(
        (node) =>
          record(node.status) &&
          typeof node.status.reachable === 'boolean' &&
          (node.sessions === undefined ||
            (record(node.sessions) && typeof node.sessions.resume_path === 'string')),
      )
    );
  }
  if (section === 'delegations')
    return validItems(
      data,
      ['id', 'origin_node', 'target_node', 'status', 'created_at', 'updated_at'],
      50,
      ['depth'],
    );
  return (
    record(data) &&
    validItems(data.inputRequired, ['node_id', 'session_id', 'title', 'resume_path'], 50, [
      'pending_interaction_count',
    ]) &&
    validItems(data.inbox, ['node_id', 'session_id', 'title', 'resume_path'], 50, [
      'attention_seq',
    ]) &&
    typeof data.totalInputRequired === 'number' &&
    typeof data.totalUnseen === 'number' &&
    typeof data.hasMore === 'boolean'
  );
}

/** Operations are serialized so an in-flight write cannot resurrect data after a purge. */
export class HubCache {
  readonly namespace: string;
  private queue: Promise<unknown> = Promise.resolve();
  private revoked = false;
  constructor(
    config: Pick<HubConfig, 'basePath' | 'cacheScope'>,
    private readonly cache = new PersistentCache(),
    origin = window.location.origin,
  ) {
    this.namespace = config.cacheScope
      ? `hub:${JSON.stringify([origin, config.basePath, config.cacheScope])}:`
      : '';
  }
  private enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const next = this.queue.then(operation);
    this.queue = next.catch(() => undefined);
    return next;
  }
  private async readRecord<K extends HubCacheSection>(
    section: K,
  ): Promise<HubCacheRecord<HubCacheData[K]> | null> {
    if (!this.namespace || this.revoked) return null;
    const row = await this.cache.get<HubCacheRecord<HubCacheData[K]>>(this.namespace, section);
    const value = row?.value;
    if (!value) return null;
    const age = Date.now() - value.timestamp;
    if (
      value.schema !== HUB_CACHE_SCHEMA ||
      typeof value.timestamp !== 'number' ||
      !Number.isFinite(age) ||
      age < 0 ||
      age > HUB_CACHE_MAX_AGE ||
      !validData(section, value.data)
    ) {
      await this.cache.delete(this.namespace, section);
      return null;
    }
    return value;
  }
  read<K extends HubCacheSection>(section: K): Promise<HubCacheRecord<HubCacheData[K]> | null> {
    return this.enqueue(() => this.readRecord(section));
  }
  private writeRecord<K extends HubCacheSection>(
    section: K,
    data: HubCacheData[K],
    timestamp: number,
  ): Promise<unknown> {
    if (!this.namespace || this.revoked) return Promise.resolve();
    return this.cache.put(
      this.namespace,
      section,
      {
        schema: HUB_CACHE_SCHEMA,
        timestamp,
        data,
      },
      HUB_CACHE_LIMITS,
    );
  }
  write<K extends HubCacheSection>(
    section: K,
    data: HubCacheData[K],
    timestamp: number,
    isCurrent: () => boolean = () => true,
  ): Promise<unknown> {
    return this.enqueue(() =>
      isCurrent() ? this.writeRecord(section, data, timestamp) : Promise.resolve(),
    );
  }
  purge(): Promise<void> {
    this.revoked = true;
    return this.namespace
      ? this.enqueue(() => this.cache.purge(this.namespace))
      : Promise.resolve();
  }
  /** Preserve original freshness while removing summaries of nodes no longer registered. */
  retainNodes(ids: Set<string>, isCurrent: () => boolean = () => true): Promise<void> {
    return this.enqueue(async () => {
      if (!isCurrent()) return;
      const attention = await this.readRecord('attention');
      if (attention && isCurrent()) {
        const data = attention.data;
        await this.writeRecord(
          'attention',
          {
            ...data,
            inputRequired: data.inputRequired.filter((item) => ids.has(item.node_id)),
            inbox: data.inbox.filter((item) => ids.has(item.node_id)),
          },
          attention.timestamp,
        );
      }
      const delegations = await this.readRecord('delegations');
      if (delegations && isCurrent())
        await this.writeRecord(
          'delegations',
          delegations.data.filter((item) => ids.has(item.origin_node) && ids.has(item.target_node)),
          delegations.timestamp,
        );
    });
  }
}

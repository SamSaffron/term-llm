import { deletePersistentUICaches } from '../../platform/persistent-cache';
import {
  HubCache,
  cacheNodes,
  cacheAttention,
  cacheDelegations,
  type HubCacheSection,
  type HubCacheData,
} from './hub-cache';
import { reconcileHubItems } from '../domain/reconcile';
import { computed, signal } from '@preact/signals';
import { activeSessionCount as countActiveSessions } from '../domain/formatting';
import { HubAPIError, type HubClient } from '../../api/hub-client';
import { SavedOrder, type OrderRanks } from '../../stores/saved-order';
import type {
  HubCredential,
  HubDelegation,
  HubInputRequiredItem,
  HubAttentionInboxItem,
  HubNode,
  NodeFormData,
  RegistrationInfoResponse,
} from '../domain/types';
import type { PasskeyPlatform } from '../platform/passkeys';

export interface HubStoreOptions {
  cache?: HubCache;
  pollMilliseconds?: number;
  setInterval?: typeof window.setInterval;
  clearInterval?: typeof window.clearInterval;
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Ranks of a committed order's IDs, by position. */
const ranksOf = (ids: readonly string[]): OrderRanks =>
  new Map(ids.map((id, index) => [id, index + 1]));

/** Explains a failed node order save; a missing node means the list changed elsewhere. */
function nodeOrderError(error: unknown): Error {
  return new Error(
    error instanceof HubAPIError && error.status === 404
      ? 'Nodes changed elsewhere; showing the latest order.'
      : `Couldn’t save the node order: ${message(error)}`,
    { cause: error },
  );
}

export class HubStore {
  readonly nodes = signal<HubNode[]>([]);
  readonly delegations = signal<HubDelegation[]>([]);
  readonly inputRequired = signal<HubInputRequiredItem[]>([]);
  readonly inbox = signal<HubAttentionInboxItem[]>([]);
  readonly attentionHasMore = signal(false);
  readonly totalInputRequired = signal(0);
  readonly totalUnseen = signal(0);

  readonly nodesVerified = signal(false);
  readonly attentionVerified = signal(false);
  readonly delegationsVerified = signal(false);
  private readonly cachedSections = signal<HubCacheSection[]>([]);
  readonly lastKnown = computed(() =>
    this.cachedSections.value.some((section) => !this.verified(section).value),
  );
  readonly hasDisplayData = computed(
    () =>
      this.cachedSections.value.length > 0 ||
      this.nodesVerified.value ||
      this.attentionVerified.value ||
      this.delegationsVerified.value,
  );
  readonly initialLoading = signal(true);
  readonly refreshing = signal(false);
  readonly nodeError = signal('');
  readonly resolverWarning = signal('');
  readonly attentionError = signal('');
  readonly clearingAttention = signal(false);
  readonly attentionClearStatus = signal('');
  readonly delegationError = signal('');
  readonly lastNodesRefresh = signal(0);
  readonly lastAttentionRefresh = signal(0);
  readonly lastDelegationsRefresh = signal(0);

  readonly addDialogOpen = signal(false);
  readonly nodeOperation = signal<'idle' | 'testing' | 'adding' | 'removing'>('idle');
  readonly nodeOperationResult = signal('');
  readonly registrationOpen = signal(false);
  readonly registrationLoading = signal(false);
  readonly registrationInfo = signal<RegistrationInfoResponse | null>(null);
  readonly registrationRevealed = signal(false);
  readonly registrationStatus = signal('');
  readonly registrationError = signal('');

  readonly securityOpen = signal(false);
  readonly securityLoading = signal(false);
  readonly securityOperation = signal('');
  readonly securityStatus = signal('');
  readonly credentials = signal<HubCredential[]>([]);
  readonly activeSessions = signal(0);

  readonly reachableCount = computed(() =>
    this.nodesVerified.value ? this.nodes.value.filter((node) => node.status.reachable).length : 0,
  );
  readonly activeSessionCount = computed(() =>
    this.nodesVerified.value ? countActiveSessions(this.nodes.value) : 0,
  );
  readonly activeDelegationCount = computed(() =>
    this.delegationsVerified.value
      ? this.delegations.value.filter(
          (delegation) =>
            !['succeeded', 'failed', 'cancelled', 'timed_out', 'error'].includes(delegation.status),
        ).length
      : 0,
  );

  private readonly pollMilliseconds: number;
  private readonly startInterval: typeof window.setInterval;
  private readonly stopInterval: typeof window.clearInterval;
  private interval: number | undefined;
  private reads: AbortController | undefined;
  private generation = 0;
  private disposed = false;
  private currentRefresh: Promise<void> | null = null;
  private registrationRead: AbortController | undefined;
  private registrationGeneration = 0;
  private securityRead: AbortController | undefined;
  /** The user's node order: shown at once, saved one request at a time. */
  private readonly nodeOrder = new SavedOrder((ranks) => this.applyNodeOrder(ranks));
  /** Counts local order changes; a node list read before the latest one keeps the shown order. */
  private orderVersion = 0;
  /** Node order saves that have not settled. */
  private orderSaves = 0;
  private readonly cache: HubCache;
  private cacheRead: Promise<void> | undefined;
  private cacheRevoked = false;
  private readonly writeTimers = new Map<HubCacheSection, ReturnType<typeof setTimeout>>();
  private readonly unsubscribeUnauthorized: (() => void) | undefined;

  constructor(
    readonly client: HubClient,
    readonly passkeys?: PasskeyPlatform,
    options: HubStoreOptions = {},
  ) {
    this.cache = options.cache ?? new HubCache(client.config ?? { basePath: '' });
    this.unsubscribeUnauthorized = client.onUnauthorized?.(() => this.invalidateCache());
    this.pollMilliseconds = options.pollMilliseconds ?? 15_000;
    this.startInterval = options.setInterval ?? window.setInterval.bind(window);
    this.stopInterval = options.clearInterval ?? window.clearInterval.bind(window);
  }

  start(): void {
    if (this.disposed || this.interval !== undefined) return;
    if (this.client.config?.cacheDisplayAllowed) void this.hydrateCache();
    void this.refresh('initial');
    this.interval = this.startInterval(() => void this.refresh('poll'), this.pollMilliseconds);
  }

  refresh(kind: 'initial' | 'manual' | 'poll' = 'manual'): Promise<void> {
    if (this.disposed || this.cacheRevoked) return Promise.resolve();
    if (kind === 'poll' && this.currentRefresh) return this.currentRefresh;
    this.reads?.abort();
    const controller = new AbortController();
    this.reads = controller;
    const generation = ++this.generation;
    if (kind === 'manual') this.refreshing.value = true;

    const run = this.runRefresh(controller.signal, generation).finally(() => {
      if (this.currentRefresh === run) this.currentRefresh = null;
      if (this.reads === controller) this.reads = undefined;
      if (generation === this.generation) {
        this.refreshing.value = false;
        this.initialLoading.value = false;
      }
    });
    this.currentRefresh = run;
    return run;
  }

  async clearAttention(): Promise<void> {
    if (
      this.disposed ||
      this.cacheRevoked ||
      this.clearingAttention.peek() ||
      !this.attentionVerified.peek()
    )
      return;
    this.clearingAttention.value = true;
    this.attentionClearStatus.value = '';
    try {
      const result = await this.client.clearAttention();
      if (this.disposed || this.cacheRevoked) return;
      if (result.failed > 0) {
        this.attentionClearStatus.value = `Cleared ${result.cleared}; ${result.failed} could not be cleared. Try Clear all again.`;
      }
    } catch (error) {
      if (this.disposed || this.cacheRevoked) return;
      if (error instanceof HubAPIError && [401, 403].includes(error.status)) {
        await this.invalidateCache();
        return;
      }
      this.attentionClearStatus.value = `Could not clear notifications: ${message(error)}. Try Clear all again.`;
    } finally {
      if (!this.disposed && !this.cacheRevoked) {
        // Reconcile even after an uncertain network failure. A new generation
        // rejects pre-mutation polls; only the server decides which rows remain.
        try {
          await this.refresh();
        } finally {
          if (!this.disposed && !this.cacheRevoked) this.clearingAttention.value = false;
        }
      }
    }
  }

  private verified(section: HubCacheSection) {
    return section === 'nodes'
      ? this.nodesVerified
      : section === 'attention'
        ? this.attentionVerified
        : this.delegationsVerified;
  }

  private isCurrent(signal: AbortSignal, generation: number): boolean {
    return (
      !this.disposed && !this.cacheRevoked && !signal.aborted && generation === this.generation
    );
  }

  private async runRefresh(signal: AbortSignal, generation: number): Promise<void> {
    const orderVersion = this.orderVersion;
    const settle = async <T>(
      section: HubCacheSection,
      request: Promise<T>,
      apply: (data: T) => void,
    ) => {
      try {
        const data = await request;
        if (!this.isCurrent(signal, generation)) return;
        apply(data);
        this.verified(section).value = true;
        // A shell without the explicit authorization guarantee must wait for
        // an authenticated response before showing any private cached section.
        void this.hydrateCache();
        this.scheduleWrite(section, generation);
      } catch (error) {
        if (!this.isCurrent(signal, generation)) return;
        if (error instanceof HubAPIError && [401, 403].includes(error.status)) {
          await this.invalidateCache();
          return;
        }
        if (error instanceof Error && error.name === 'AbortError') return;
        const target =
          section === 'nodes'
            ? this.nodeError
            : section === 'attention'
              ? this.attentionError
              : this.delegationError;
        target.value =
          section === 'nodes' ? `Failed to load nodes: ${message(error)}` : message(error);
      }
    };
    await Promise.allSettled([
      settle('nodes', this.client.listNodes(signal), (data) =>
        this.acceptNodes(data.nodes ?? [], data.resolver_error ?? '', orderVersion, generation),
      ),
      settle('attention', this.client.listAttention(signal), (data) => {
        this.inputRequired.value = reconcileHubItems(
          this.inputRequired.peek(),
          data.input_required ?? [],
          (item) => JSON.stringify([item.node_id, item.session_id]),
        );
        this.inbox.value = reconcileHubItems(this.inbox.peek(), data.inbox ?? [], (item) =>
          JSON.stringify([item.node_id, item.session_id]),
        );
        this.totalInputRequired.value =
          data.total_input_required ?? this.inputRequired.value.length;
        this.totalUnseen.value = data.total_unseen ?? this.inbox.value.length;
        this.attentionHasMore.value = Boolean(data.has_more);
        this.attentionError.value = '';
        this.lastAttentionRefresh.value = Date.now();
      }),
      settle('delegations', this.client.listDelegations(signal), (data) => {
        this.delegations.value = reconcileHubItems(
          this.delegations.peek(),
          data.delegations ?? [],
          (item) => item.id,
        );
        this.delegationError.value = '';
        this.lastDelegationsRefresh.value = Date.now();
      }),
    ]);
  }

  private acceptNodes(
    nodes: HubNode[],
    warning: string,
    orderVersion: number,
    generation: number,
  ): void {
    this.showNodes(nodes, orderVersion);
    this.nodesVerified.value = true;
    this.resolverWarning.value = warning;
    this.nodeError.value = '';
    this.lastNodesRefresh.value = Date.now();
    const ids = new Set(nodes.map((node) => node.id));
    // Only cached/unverified summaries are pruned from the display. Verified
    // attention remains durable across transient resolver failures.
    if (!this.attentionVerified.peek()) {
      this.inputRequired.value = this.inputRequired.peek().filter((item) => ids.has(item.node_id));
      this.inbox.value = this.inbox.peek().filter((item) => ids.has(item.node_id));
    }
    if (!this.delegationsVerified.peek())
      this.delegations.value = this.delegations
        .peek()
        .filter((item) => ids.has(item.origin_node) && ids.has(item.target_node));
    void this.cache.retainNodes(
      ids,
      () => !this.disposed && !this.cacheRevoked && generation === this.generation,
    );
  }

  private hydrateCache(): Promise<void> {
    if (this.cacheRead) return this.cacheRead;
    this.cacheRead = this.readCache();
    return this.cacheRead;
  }

  private async readCache(): Promise<void> {
    for (const section of ['nodes', 'attention', 'delegations'] as const) {
      const row = await this.cache.read(section);
      if (!row || this.disposed || this.cacheRevoked || this.verified(section).peek()) continue;
      if (section === 'nodes') {
        // Local order edits made while IndexedDB opened must survive hydration.
        if (this.orderVersion > 0) continue;
        this.nodes.value = cacheNodes(row.data as HubCacheData['nodes']);
        this.lastNodesRefresh.value = row.timestamp;
      } else if (section === 'attention') {
        const data = cacheAttention(row.data as HubCacheData['attention']);
        const keep = (id: string) =>
          !this.nodesVerified.peek() || this.nodes.peek().some((node) => node.id === id);
        this.inputRequired.value = data.inputRequired.filter((item) => keep(item.node_id));
        this.inbox.value = data.inbox.filter((item) => keep(item.node_id));
        this.totalInputRequired.value = data.totalInputRequired;
        this.totalUnseen.value = data.totalUnseen;
        this.attentionHasMore.value = data.hasMore;
        this.lastAttentionRefresh.value = row.timestamp;
      } else {
        this.delegations.value = cacheDelegations(row.data as HubCacheData['delegations']).filter(
          (item) =>
            !this.nodesVerified.peek() ||
            (this.nodes.peek().some((node) => node.id === item.origin_node) &&
              this.nodes.peek().some((node) => node.id === item.target_node)),
        );
        this.lastDelegationsRefresh.value = row.timestamp;
      }
      this.cachedSections.value = [...this.cachedSections.peek(), section];
    }
  }

  private scheduleWrite(section: HubCacheSection, generation = this.generation): void {
    clearTimeout(this.writeTimers.get(section));
    if (!this.cache.namespace) return;
    this.writeTimers.set(
      section,
      setTimeout(() => {
        this.writeTimers.delete(section);
        const current = () =>
          !this.disposed && !this.cacheRevoked && generation === this.generation;
        if (!current() || !this.verified(section).peek()) return;
        const ids = new Set(this.nodes.peek().map((node) => node.id));
        const keep = (id: string) => !this.nodesVerified.peek() || ids.has(id);
        if (section === 'nodes') {
          void this.cache.write(
            section,
            cacheNodes(this.nodes.peek()),
            this.lastNodesRefresh.peek(),
            current,
          );
        } else if (section === 'attention') {
          void this.cache.write(
            section,
            cacheAttention({
              inputRequired: this.inputRequired.peek().filter((item) => keep(item.node_id)),
              inbox: this.inbox.peek().filter((item) => keep(item.node_id)),
              totalInputRequired: this.totalInputRequired.peek(),
              totalUnseen: this.totalUnseen.peek(),
              hasMore: this.attentionHasMore.peek(),
            }),
            this.lastAttentionRefresh.peek(),
            current,
          );
        } else {
          void this.cache.write(
            section,
            cacheDelegations(
              this.delegations
                .peek()
                .filter((item) => keep(item.origin_node) && keep(item.target_node)),
            ),
            this.lastDelegationsRefresh.peek(),
            current,
          );
        }
      }, 200),
    );
  }

  private async invalidateCache(): Promise<void> {
    this.cacheRevoked = true;
    this.generation++;
    this.reads?.abort();
    this.writeTimers.forEach(clearTimeout);
    this.writeTimers.clear();
    this.nodes.value = [];
    this.inputRequired.value = [];
    this.inbox.value = [];
    this.delegations.value = [];
    this.totalInputRequired.value = 0;
    this.totalUnseen.value = 0;
    this.nodesVerified.value = false;
    this.attentionVerified.value = false;
    this.clearingAttention.value = false;
    this.attentionClearStatus.value = '';
    this.delegationsVerified.value = false;
    this.cachedSections.value = [];
    this.initialLoading.value = false;
    this.refreshing.value = false;
    this.nodeError.value = 'Hub authorization expired. Sign in again.';
    await this.cache.purge();
  }

  openAddDialog(): void {
    this.addDialogOpen.value = true;
    this.nodeOperationResult.value = '';
  }

  closeAddDialog(): void {
    this.addDialogOpen.value = false;
    this.closeRegistrationHelp();
  }

  async testNode(value: NodeFormData): Promise<void> {
    if (this.nodeOperation.value !== 'idle') return;
    this.nodeOperation.value = 'testing';
    this.nodeOperationResult.value = 'Testing…';
    try {
      const response = await this.client.testNode(value);
      if (this.disposed) return;
      const status = response.status;
      this.nodeOperationResult.value = status.reachable
        ? `✓ Reachable in ${status.latency_ms} ms${status.agent ? ` · agent: ${status.agent}` : ''}${status.version ? ` · ${status.version}` : ''}`
        : `✗ Not reachable: ${status.error || status.state}`;
    } catch (error) {
      if (!this.disposed) this.nodeOperationResult.value = `✗ ${message(error)}`;
    } finally {
      if (!this.disposed) this.nodeOperation.value = 'idle';
    }
  }

  async addNode(value: NodeFormData): Promise<{ clean: boolean }> {
    if (this.nodeOperation.value !== 'idle') return { clean: false };
    this.nodeOperation.value = 'adding';
    this.nodeOperationResult.value = 'Adding…';
    let warning: string;
    try {
      const response = await this.client.addNode(value);
      warning = response.warning ?? '';
    } catch (error) {
      if (!this.disposed) {
        this.nodeOperationResult.value = `✗ ${message(error)}`;
        this.nodeOperation.value = 'idle';
      }
      return { clean: false };
    }
    if (this.disposed) return { clean: !warning };
    if (warning) {
      this.nodeOperationResult.value = `Added with warning: ${warning}`;
    } else {
      this.nodeOperationResult.value = '';
      this.closeAddDialog();
    }
    try {
      await this.refreshNodes();
    } catch (error) {
      this.nodeError.value = `Node was added, but the list could not refresh: ${message(error)}`;
    } finally {
      this.nodeOperation.value = 'idle';
    }
    return { clean: !warning };
  }

  async removeNode(id: string): Promise<void> {
    if (this.nodeOperation.value !== 'idle') return;
    this.nodeOperation.value = 'removing';
    try {
      await this.client.removeNode(id);
    } catch (error) {
      if (!this.disposed) {
        this.nodeError.value = `Could not remove node: ${message(error)}`;
        this.nodeOperation.value = 'idle';
      }
      return;
    }
    if (this.disposed) return;
    try {
      await this.refreshNodes();
    } catch (error) {
      this.nodeError.value = `Node was removed, but the list could not refresh: ${message(error)}`;
    } finally {
      this.nodeOperation.value = 'idle';
    }
  }

  private async refreshNodes(): Promise<void> {
    if (this.disposed || this.cacheRevoked) return;
    this.reads?.abort();
    const controller = new AbortController();
    const generation = ++this.generation;
    const orderVersion = this.orderVersion;
    this.reads = controller;
    try {
      const response = await this.client.listNodes(controller.signal);
      if (this.disposed || controller.signal.aborted || generation !== this.generation) return;
      this.acceptNodes(
        response.nodes ?? [],
        response.resolver_error ?? '',
        orderVersion,
        generation,
      );
      this.scheduleWrite('nodes', generation);
      this.initialLoading.value = false;
    } catch (error) {
      if (this.disposed || controller.signal.aborted || generation !== this.generation) return;
      this.initialLoading.value = false;
      if (error instanceof HubAPIError && [401, 403].includes(error.status))
        await this.invalidateCache();
      throw error;
    } finally {
      if (this.reads === controller) this.reads = undefined;
      // This read can replace an in-flight manual refresh. Its old finally
      // block will skip cleanup after our generation bump.
      if (generation === this.generation) this.refreshing.value = false;
    }
  }

  /**
   * Shows a node list the server sent, read when the local order was at
   * `orderVersion`. The Hub lists nodes in their saved order, but a list read
   * before the latest local reorder, or while one is saving, may predate it:
   * such a list keeps the order shown now, with nodes new to it at the end.
   */
  private showNodes(nodes: HubNode[], orderVersion: number): void {
    let listed = nodes;
    if (orderVersion !== this.orderVersion || this.orderSaves > 0) {
      const shown = new Map(this.nodes.peek().map((node, index) => [node.id, index]));
      const place = (node: HubNode) => shown.get(node.id) ?? shown.size;
      listed = [...nodes].sort((left, right) => place(left) - place(right));
    }
    this.nodes.value = reconcileHubItems(this.nodes.peek(), listed, (node) => node.id);
  }

  /** Shows nodes in `ranks` order; nodes it does not rank keep their order after the others. */
  private applyNodeOrder(ranks: OrderRanks): void {
    this.orderVersion++;
    const rank = (node: HubNode) => ranks.get(node.id) ?? Number.MAX_SAFE_INTEGER;
    const shown = this.nodes.peek();
    const next = [...shown].sort((left, right) => rank(left) - rank(right));
    if (next.some((node, index) => node !== shown[index])) this.nodes.value = next;
  }

  /**
   * Saves a new node order. The listed nodes move, in order, into the
   * positions they already occupy, so nodes that are not listed keep theirs.
   * The new order shows at once; saves run one at a time, and a newer order
   * replaces one not yet sent. If the newest save fails, the previous order
   * returns, the list refreshes, and the error is thrown for the caller to
   * report.
   */
  async reorderNodes(orderedIds: string[]): Promise<void> {
    if (this.disposed) return;
    this.orderSaves++;
    let failure: { error: unknown } | null = null;
    // The server permutes only the listed nodes. Send the smallest span that
    // changed, so a move in one part of the grid cannot undo an independent
    // reorder elsewhere (or fail because an unrelated node vanished).
    const current = this.nodes.peek().map((node) => node.id);
    let first = 0;
    while (first < current.length && current[first] === orderedIds[first]) first++;
    let last = current.length - 1;
    while (last > first && current[last] === orderedIds[last]) last--;
    const moved = orderedIds.slice(first, last + 1);
    try {
      await this.nodeOrder.save(
        current.map((id, index) => ({ id, rank: index + 1 })),
        moved,
        async (listed) => ranksOf((await this.client.reorderNodes(listed)).node_ids ?? []),
      );
    } catch (error) {
      failure = { error };
    } finally {
      this.orderSaves--;
    }
    if (!failure) {
      this.scheduleWrite('nodes');
      return;
    }
    // The restored order may be stale too: show the Hub's order.
    await this.refreshNodes().catch(() => undefined);
    throw nodeOrderError(failure.error);
  }

  /** Shows why a node order could not be saved until the next successful refresh. */
  reportNodeOrderError(error: unknown): void {
    if (!this.disposed) this.nodeError.value = message(error);
  }

  async openRegistrationHelp(): Promise<void> {
    this.registrationOpen.value = true;
    if (this.registrationInfo.value || this.registrationLoading.value) return;
    this.registrationRead?.abort();
    const controller = new AbortController();
    const generation = ++this.registrationGeneration;
    this.registrationRead = controller;
    this.registrationLoading.value = true;
    this.registrationStatus.value = '';
    this.registrationError.value = '';
    try {
      const info = await this.client.registrationInfo(controller.signal);
      if (this.registrationOpen.value && generation === this.registrationGeneration) {
        this.registrationInfo.value = info;
      }
    } catch (error) {
      if (controller.signal.aborted || generation !== this.registrationGeneration) return;
      this.registrationError.value = `Could not load registration settings: ${message(error)}`;
    } finally {
      if (generation === this.registrationGeneration) {
        this.registrationLoading.value = false;
        this.registrationRead = undefined;
      }
    }
  }

  closeRegistrationHelp(): void {
    this.registrationGeneration++;
    this.registrationRead?.abort();
    this.registrationRead = undefined;
    this.registrationOpen.value = false;
    this.registrationLoading.value = false;
    this.registrationInfo.value = null;
    this.registrationRevealed.value = false;
    this.registrationStatus.value = '';
    this.registrationError.value = '';
  }

  async openSecurity(): Promise<void> {
    if (this.securityOpen.value) return;
    this.securityOpen.value = true;
    await this.loadSecurity();
  }

  closeSecurity(): void {
    this.securityOpen.value = false;
    this.securityRead?.abort();
    this.securityRead = undefined;
    this.securityLoading.value = false;
  }

  async toggleSecurity(): Promise<void> {
    if (this.securityOpen.value) {
      this.closeSecurity();
    } else {
      await this.openSecurity();
    }
  }

  async loadSecurity(): Promise<void> {
    if (this.securityLoading.value || this.disposed) return;
    const controller = new AbortController();
    this.securityRead = controller;
    this.securityLoading.value = true;
    try {
      const [credentials, session] = await Promise.all([
        this.client.listCredentials(controller.signal),
        this.client.session(controller.signal),
      ]);
      if (this.disposed || controller.signal.aborted || this.securityRead !== controller) return;
      this.credentials.value = credentials.credentials ?? [];
      this.activeSessions.value = session.active_sessions ?? 0;
    } catch (error) {
      if (!this.disposed && !controller.signal.aborted && this.securityRead === controller) {
        this.securityStatus.value = message(error);
      }
    } finally {
      if (this.securityRead === controller) {
        this.securityRead = undefined;
        if (!this.disposed) this.securityLoading.value = false;
      }
    }
  }

  async renameCredential(recordID: string, displayName: string): Promise<void> {
    await this.securityAction('rename', async () => {
      await this.client.renameCredential(recordID, displayName);
      return 'Passkey renamed.';
    });
  }

  private async reauthenticate(): Promise<void> {
    if (!this.passkeys) throw new Error('Passkeys are unavailable.');
    const options = await this.client.beginReauthentication();
    const credential = await this.passkeys.get(options);
    await this.client.finishReauthentication(credential);
  }

  async removeCredential(recordID: string): Promise<void> {
    await this.securityAction('remove', async () => {
      this.securityStatus.value = 'Confirm with a passkey…';
      await this.reauthenticate();
      await this.client.removeCredential(recordID);
      return 'Passkey removed.';
    });
  }

  async addPasskey(displayName: string): Promise<void> {
    await this.securityAction('add', async () => {
      this.securityStatus.value = 'Confirm with an existing passkey…';
      await this.reauthenticate();
      if (this.disposed) return '';
      this.securityStatus.value = 'Create the new passkey…';
      if (!this.passkeys) throw new Error('Passkeys are unavailable.');
      const options = await this.client.beginAdditionalRegistration(displayName);
      const credential = await this.passkeys.create(options);
      await this.client.finishAdditionalRegistration(credential);
      return 'Passkey added.';
    });
  }

  async revokeOtherSessions(): Promise<void> {
    await this.securityAction('revoke', async () => {
      const result = await this.client.revokeOtherSessions();
      return `Revoked ${result.revoked} other session${result.revoked === 1 ? '' : 's'}.`;
    });
  }

  async signOut(): Promise<string | null> {
    if (this.securityOperation.value || this.disposed) return null;
    this.securityOperation.value = 'logout';
    try {
      const result = await this.client.logout();
      await this.invalidateCache();
      await deletePersistentUICaches();
      return result.redirect;
    } catch (error) {
      if (!this.disposed) this.securityStatus.value = message(error);
      return null;
    } finally {
      if (!this.disposed) this.securityOperation.value = '';
    }
  }

  private async securityAction(name: string, action: () => Promise<string>): Promise<void> {
    if (this.securityOperation.value || this.disposed) return;
    this.securityOperation.value = name;
    try {
      const status = await action();
      if (this.disposed) return;
      this.securityStatus.value = status;
      await this.loadSecurity();
    } catch (error) {
      if (!this.disposed) this.securityStatus.value = message(error);
    } finally {
      if (!this.disposed) this.securityOperation.value = '';
    }
  }

  dispose(): void {
    this.disposed = true;
    this.unsubscribeUnauthorized?.();
    this.writeTimers.forEach(clearTimeout);
    this.writeTimers.clear();
    this.generation++;
    this.reads?.abort();
    this.reads = undefined;
    this.closeSecurity();
    if (this.interval !== undefined) this.stopInterval(this.interval);
    this.interval = undefined;
    this.closeAddDialog();
  }
}

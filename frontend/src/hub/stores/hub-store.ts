import { reconcileHubItems } from '../domain/reconcile';
import { computed, signal } from '@preact/signals';
import { activeSessionCount as countActiveSessions } from '../domain/formatting';
import type { HubClient } from '../../api/hub-client';
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
  pollMilliseconds?: number;
  setInterval?: typeof window.setInterval;
  clearInterval?: typeof window.clearInterval;
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export class HubStore {
  readonly nodes = signal<HubNode[]>([]);
  readonly delegations = signal<HubDelegation[]>([]);
  readonly inputRequired = signal<HubInputRequiredItem[]>([]);
  readonly inbox = signal<HubAttentionInboxItem[]>([]);
  readonly attentionHasMore = signal(false);
  readonly totalInputRequired = signal(0);
  readonly totalUnseen = signal(0);

  readonly initialLoading = signal(true);
  readonly refreshing = signal(false);
  readonly nodeError = signal('');
  readonly resolverWarning = signal('');
  readonly attentionError = signal('');
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

  readonly reachableCount = computed(
    () => this.nodes.value.filter((node) => node.status.reachable).length,
  );
  readonly activeSessionCount = computed(() => countActiveSessions(this.nodes.value));
  readonly activeDelegationCount = computed(
    () =>
      this.delegations.value.filter(
        (delegation) =>
          !['succeeded', 'failed', 'cancelled', 'timed_out', 'error'].includes(delegation.status),
      ).length,
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

  constructor(
    readonly client: HubClient,
    readonly passkeys?: PasskeyPlatform,
    options: HubStoreOptions = {},
  ) {
    this.pollMilliseconds = options.pollMilliseconds ?? 15_000;
    this.startInterval = options.setInterval ?? window.setInterval.bind(window);
    this.stopInterval = options.clearInterval ?? window.clearInterval.bind(window);
  }

  start(): void {
    if (this.disposed || this.interval !== undefined) return;
    void this.refresh('initial');
    this.interval = this.startInterval(() => void this.refresh('poll'), this.pollMilliseconds);
  }

  refresh(kind: 'initial' | 'manual' | 'poll' = 'manual'): Promise<void> {
    if (this.disposed) return Promise.resolve();
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

  private async runRefresh(signal: AbortSignal, generation: number): Promise<void> {
    const [nodes, attention, delegations] = await Promise.allSettled([
      this.client.listNodes(signal),
      this.client.listAttention(signal),
      this.client.listDelegations(signal),
    ]);
    if (this.disposed || signal.aborted || generation !== this.generation) return;
    const now = Date.now();
    if (nodes.status === 'fulfilled') {
      this.nodes.value = reconcileHubItems(
        this.nodes.peek(),
        nodes.value.nodes ?? [],
        (node) => node.id,
      );
      this.resolverWarning.value = nodes.value.resolver_error ?? '';
      this.nodeError.value = '';
      this.lastNodesRefresh.value = now;
    } else if (nodes.reason?.name !== 'AbortError') {
      this.nodeError.value = `Failed to load nodes: ${message(nodes.reason)}`;
    }
    if (attention.status === 'fulfilled') {
      this.inputRequired.value = reconcileHubItems(
        this.inputRequired.peek(),
        attention.value.input_required ?? [],
        (item) => JSON.stringify([item.node_id, item.session_id]),
      );
      this.inbox.value = reconcileHubItems(this.inbox.peek(), attention.value.inbox ?? [], (item) =>
        JSON.stringify([item.node_id, item.session_id]),
      );
      this.totalInputRequired.value =
        attention.value.total_input_required ?? this.inputRequired.value.length;
      this.totalUnseen.value = attention.value.total_unseen ?? this.inbox.value.length;
      this.attentionHasMore.value = Boolean(attention.value.has_more);
      this.attentionError.value = '';
      this.lastAttentionRefresh.value = now;
    } else if (attention.reason?.name !== 'AbortError') {
      this.attentionError.value = message(attention.reason);
    }
    if (delegations.status === 'fulfilled') {
      this.delegations.value = reconcileHubItems(
        this.delegations.peek(),
        delegations.value.delegations ?? [],
        (item) => item.id,
      );
      this.delegationError.value = '';
      this.lastDelegationsRefresh.value = now;
    } else if (delegations.reason?.name !== 'AbortError') {
      this.delegationError.value = message(delegations.reason);
    }
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
    if (this.disposed) return;
    this.reads?.abort();
    const controller = new AbortController();
    const generation = ++this.generation;
    this.reads = controller;
    try {
      const response = await this.client.listNodes(controller.signal);
      if (this.disposed || controller.signal.aborted || generation !== this.generation) return;
      this.nodes.value = response.nodes ?? [];
      this.resolverWarning.value = response.resolver_error ?? '';
      this.nodeError.value = '';
      this.lastNodesRefresh.value = Date.now();
      this.initialLoading.value = false;
    } catch (error) {
      if (this.disposed || controller.signal.aborted || generation !== this.generation) return;
      this.initialLoading.value = false;
      throw error;
    } finally {
      if (this.reads === controller) this.reads = undefined;
    }
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
      return (await this.client.logout()).redirect;
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
    this.generation++;
    this.reads?.abort();
    this.reads = undefined;
    this.closeSecurity();
    if (this.interval !== undefined) this.stopInterval(this.interval);
    this.interval = undefined;
    this.closeAddDialog();
  }
}

import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { Attachment, Message, Session } from '../domain/types';
import type { Modal } from './store-types';
import { uuid } from './store-utils';
import type { AppStoreServices } from './app-store-services';

export interface BranchStoreOptions {
  activeSession: ReadonlySignal<Session | null>;
  activeSessionId: ReadonlySignal<string>;
  draftActive: ReadonlySignal<boolean>;
  attachments: ReadonlySignal<Attachment[]>;
  visibleMessages: ReadonlySignal<Message[]>;
  prompt: Signal<string>;
  modal: Signal<Modal>;
  refreshSidebar: () => Promise<void>;
  publishSessionChange: () => void;
  findSession: (id: string) => Session | undefined;
  createSession: (value: Record<string, unknown>) => Session;
  prependSession: (session: Session) => void;
  selectSession: (session: Session) => Promise<void>;
  send: () => Promise<void>;
}

/** Owns branch tree presentation and branch/fork creation commands. */
export class BranchStore {
  readonly tree = signal<Record<string, unknown> | null>(null);
  readonly pathCount = signal(0);
  readonly target = signal('');
  readonly prefill = signal('');

  constructor(
    private readonly services: AppStoreServices,
    private readonly options: BranchStoreOptions,
  ) {}

  async refresh(
    sessionId = this.options.activeSessionId.peek(),
    includeBranchPoints = false,
  ): Promise<Record<string, unknown> | null> {
    if (!sessionId) {
      this.tree.value = null;
      this.pathCount.value = 0;
      return null;
    }
    try {
      const tree = includeBranchPoints
        ? await this.services.endpoints.tree(sessionId, undefined, true)
        : await this.services.endpoints.tree(sessionId);
      if (this.options.activeSessionId.peek() !== sessionId) return null;
      this.tree.value = tree;
      this.pathCount.value = Math.max(1, Number(tree.path_count) || 1);
      return tree;
    } catch {
      if (this.options.activeSessionId.peek() === sessionId) {
        this.tree.value = null;
        this.pathCount.value = 0;
      }
      return null;
    }
  }

  async load(): Promise<void> {
    const sessionId = this.options.activeSessionId.peek();
    const tree = await this.refresh(sessionId, true);
    if (tree) this.options.modal.value = 'branch';
  }

  async command(kind: 'fork' | 'thread', message = ''): Promise<void> {
    const session = this.options.activeSession.value;
    if (!session || this.options.draftActive.value) {
      this.services.toast('Start the conversation before creating a thread or fork.', 'error');
      return;
    }
    if (this.options.attachments.value.length) {
      this.services.toast('Create the thread or fork before attaching files or images.', 'error');
      return;
    }
    const anchor =
      kind === 'thread'
        ? 0
        : [...this.options.visibleMessages.value]
            .reverse()
            .find((entry) => Number(entry.durableRowId) > 0)?.durableRowId || 0;
    const original = this.options.prompt.value;
    this.options.prompt.value = '';
    try {
      await this.branchFrom(String(anchor), 'clean', '', message.trim());
    } catch (error) {
      if (!this.options.prompt.value) this.options.prompt.value = original;
      this.services.toast(error, 'error');
    }
  }

  openContext(messageId: string, prefill = ''): void {
    this.target.value = messageId;
    this.prefill.value = prefill;
    this.options.modal.value = 'branch-context';
  }

  async branchFrom(
    messageId: string,
    context: string,
    focus = '',
    autoSend = '',
    prefill = '',
  ): Promise<void> {
    const session = this.options.activeSession.value;
    if (!session) return;
    const data = await this.services.endpoints.branch(session.id, {
      anchor_message_id: Number(messageId) || 0,
      expected_rev: session.transcriptRev || 0,
      idempotency_key: uuid(),
    });
    const child =
      data.session && typeof data.session === 'object'
        ? (data.session as Record<string, unknown>)
        : data;
    const id = String(child.id || data.session_id || '');
    if (id && (context === 'notes' || context === 'focused'))
      await this.services.endpoints.pathNotes(id, {
        mode: context,
        ...(context === 'focused' ? { focus } : {}),
      });
    await this.options.refreshSidebar();
    this.options.publishSessionChange();
    let target = this.options.findSession(id);
    if (!target && id) {
      target = this.options.createSession({ ...child, id });
      this.options.prependSession(target);
    }
    if (target) {
      await this.options.selectSession(target);
      if (autoSend) {
        this.options.prompt.value = autoSend;
        await this.options.send();
      } else if (prefill) {
        this.options.prompt.value = prefill;
      }
    }
    this.target.value = '';
    this.prefill.value = '';
    this.options.modal.value = '';
  }
}

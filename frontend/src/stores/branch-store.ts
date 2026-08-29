import { signal, type ReadonlySignal, type Signal } from '@preact/signals';
import type { Attachment, Message, Session } from '../domain/types';
import { errorMessage } from '../domain/text';
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
  readonly busy = signal(false);
  readonly error = signal('');

  private retryOperation: { signature: string; idempotencyKey: string } | null = null;

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
    if (this.busy.peek()) {
      this.services.toast('A conversation path is already being created.', 'info');
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
    const created = await this.branchFrom(String(anchor), 'clean', '', message.trim());
    if (!created && !this.options.prompt.value) this.options.prompt.value = original;
  }

  openContext(messageId: string, prefill = ''): void {
    this.target.value = messageId;
    this.prefill.value = prefill;
    this.error.value = '';
    this.options.modal.value = 'branch-context';
  }

  async branchFrom(
    messageId: string,
    context: string,
    focus = '',
    autoSend = '',
    prefill = '',
  ): Promise<boolean> {
    if (this.busy.peek()) return false;
    const session = this.options.activeSession.value;
    if (!session) return false;

    const anchor = Number(messageId) || 0;
    const signature = `${session.id}\u0000${anchor}`;
    if (!this.retryOperation || this.retryOperation.signature !== signature)
      this.retryOperation = { signature, idempotencyKey: uuid() };
    const operation = this.retryOperation;
    const modalTarget = this.target.peek();
    const ownsBranchModal =
      this.options.modal.peek() === 'branch-context' && modalTarget === messageId;
    const finishBranchModal = () => {
      if (
        ownsBranchModal &&
        this.options.modal.peek() === 'branch-context' &&
        this.target.peek() === modalTarget
      ) {
        this.target.value = '';
        this.prefill.value = '';
        this.options.modal.value = '';
      }
    };
    let createdID = '';
    this.busy.value = true;
    this.error.value = '';
    try {
      const data = await this.services.endpoints.branch(session.id, {
        anchor_message_id: anchor,
        idempotency_key: operation.idempotencyKey,
      });
      const child =
        data.session && typeof data.session === 'object'
          ? (data.session as Record<string, unknown>)
          : data;
      const id = String(child.id || data.session_id || '').trim();
      if (!id) throw new Error('Branch response did not identify the new conversation path.');

      createdID = id;
      if (this.retryOperation === operation) this.retryOperation = null;
      let target = this.options.findSession(id);
      if (!target) {
        target = this.options.createSession({ ...child, id });
        this.options.prependSession(target);
      }
      try {
        await this.options.refreshSidebar();
      } catch (error) {
        this.services.toast(
          `New path created, but the sidebar could not refresh: ${errorMessage(error)}`,
          'error',
        );
      }
      target = this.options.findSession(id) || target;
      if (!this.options.findSession(id)) this.options.prependSession(target);
      this.options.publishSessionChange();

      if (context === 'notes' || context === 'focused') {
        try {
          const result = await this.services.endpoints.pathNotes(id, {
            mode: context,
            ...(context === 'focused' ? { focus } : {}),
          });
          if (result.limited)
            this.services.toast(
              String(
                result.message ||
                  'Some source output was omitted from the new path context because it was not available yet.',
              ),
              'info',
            );
        } catch (error) {
          this.services.toast(
            `New path created, but its additional context could not be prepared: ${errorMessage(error)}`,
            'error',
          );
        }
      }
      let selected = false;
      try {
        await this.options.selectSession(target);
        selected = true;
      } catch (error) {
        this.services.toast(
          `New path created, but it could not be opened: ${errorMessage(error)}`,
          'error',
        );
      }
      if (selected && autoSend) {
        this.options.prompt.value = autoSend;
        try {
          await this.options.send();
        } catch (error) {
          this.services.toast(
            `New path created, but its first message could not be sent: ${errorMessage(error)}`,
            'error',
          );
        }
      } else if (selected && prefill) {
        this.options.prompt.value = prefill;
      }
      finishBranchModal();
      return true;
    } catch (error) {
      const detail = errorMessage(error);
      if (createdID) {
        this.services.toast(
          `New path created, but the browser could not finish opening it: ${detail}`,
          'error',
        );
        finishBranchModal();
        return true;
      }
      const message = detail || 'Could not create conversation path.';
      this.error.value = message;
      this.services.toast(message, 'error');
      return false;
    } finally {
      this.busy.value = false;
    }
  }
}

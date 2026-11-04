import type { ReadonlySignal, Signal } from '@preact/signals';
import type { DiffComment, DiffFile, Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import type { DiffState, SendOptions } from './store-types';

/**
 * Host capabilities the review store borrows from AppStore. Kept in a
 * type-only module so neither side of the lazy review boundary gains a runtime
 * import from the other.
 */
export interface ReviewStoreHost {
  activeSession: ReadonlySignal<Session | null>;
  activeSessionId: ReadonlySignal<string>;
  streaming: ReadonlySignal<boolean>;
  closePlan: () => void;
  restartStatusPoll: () => void;
  send: (options?: SendOptions) => Promise<void>;
  steer: (content: string, options?: SendOptions) => Promise<void>;
  publishSessionChange: (type: 'review-comment-changed', sessionId: string) => void;
}

/**
 * Small generation counters owned by the eager store. Deferred operations read
 * and advance these so a superseded load, expansion, session switch, or
 * disposal can be detected after the lazy import and after every await.
 */
export interface ReviewOperationState {
  loadEpoch: number;
  readonly expandEpoch: Map<string, number>;
}

/**
 * The eager review surface handed to the lazily imported operations module.
 * Everything here is either a signal, a service, or a callback back into the
 * small eager layer.
 */
export interface ReviewOperationContext {
  services: AppStoreServices;
  host: ReviewStoreHost;
  diff: Signal<DiffState>;
  state: ReviewOperationState;
  refreshDiffComments: (sessionId: string) => Promise<void>;
  loadMarkdownPreview: (file: DiffFile, force?: boolean) => Promise<void>;
}

/** Structural surface of the lazily imported review operations implementation. */
export interface ReviewOperationsApi {
  loadDiff(owner: string, scope: string, epoch: number): Promise<void>;
  openWorktreeDiff(owner: string, dir: string, title: string, epoch: number): Promise<void>;
  expandDiff(
    file: DiffFile,
    owner: string,
    scope: string,
    context?: number,
    refresh?: boolean,
  ): Promise<void>;
  sendDiffComments(owner: string): Promise<void>;
  sendDiffComment(owner: string, comment: DiffComment): Promise<void>;
}

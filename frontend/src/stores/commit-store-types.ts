import type { ReadonlySignal, Signal } from '@preact/signals';
import type { Session } from '../domain/types';
import type { AppStoreServices } from './app-store-services';
import type { Modal } from './store-types';

export interface CommitChange {
  path: string;
  old_path?: string;
  kind: string;
  staged?: boolean;
  unstaged?: boolean;
  untracked?: boolean;
  partially_staged?: boolean;
  additions?: number;
  deletions?: number;
  binary?: boolean;
}
export interface CommitStatus {
  branch?: string;
  detached?: boolean;
  staged: CommitChange[];
  unstaged: CommitChange[];
  untracked: CommitChange[];
  summary?: { files?: number; additions?: number; deletions?: number; binary_files?: number };
  fingerprint: Record<string, unknown>;
  status_token?: string;
  selection_available?: boolean;
  selection_unavailable_reason?: string;
  total_staged?: number;
  total_unstaged?: number;
  total_untracked?: number;
  truncated?: boolean;
}
export type CommitPhase =
  | 'closed'
  | 'loading'
  | 'choosing_scope'
  | 'planning_scope'
  | 'reviewing_scope'
  | 'staging'
  | 'drafting_message'
  | 'editing'
  | 'committing'
  | 'success'
  | 'error';
export interface PublishForm {
  kind: 'push' | 'pr';
  plan: Record<string, unknown>;
  branch: string;
  base: string;
  title: string;
  body: string;
  draft: boolean;
}
export interface CommitUIState {
  publishForm: PublishForm | null;
  publishBusy: boolean;
  publishLoading: boolean;
  publishPending: boolean;
  publishResult: Record<string, unknown> | null;
  sessionId: string;
  phase: CommitPhase;
  intent: string;
  status: CommitStatus | null;
  selected: string[];
  scopeSummary: string;
  message: string;
  generated: string;
  agent: string;
  agentSource: string;
  error: string;
  info: string;
  runId: string;
  operationId: string;
  result: Record<string, unknown> | null;
  reviewRequired: boolean;
  reviewingFromEditor: boolean;
  selectionNeedsApply: boolean;
}

export interface CommitStoreOptions {
  activeSession: ReadonlySignal<Session | null>;
  draftActive: ReadonlySignal<boolean>;
  modal: Signal<Modal>;
  busy: () => boolean;
  changed: () => void;
}

/**
 * The eagerly constructed `CommitStore` hands this seam to the on-demand
 * workflow so both share one state signal and one review baseline.
 */
export interface CommitWorkflowHost {
  readonly services: AppStoreServices;
  readonly state: Signal<CommitUIState>;
  readonly options: CommitStoreOptions;
  patch(patch: Partial<CommitUIState>): void;
  setReviewBaseline(paths: Iterable<string>): void;
  clearReviewBaseline(): void;
}

export const initialState = (): CommitUIState => ({
  publishForm: null,
  publishBusy: false,
  publishLoading: false,
  publishPending: false,
  publishResult: null,
  sessionId: '',
  phase: 'closed',
  intent: '',
  status: null,
  selected: [],
  scopeSummary: '',
  message: '',
  generated: '',
  agent: '',
  agentSource: '',
  error: '',
  info: '',
  runId: '',
  operationId: '',
  result: null,
  reviewRequired: false,
  reviewingFromEditor: false,
  selectionNeedsApply: false,
});

/** Sorted, de-duplicated paths from every Git status category. */
export const commitPaths = (status: CommitStatus | null): string[] =>
  [
    ...new Set(
      [...(status?.staged || []), ...(status?.unstaged || []), ...(status?.untracked || [])].map(
        (entry) => entry.path,
      ),
    ),
  ].sort();

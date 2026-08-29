import type { ModelOption } from '../domain/runtime';
import type {
  DiffComment,
  DiffFile,
  FilesystemObservation,
  OutputClaimDiagnostic,
} from '../domain/types';

export type Modal =
  | ''
  | 'settings'
  | 'rename'
  | 'ask-user'
  | 'approval'
  | 'mcp'
  | 'goal'
  | 'widgets'
  | 'branch'
  | 'branch-context'
  | 'project'
  | 'worktrees'
  | 'skills'
  | 'side';

export interface Toast {
  id: string;
  message: string;
  kind: 'info' | 'success' | 'error';
  leaving?: boolean;
}

export interface RuntimeOption extends ModelOption {
  [key: string]: unknown;
}

export interface SideQuestionState {
  sessionId: string;
  loading: boolean;
  running: boolean;
  draft: string;
  question: string;
  response: string;
  error: string;
  history: Array<{ question: string; response: string }>;
}

export interface DiffState {
  open: boolean;
  sessionId: string;
  scope: string;
  git: boolean;
  loading: boolean;
  files: DiffFile[];
  materializations: FilesystemObservation[];
  observations: FilesystemObservation[];
  claimDiagnostics: OutputClaimDiagnostic[];
  unavailableLineCountFiles: number;
  filter: string;
  comments: DiffComment[];
  historyComments: DiffComment[];
  error: string;
  maximized: boolean;
  width: number;
  selectedPath: string;
  followCurrentFile: boolean;
  worktreeDir?: string;
  worktreeTitle?: string;
  readOnly?: boolean;
}

export interface PendingInterjection {
  id: string;
  sessionId: string;
  content: string;
  state: 'sending' | 'pending' | 'committed' | 'failed';
}

export interface SendOptions {
  contentParts?: Record<string, unknown>[];
  inputText?: string;
  displayContent?: string;
  preserveComposer?: boolean;
  diffComments?: DiffComment[];
  onTransportStarted?: () => void;
  onTransportFailed?: (error: unknown) => void;
}

export interface HubAgent {
  id: string;
  name: string;
  target: string;
  active: boolean;
  attention: boolean;
}

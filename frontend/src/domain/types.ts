export type MessageRole =
  | 'user'
  | 'assistant'
  | 'error'
  | 'guardian-notice'
  | 'tool-group'
  | 'compaction'
  | 'compaction-boundary'
  | 'model-swap'
  | 'phase'
  | 'skill-run'
  | 'path-note';

export interface Attachment {
  id?: string;
  name: string;
  type: string;
  size?: number;
  dataURL?: string;
  previewURL?: string;
  url?: string;
  width?: number;
  height?: number;
  mention?: boolean;
  file?: File;
  status?: 'preparing' | 'ready' | 'error';
  progress?: number;
  error?: string;
  checksum?: string;
  blobRef?: string;
  draftId?: string;
}

export interface GuardianReview {
  outcome?: string;
  message?: string;
  model?: string;
  tool?: string;
  command?: string;
  path?: string;
  is_write?: boolean;
  workdir?: string;
}

export interface ToolCall {
  id: string;
  /** Response-local output item identity; call id remains the canonical execution identity. */
  itemId?: string;
  name: string;
  arguments?: string;
  argumentsFinalized?: boolean;
  status: 'running' | 'done' | 'cancelled' | 'error';
  resultStatus?: 'success' | 'error';
  result?: string;
  images?: string[];
  guardianReviews?: GuardianReview[];
  subagent?: Record<string, unknown>;
}

export interface Usage {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_input_tokens?: number;
  reasoning_tokens?: number;
  cost_usd?: number;
  [key: string]: unknown;
}

export interface Message {
  id: string;
  role: MessageRole;
  content: string;
  created: number;
  durableRowId?: number;
  clientMessageId?: string;
  responseId?: string;
  assistantSegmentOrdinal?: number;
  attachments?: Attachment[];
  tools?: ToolCall[];
  /** Streaming-only membership boundary; execution status may be done while the group remains appendable. */
  toolGroupClosed?: boolean;
  usage?: Usage;
  status?: string;
  title?: string;
  rawContent?: string;
  lineCount?: number;
  expanded?: boolean;
  activeBoundary?: boolean;
  askUser?: boolean;
  diffComments?: DiffComment[];
  [key: string]: unknown;
}

export interface MCPServer {
  name: string;
  configured: boolean;
  enabled: boolean;
  status: string;
  error: string;
  refreshWarning: string;
  tools: number;
  active: number;
  deferred: number;
  loadingMode: string;
}

export interface MCPResponse {
  servers: Record<string, unknown>[];
  enabled: string[];
}

export interface Goal {
  objective: string;
  token_budget?: number;
  status?: 'active' | 'paused' | 'completed';
  tokens_used?: number;
}

export interface Widget {
  id: string;
  name: string;
  url: string;
  mount?: string;
  description?: string;
  state?: string;
  error?: string;
}

export interface Session {
  id: string;
  number?: number;
  name: string;
  title: string;
  longTitle?: string;
  generatedShortTitle?: string;
  generatedLongTitle?: string;
  mode: string;
  origin: string;
  agent?: string;
  archived: boolean;
  pinned: boolean;
  created: number;
  lastMessageAt: number;
  lastResponseId?: string | null;
  activeResponseId?: string | null;
  /** Server-observed activity, available before this tab attaches to the response stream. */
  activeRun?: boolean;
  activeModel?: string;
  activeEffort?: string;
  activeReasoningMode?: string;
  activeProvider?: string;
  projectId?: string;
  projectName?: string;
  projectUnavailable?: boolean;
  projectUnavailableReason?: string;
  workingDir?: string;
  worktreeDir?: string;
  messages: Message[];
  usage?: Usage;
  goal?: Goal | null;
  mcpServers?: string[];
  mcpEnabled?: string[];
  messageCount?: number;
  transcriptRev?: number;
  fileChangeSummary?: { fileCount: number; additions: number; deletions: number; git: boolean };
}

export interface WorktreeRecoveryOffer {
  kind: 'conflict';
  title: string;
  question: string;
  yes_label: string;
  no_label: string;
  details?: string;
  conflicts?: string[];
  available: boolean;
  unavailable_reason?: string;
  decline_message?: string;
}

export interface Project {
  id: string;
  name: string;
  path?: string;
  archived?: boolean;
  available?: boolean;
  unavailableReason?: string;
  git?: boolean;
  sessions?: Session[];
  sessionCount?: number;
  next_cursor?: string;
  has_more?: boolean;
}

export interface DiffLine {
  kind: 'context' | 'add' | 'delete' | 'hunk' | 'gap';
  content: string;
  oldLine?: number;
  newLine?: number;
  hiddenOld?: number;
  hiddenNew?: number;
  gapDirection?: 'above' | 'between' | 'below';
}

export interface DiffFile {
  path: string;
  old_path?: string;
  status?: string;
  additions?: number;
  deletions?: number;
  binary?: boolean;
  image?: boolean;
  beforeURL?: string;
  afterURL?: string;
  patch?: string;
  lastChangedAt?: number;
  sequence?: number;
  snapshotSeq?: number;
  truncated?: boolean;
  provenance?: 'direct' | 'declared_transform' | 'declared_generate' | 'mixed';
  provenances?: string[];
  baselineState?: 'normal' | 'preexisting_dirty' | 'unknown';
  contentStatus?: string;
  contentAvailable?: boolean;
  claimCoverage?: 'complete' | 'truncated' | 'unavailable';
  context?: number;
  oldLineCount?: number;
  newLineCount?: number;
  lang?: string;
  expanded?: boolean;
  loading?: boolean;
  error?: string;
  lines?: DiffLine[];
}

export interface FilesystemObservation {
  id: number;
  classification: string;
  root?: string;
  createdCount: number;
  modifiedCount: number;
  deletedCount: number;
  sampledPaths: string[];
  samplesTruncated: boolean;
  coverageStatus: string;
  eventSeq: number;
}

export interface OutputClaimDiagnostic {
  normalizedPattern: string;
  claimKind: string;
  reason: string;
  coverageStatus: string;
  matchingPathCount: number;
  message?: string;
}

export interface DiffComment {
  id?: string;
  parentId?: string;
  path: string;
  side: 'old' | 'new';
  line: number;
  body: string;
  sessionId?: string;
  scope?: string;
  context?: string;
  contextBefore?: string[];
  contextAfter?: string[];
  anchorFingerprint?: string;
  fileChangeSeq?: number;
  rev?: number;
  updatedAt?: number;
  state?: 'fresh' | 'stale' | 'sending' | 'failed';
  error?: string;
  clientMessageId?: string;
  createdAt?: number;
  optimistic?: boolean;
}

export interface PlanStep {
  step: string;
  status: 'pending' | 'in_progress' | 'completed';
}

export interface CurrentPlan {
  explanation?: string;
  plan: PlanStep[];
}

export type InteractionState =
  | 'waiting'
  | 'dismissed'
  | 'submitting'
  | 'accepted'
  | 'denied'
  | 'cancelled-by-user'
  | 'cancelled-by-agent'
  | 'failed'
  | 'resolved-elsewhere';

export interface InteractionRecord {
  key: string;
  sessionId: string;
  responseId: string;
  requestId: string;
  kind: 'approval' | 'ask-user';
  state: InteractionState;
  order: number;
  createdAt: number;
  prompt: ApprovalPrompt | AskUserPrompt;
  error?: string;
  outcome?: string;
  resolvedAt?: number;
}

export interface AskUserQuestion {
  header?: string;
  question: string;
  multi_select?: boolean;
  options?: Array<{ label: string; description?: string }>;
}

export interface AskUserPrompt {
  sessionId: string;
  callId?: string;
  questions: AskUserQuestion[];
}

export interface ApprovalOption {
  index: number;
  choice?: string;
  label?: string;
  title?: string;
  description?: string;
  [key: string]: unknown;
}

export interface ApprovalPrompt {
  sessionId: string;
  id?: string;
  title?: string;
  intro?: string;
  path?: string;
  body?: string;
  note?: string;
  options?: ApprovalOption[];
  selectedIndex?: number;
  resumeAutoAvailable?: boolean;
}

export type RunStatus =
  | 'idle'
  | 'connecting'
  | 'checking'
  | 'streaming'
  | 'cancelling'
  | 'completed'
  | 'cancelled'
  | 'failed';

export interface ActiveRun {
  responseId: string;
  sessionId: string;
  epoch: number;
  status: RunStatus;
  lastSequence: number;
  startedRev: number;
  startedAt?: number;
  endedAt?: number;
  reconnects: number;
  error?: string;
  requestId?: string;
  notificationSubscriptionId?: string;
  finalRev?: number;
  durableHandoff?: boolean;
  summary?: string;
}

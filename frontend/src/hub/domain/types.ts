export interface HubNodeStatus {
  reachable: boolean;
  state: string;
  latency_ms: number;
  version?: string;
  agent?: string;
  capabilities?: string[];
  details?: Record<string, string>;
  error?: string;
}

export interface HubNodeDiagnostic {
  severity: string;
  code: string;
  message: string;
}

export interface HubNodeSession {
  id: string;
  number?: number;
  short_title: string;
  long_title?: string;
  pinned?: boolean;
  active_run?: boolean;
  interaction_required?: boolean;
  pending_interaction_count?: number;
  pending_interaction_kinds?: string[];
  last_message_at?: number;
  message_count?: number;
  resume_path: string;
}

export interface HubNodeSessions {
  count_label: string;
  has_more?: boolean;
  active_count?: number;
  input_required_count?: number;
  unseen_count?: number;
  attention_capability?: string;
  attention_last_success_at?: number;
  active?: HubNodeSession[];
  recent?: HubNodeSession[];
  resume_path?: string;
}

export interface HubNode {
  id: string;
  name: string;
  source: string;
  connection: string;
  url: string;
  base_path: string;
  proxy_path: string;
  new_session_path: string;
  has_token: boolean;
  status: HubNodeStatus;
  sessions?: HubNodeSessions;
  diagnostics?: HubNodeDiagnostic[];
}

export interface NodesResponse {
  nodes: HubNode[];
  resolver_error?: string;
}

export interface HubAttentionNode {
  node_id: string;
  node_name: string;
  capability_state: string;
  stale: boolean;
  last_success_at?: string;
  last_error?: string;
  running_count: number;
  input_required_count: number;
  unseen_count: number;
  has_green_indicator: boolean;
}

export interface HubAttentionInboxItem {
  node_id: string;
  node_name: string;
  session_id: string;
  session_number?: number;
  title: string;
  outcome: string;
  terminal_at?: string;
  attention_seq: number;
  resume_path: string;
}

export interface HubInputRequiredItem {
  node_id: string;
  node_name: string;
  session_id: string;
  session_number?: number;
  title: string;
  pending_interaction_count: number;
  pending_interaction_kinds?: string[];
  required_since?: string;
  resume_path: string;
  stale?: boolean;
}

export interface AttentionResponse {
  total_running: number;
  total_input_required: number;
  total_unseen: number;
  nodes: HubAttentionNode[];
  input_required: HubInputRequiredItem[];
  inbox: HubAttentionInboxItem[];
  has_more: boolean;
}

export interface HubDelegation {
  id: string;
  origin_node: string;
  target_node: string;
  agent_name?: string;
  prompt?: string;
  model?: string;
  cwd?: string;
  job_id?: string;
  run_id?: string;
  status: string;
  depth: number;
  chain?: string[];
  parent_delegation_id?: string;
  response?: string;
  error?: string;
  created_at: string;
  updated_at: string;
}

export interface DelegationsResponse {
  delegations: HubDelegation[];
}

export interface NodeFormData {
  name: string;
  url: string;
  token: string;
}

export interface TestNodeResponse {
  status: HubNodeStatus;
}

export interface AddNodeResponse {
  id: string;
  warning?: string;
}

export interface RegistrationInfoResponse {
  enabled: boolean;
  registration_token?: string;
}

export interface HubCredential {
  record_id: string;
  display_name: string;
  transports: string[] | null;
  created_at: string;
  last_used_at: string;
}

export interface CredentialsResponse {
  credentials: HubCredential[];
}

export interface HubSessionResponse {
  administrator: string;
  session: Record<string, unknown>;
  recently_authenticated: boolean;
  active_sessions: number;
}

export interface RedirectResponse {
  ok?: boolean;
  redirect: string;
}

export interface RevokeSessionsResponse {
  revoked: number;
}

export interface WebAuthnCreationWireOptions {
  publicKey: Omit<
    PublicKeyCredentialCreationOptions,
    'challenge' | 'user' | 'excludeCredentials'
  > & {
    challenge: string;
    user: Omit<PublicKeyCredentialUserEntity, 'id'> & { id: string };
    excludeCredentials?: Array<Omit<PublicKeyCredentialDescriptor, 'id'> & { id: string }>;
  };
}

export interface WebAuthnRequestWireOptions {
  publicKey: Omit<PublicKeyCredentialRequestOptions, 'challenge' | 'allowCredentials'> & {
    challenge: string;
    allowCredentials?: Array<Omit<PublicKeyCredentialDescriptor, 'id'> & { id: string }>;
  };
}

export interface SerializedPublicKeyCredential {
  id: string;
  rawId: string;
  type: string;
  response: Record<string, unknown>;
  clientExtensionResults: AuthenticationExtensionsClientOutputs;
  authenticatorAttachment: AuthenticatorAttachment | string | null;
}

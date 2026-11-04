import type { ActivitySnapshot, StatsChild } from '../api/endpoints';

export const activityChild = (patch: Partial<StatsChild> = {}): StatsChild => ({
  session_id: 'child',
  parent_session_id: 'parent',
  title: '@developer: Fix navigation',
  agent: 'developer',
  state: 'active',
  live: true,
  attention: false,
  revision: 1,
  input_tokens: 100,
  output_tokens: 200,
  cached_input_tokens: 300,
  cache_write_tokens: 400,
  tool_calls: 0,
  llm_turns: 1,
  model: 'provider:real-model',
  started_at: 1000,
  ...patch,
});

export const activitySnapshot = (patch: Partial<ActivitySnapshot> = {}): ActivitySnapshot => ({
  session_id: 'parent',
  agents: [activityChild()],
  jobs: [],
  agents_availability: 'available',
  jobs_availability: 'available',
  agents_partial: false,
  jobs_partial: false,
  ...patch,
});

export interface ChildRun {
  sessionId: string;
  parentSessionId: string;
  parentSpawnItemId?: number;
  parentSpawnCallId?: string;
  title: string;
  agent?: string;
  taskSummary?: string;
  state: string;
  attention: boolean;
  responseId?: string;
  runEpoch?: number;
  revision: number;
  startedAt?: number;
  endedAt?: number;
  approximateTimes?: boolean;
}

export interface StreamSupervisor {
  sessionId: string;
  responseId: string;
  generation: number;
  transportGeneration: number;
  abort: AbortController;
  retryTimer: number | null;
  terminal: boolean;
  cancelled: boolean;
  recoveryInFlight: boolean;
  subscriptionInFlight: boolean;
  watchdogTimer: number | null;
  lastSequence: number;
}

/**
 * Owns the complete transport lifecycle for one current response per session.
 * Async callbacks must retain their supervisor object and call owns() before
 * mutating state. Replacing an owner invalidates callbacks before aborting it.
 */
export class StreamSupervisors {
  private readonly owners = new Map<string, StreamSupervisor>();
  private readonly generations = new Map<string, number>();

  current(sessionId: string): StreamSupervisor | undefined {
    return this.owners.get(sessionId);
  }

  begin(sessionId: string, responseId: string, lastSequence = 0): StreamSupervisor {
    const previous = this.owners.get(sessionId);
    const generation = (this.generations.get(sessionId) || 0) + 1;
    this.generations.set(sessionId, generation);
    // Install the replacement first: abort callbacks from the previous owner
    // can no longer pass owns().
    const owner: StreamSupervisor = {
      sessionId,
      responseId,
      generation,
      transportGeneration: 0,
      abort: new AbortController(),
      retryTimer: null,
      terminal: false,
      cancelled: false,
      recoveryInFlight: false,
      subscriptionInFlight: false,
      watchdogTimer: null,
      lastSequence: Math.max(0, lastSequence),
    };
    this.owners.set(sessionId, owner);
    if (previous) this.cleanup(previous);
    return owner;
  }

  adoptResponse(owner: StreamSupervisor, responseId: string): boolean {
    if (!this.owns(owner) || !responseId) return false;
    owner.responseId = responseId;
    return true;
  }

  rekey(owner: StreamSupervisor, nextSessionId: string): boolean {
    if (!this.owns(owner) || !nextSessionId) return false;
    const previousSessionId = owner.sessionId;
    const replaced = this.owners.get(nextSessionId);
    this.owners.delete(previousSessionId);
    // Install the incoming owner before aborting a destination owner so every
    // synchronous abort callback is stale under both session keys.
    owner.sessionId = nextSessionId;
    this.owners.set(nextSessionId, owner);
    if (replaced && replaced !== owner) this.cleanup(replaced);
    this.generations.set(
      nextSessionId,
      Math.max(owner.generation, this.generations.get(nextSessionId) || 0),
    );
    return true;
  }

  owns(owner: StreamSupervisor, responseId = owner.responseId): boolean {
    return (
      this.owners.get(owner.sessionId) === owner &&
      owner.responseId === responseId &&
      !owner.cancelled &&
      !owner.terminal
    );
  }

  ownsTransport(owner: StreamSupervisor, transportGeneration: number): boolean {
    return this.owns(owner) && owner.transportGeneration === transportGeneration;
  }

  replaceAbort(owner: StreamSupervisor, invalidateSubscription = false): AbortController | null {
    if (!this.owns(owner)) return null;
    const previous = owner.abort;
    this.clearWatchdog(owner);
    owner.transportGeneration += 1;
    owner.abort = new AbortController();
    if (invalidateSubscription) owner.subscriptionInFlight = false;
    // Install the replacement lease before aborting. Synchronous callbacks from
    // the previous transport can no longer mutate or clean up the new lease.
    previous.abort();
    return owner.abort;
  }

  checkpoint(owner: StreamSupervisor, transportGeneration: number, sequence: number): boolean {
    if (!this.ownsTransport(owner, transportGeneration) || !Number.isFinite(sequence)) return false;
    owner.lastSequence = Math.max(0, Math.trunc(sequence));
    return true;
  }

  advance(owner: StreamSupervisor, sequence: number): boolean {
    if (!this.owns(owner) || !Number.isFinite(sequence) || sequence <= owner.lastSequence)
      return false;
    owner.lastSequence = sequence;
    return true;
  }

  startSubscription(owner: StreamSupervisor): boolean {
    if (!this.owns(owner) || owner.recoveryInFlight || owner.subscriptionInFlight) return false;
    this.clearRetry(owner);
    owner.subscriptionInFlight = true;
    return true;
  }

  finishSubscription(owner: StreamSupervisor, transportGeneration: number): void {
    if (this.ownsTransport(owner, transportGeneration)) owner.subscriptionInFlight = false;
    this.clearWatchdog(owner, transportGeneration);
  }

  touchWatchdog(
    owner: StreamSupervisor,
    transportGeneration: number,
    callback: () => void,
    delayMs: number,
  ): boolean {
    if (!this.ownsTransport(owner, transportGeneration)) return false;
    this.clearWatchdog(owner, transportGeneration);
    owner.watchdogTimer = window.setTimeout(
      () => {
        owner.watchdogTimer = null;
        if (this.ownsTransport(owner, transportGeneration)) callback();
      },
      Math.max(0, delayMs),
    );
    return true;
  }

  clearWatchdog(owner: StreamSupervisor, transportGeneration?: number): void {
    if (transportGeneration !== undefined && owner.transportGeneration !== transportGeneration)
      return;
    if (owner.watchdogTimer !== null) {
      window.clearTimeout(owner.watchdogTimer);
      owner.watchdogTimer = null;
    }
  }

  startRecovery(owner: StreamSupervisor): boolean {
    if (!this.owns(owner) || owner.recoveryInFlight) return false;
    this.clearRetry(owner);
    owner.recoveryInFlight = true;
    return true;
  }

  finishRecovery(owner: StreamSupervisor): void {
    if (this.owners.get(owner.sessionId) === owner) owner.recoveryInFlight = false;
  }

  scheduleRetry(owner: StreamSupervisor, callback: () => void, delayMs: number): boolean {
    if (!this.owns(owner) || owner.retryTimer !== null) return false;
    owner.retryTimer = window.setTimeout(
      () => {
        owner.retryTimer = null;
        if (this.owns(owner)) callback();
      },
      Math.max(0, delayMs),
    );
    return true;
  }

  cancel(sessionId: string, responseId?: string): StreamSupervisor | undefined {
    const owner = this.owners.get(sessionId);
    if (!owner || (responseId && owner.responseId !== responseId)) return undefined;
    owner.cancelled = true;
    this.owners.delete(sessionId);
    this.cleanup(owner);
    return owner;
  }

  retire(owner: StreamSupervisor): boolean {
    if (this.owners.get(owner.sessionId) !== owner) return false;
    owner.terminal = true;
    this.owners.delete(owner.sessionId);
    this.cleanup(owner);
    return true;
  }

  dispose(): void {
    const owners = [...this.owners.values()];
    this.owners.clear();
    owners.forEach((owner) => {
      owner.cancelled = true;
      this.cleanup(owner);
    });
  }

  private clearRetry(owner: StreamSupervisor): void {
    if (owner.retryTimer !== null) {
      window.clearTimeout(owner.retryTimer);
      owner.retryTimer = null;
    }
  }

  private cleanup(owner: StreamSupervisor): void {
    this.clearRetry(owner);
    this.clearWatchdog(owner);
    owner.recoveryInFlight = false;
    owner.subscriptionInFlight = false;
    owner.abort.abort();
  }
}

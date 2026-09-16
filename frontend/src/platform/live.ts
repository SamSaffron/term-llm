import { liveCapability, type VoiceCapability } from './voice';

export { liveCapability };

export type LivePhase =
  | 'idle'
  | 'requesting-permission'
  | 'connecting'
  | 'listening'
  | 'speaking'
  | 'working'
  | 'failed'
  | 'ended';

export interface LiveStartResponse {
  live_id: string;
  session_id: string;
  sdp: string;
}

export interface LiveStopResponse {
  live_id: string;
  status: 'ended';
}

export interface LiveSnapshot {
  phase: LivePhase;
  capability: VoiceCapability;
  generation: number;
  liveId: string;
  sessionId: string;
  error?: string;
  retryable?: boolean;
}

export type LiveStart = (sdp: string, sessionId: string) => Promise<LiveStartResponse>;
export type LiveStop = (liveId: string) => Promise<LiveStopResponse>;

export interface LiveCallOptions {
  peerConnectionConfig?: RTCConfiguration;
  createPeerConnection?: (config?: RTCConfiguration) => RTCPeerConnection;
}

function liveError(error: unknown): { message: string; retryable: boolean } {
  const name = (error as { name?: string } | null)?.name || '';
  if (name === 'NotAllowedError' || name === 'SecurityError')
    return {
      message: 'Microphone access was denied. Enable it in browser or system settings.',
      retryable: true,
    };
  if (name === 'NotFoundError' || name === 'DevicesNotFoundError')
    return { message: 'No microphone was found.', retryable: true };
  return {
    message:
      error instanceof Error && error.message ? error.message : 'Live voice could not start.',
    retryable: true,
  };
}

export class LiveCall {
  private snapshotValue: LiveSnapshot;
  private listener: (snapshot: LiveSnapshot) => void = () => {};
  private stream: MediaStream | null = null;
  private peer: RTCPeerConnection | null = null;
  private channel: RTCDataChannel | null = null;
  private audio: HTMLAudioElement | null = null;
  private liveId = '';
  private cancelICEWait: (() => void) | null = null;
  private stoppedTracks = new WeakSet<MediaStreamTrack>();
  private disposed = false;

  constructor(
    private readonly startEndpoint: LiveStart,
    private readonly stopEndpoint: LiveStop,
    private readonly options: LiveCallOptions = {},
  ) {
    this.snapshotValue = {
      phase: 'idle',
      capability: liveCapability(),
      generation: 0,
      liveId: '',
      sessionId: '',
    };
  }

  get snapshot(): LiveSnapshot {
    return this.snapshotValue;
  }

  subscribe(listener: (snapshot: LiveSnapshot) => void): () => void {
    this.listener = listener;
    listener(this.snapshotValue);
    return () => {
      if (this.listener === listener) this.listener = () => {};
    };
  }

  private update(patch: Partial<LiveSnapshot>, generation = this.snapshotValue.generation): void {
    if (this.disposed || generation !== this.snapshotValue.generation) return;
    this.snapshotValue = { ...this.snapshotValue, ...patch };
    this.listener(this.snapshotValue);
  }

  async start(sessionId: string): Promise<LiveStartResponse | null> {
    if (!['idle', 'failed', 'ended'].includes(this.snapshotValue.phase)) return null;

    // Create the autoplay target synchronously in the click gesture, before microphone permission
    // or signaling yields control to the browser.
    this.cleanupMedia();
    const audio = document.createElement('audio');
    audio.autoplay = true;
    audio.hidden = true;
    audio.setAttribute('aria-hidden', 'true');
    document.body.append(audio);
    this.audio = audio;

    const capability = liveCapability();
    if (!capability.supported) {
      this.snapshotValue = {
        phase: 'failed',
        capability,
        generation: this.snapshotValue.generation + 1,
        liveId: '',
        sessionId,
        error: capability.reason,
        retryable: false,
      };
      this.removeAudio();
      this.listener(this.snapshotValue);
      return null;
    }

    const generation = this.snapshotValue.generation + 1;
    this.snapshotValue = {
      phase: 'requesting-permission',
      capability,
      generation,
      liveId: '',
      sessionId,
    };
    this.listener(this.snapshotValue);

    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      if (!this.current(generation)) {
        this.stopStream(stream);
        return null;
      }
      this.stream = stream;
      this.update({ phase: 'connecting' }, generation);

      const peer = this.options.createPeerConnection
        ? this.options.createPeerConnection(this.options.peerConnectionConfig)
        : new RTCPeerConnection(this.options.peerConnectionConfig);
      this.peer = peer;
      const channel = peer.createDataChannel('oai-events', { ordered: true });
      this.channel = channel;
      peer.ontrack = (event) => this.onTrack(event, generation);
      peer.onconnectionstatechange = () => {
        if (peer.connectionState === 'failed' && this.current(generation))
          this.fail(new Error('The live voice connection failed.'), generation);
      };
      const tracks = stream.getAudioTracks?.() || stream.getTracks();
      tracks.forEach((track) => peer.addTrack(track, stream));

      const offer = await peer.createOffer();
      if (!this.current(generation)) return null;
      await peer.setLocalDescription(offer);
      if (!this.current(generation)) return null;
      await this.waitForICE(peer);
      if (!this.current(generation)) return null;

      const sdp = peer.localDescription?.sdp || offer.sdp || '';
      if (!sdp) throw new Error('The browser did not create a live voice offer.');
      const started = await this.startEndpoint(sdp, sessionId);
      if (!this.current(generation)) {
        if (started.live_id) void this.stopEndpoint(started.live_id).catch(() => undefined);
        return null;
      }
      if (!started.live_id || !started.sdp)
        throw new Error('The live voice server returned an incomplete connection answer.');
      this.liveId = started.live_id;
      await peer.setRemoteDescription({ type: 'answer', sdp: started.sdp });
      if (!this.current(generation)) return null;
      this.update(
        {
          phase: 'listening',
          liveId: started.live_id,
          sessionId: started.session_id || sessionId,
          error: undefined,
          retryable: undefined,
        },
        generation,
      );
      return started;
    } catch (error) {
      if (this.current(generation)) this.fail(error, generation);
      return null;
    }
  }

  private waitForICE(peer: RTCPeerConnection): Promise<void> {
    if (peer.iceGatheringState === 'complete') return Promise.resolve();
    return new Promise((resolve) => {
      let settled = false;
      const finish = () => {
        if (settled) return;
        settled = true;
        window.clearTimeout(timer);
        peer.removeEventListener('icegatheringstatechange', onChange);
        if (this.cancelICEWait === finish) this.cancelICEWait = null;
        resolve();
      };
      const onChange = () => {
        if (peer.iceGatheringState === 'complete') finish();
      };
      const timer = window.setTimeout(finish, 5_000);
      this.cancelICEWait = finish;
      peer.addEventListener('icegatheringstatechange', onChange);
      onChange();
    });
  }

  private onTrack(event: RTCTrackEvent, generation: number): void {
    if (!this.current(generation) || !this.audio) return;
    const remote = event.streams[0] || new MediaStream([event.track]);
    this.audio.srcObject = remote;
    void this.audio.play().catch(() => {
      // The autoplay attribute is the primary path; a later provider track may still be gated.
    });
  }

  private fail(error: unknown, generation: number): void {
    if (!this.current(generation)) return;
    const failure = liveError(error);
    const liveId = this.liveId;
    this.liveId = '';
    this.cleanupMedia();
    if (liveId) void this.stopEndpoint(liveId).catch(() => undefined);
    this.update(
      {
        phase: 'failed',
        liveId: '',
        error: failure.message,
        retryable: failure.retryable,
      },
      generation,
    );
  }

  async stop(): Promise<void> {
    if (this.disposed) return;
    const liveId = this.liveId || this.snapshotValue.liveId;
    const generation = this.snapshotValue.generation + 1;
    this.snapshotValue = {
      ...this.snapshotValue,
      phase: 'ended',
      generation,
      liveId: '',
      error: undefined,
      retryable: undefined,
    };
    this.liveId = '';
    this.cleanupMedia();
    this.listener(this.snapshotValue);
    if (liveId) await this.stopEndpoint(liveId);
  }

  dispose(): void {
    if (this.disposed) return;
    const liveId = this.liveId || this.snapshotValue.liveId;
    this.disposed = true;
    this.liveId = '';
    this.snapshotValue = {
      ...this.snapshotValue,
      phase: 'ended',
      generation: this.snapshotValue.generation + 1,
      liveId: '',
    };
    this.cleanupMedia();
    if (liveId) void this.stopEndpoint(liveId).catch(() => undefined);
    this.listener = () => {};
  }

  private current(generation: number): boolean {
    return !this.disposed && generation === this.snapshotValue.generation;
  }

  private cleanupMedia(): void {
    this.cancelICEWait?.();
    this.cancelICEWait = null;
    if (this.channel) {
      try {
        this.channel.close();
      } catch {
        // The peer teardown below is still authoritative.
      }
    }
    this.channel = null;
    if (this.peer) {
      this.peer.ontrack = null;
      this.peer.onconnectionstatechange = null;
      try {
        this.peer.close();
      } catch {
        // Tracks and the audio sink still need cleanup.
      }
    }
    this.peer = null;
    if (this.stream) this.stopStream(this.stream);
    this.stream = null;
    this.removeAudio();
  }

  private stopStream(stream: MediaStream): void {
    stream.getTracks().forEach((track) => {
      if (this.stoppedTracks.has(track)) return;
      this.stoppedTracks.add(track);
      track.stop();
    });
  }

  private removeAudio(): void {
    if (!this.audio) return;
    this.audio.srcObject = null;
    this.audio.remove();
    this.audio = null;
  }
}

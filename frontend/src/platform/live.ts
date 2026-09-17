import { decodeSSE } from '../api/client';
import { liveAudioSocketOpen, openLiveAudioSocket, type LiveAudioSocket } from '../api/live-socket';
import type { LivePCMArtifactCapture } from './live-pcm-diagnostics';
import {
  PCM_OUTPUT_PROCESSOR_NAME,
  pcmOutputProcessorSource,
  type PCMOutputEvent,
} from './live-pcm-output-worklet';
import { liveCapability, type LiveTransport, type VoiceCapability } from './voice';

export { liveCapability };
export type { LiveTransport };

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
  transport?: LiveTransport;
  sdp?: string;
  audio_url?: string;
  audio_capability?: string;
  diagnostics?: boolean;
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

export type LiveStart = (
  sdp: string,
  sessionId: string,
  audioTransport?: 'websocket_pcm' | 'http_pcm',
) => Promise<LiveStartResponse>;
export type LiveStop = (liveId: string) => Promise<LiveStopResponse>;
export type LiveAudioOutput = (
  liveId: string,
  capability: string,
  signal: AbortSignal,
) => Promise<Response>;
// Diagnostics reuse the lazy PCM-input boundary so the eager endpoint table
// does not grow for a debug-only route. The fourth argument selects JSON
// diagnostics; ordinary audio calls omit it and retain the binary endpoint.
export type LiveAudioInput = (
  liveId: string,
  pcm: ArrayBuffer,
  signal: AbortSignal,
  diagnostics?: LiveDiagnosticsReport,
) => Promise<Response>;
export interface LiveDiagnosticsReport {
  sequence: number;
  elapsed_ms: number;
  audio_context_state: string;
  stream_open: boolean;
  packets_received: number;
  bytes_received: number;
  packets_scheduled: number;
  packets_ended: number;
  playback_queued_seconds: number;
  underruns: number;
  flush_interrupts: number;
  flush_buffer_resets: number;
  input_packets: number;
  input_bytes: number;
  input_queue_drops: number;
  post_count: number;
  post_errors: number;
  post_latency_total_ms: number;
  post_latency_max_ms: number;
  post_inflight: number;
  input_silence_ms: number;
  output_silence_ms: number;
}

export interface LiveCallOptions {
  transport?: LiveTransport;
  openPCMOutput?: LiveAudioOutput;
  sendPCMInput?: LiveAudioInput;
  peerConnectionConfig?: RTCConfiguration;
  createPeerConnection?: (config?: RTCConfiguration) => RTCPeerConnection;
  createWebSocket?: (url: string) => LiveAudioSocket;
  createAudioContext?: () => AudioContext;
  createAudioWorkletNode?: (
    context: AudioContext,
    name: string,
    options?: AudioWorkletNodeOptions,
  ) => AudioWorkletNode;
  captureModuleURL?: string;
}

const PCM_INPUT_RATE = 16_000;
const PCM_OUTPUT_RATE = 24_000;
const PCM_INPUT_BATCH_BYTES = (PCM_INPUT_RATE * 2) / 10;
// Start promptly at 100 ms, but drain accumulated audio after a slow POST.
// A fixed 100 ms maximum loses most microphone audio when RTT exceeds 100 ms.
const PCM_INPUT_POST_LIMIT = 64 * 1024;
const PCM_INPUT_QUEUE_LIMIT = PCM_INPUT_RATE * 2 * 5;
const PCM_INPUT_BATCH_MS = 100;
const PCM_SOCKET_BUFFER_LIMIT = 256 * 1024;
// Retain at most ten minutes of provider audio and cap packet metadata too.
// Exceeding either limit fails the call visibly instead of dropping speech.
const PCM_PLAYBACK_MAX_BYTES = PCM_OUTPUT_RATE * 2 * 60 * 10;
const PCM_PLAYBACK_MAX_SAMPLES = PCM_PLAYBACK_MAX_BYTES / 2;
const PCM_PLAYBACK_MAX_PACKETS = 16_384;
const PCM_CONNECT_TIMEOUT_MS = 10_000;
const LIVE_DIAGNOSTICS_INTERVAL_MS = 5_000;
const LIVE_PCM_CAPTURE_STORAGE_KEY = 'term-llm-live-pcm-capture';

function pcmArtifactCaptureRequested(): boolean {
  try {
    return localStorage.getItem(LIVE_PCM_CAPTURE_STORAGE_KEY) === '1';
  } catch {
    return false;
  }
}

interface LiveDiagnosticState {
  startedAt: number;
  lastInputAt: number;
  lastOutputAt: number;
  sequence: number;
  streamOpen: boolean;
  packetsReceived: number;
  bytesReceived: number;
  packetsScheduled: number;
  packetsEnded: number;
  underruns: number;
  flushInterrupts: number;
  flushBufferResets: number;
  inputPackets: number;
  inputBytes: number;
  inputQueueDrops: number;
  postCount: number;
  postErrors: number;
  postLatencyTotalMS: number;
  postLatencyMaxMS: number;
  postInflight: number;
  reportInflight: boolean;
  timer: number;
  abort: AbortController;
}

const captureProcessorSource = `
class TermLLMPCMCapture extends AudioWorkletProcessor {
  constructor() {
    super();
    this.input = [];
    this.output = [];
    this.position = 0;
  }
  process(inputs) {
    const channel = inputs[0] && inputs[0][0];
    if (!channel || channel.length === 0) return true;
    for (let i = 0; i < channel.length; i += 1) this.input.push(channel[i]);
    const step = sampleRate / ${PCM_INPUT_RATE};
    while (this.position + 1 < this.input.length) {
      const index = Math.floor(this.position);
      const fraction = this.position - index;
      const sample = this.input[index] + (this.input[index + 1] - this.input[index]) * fraction;
      this.output.push(Math.max(-1, Math.min(1, sample)));
      this.position += step;
      if (this.output.length >= 320) {
        const pcm = new Int16Array(this.output.length);
        for (let i = 0; i < this.output.length; i += 1)
          pcm[i] = this.output[i] < 0 ? this.output[i] * 32768 : this.output[i] * 32767;
        this.port.postMessage(pcm.buffer, [pcm.buffer]);
        this.output = [];
      }
    }
    const consumed = Math.min(Math.floor(this.position), Math.max(0, this.input.length - 1));
    if (consumed > 0) {
      this.input.splice(0, consumed);
      this.position -= consumed;
    }
    return true;
  }
}
registerProcessor('term-llm-pcm-capture', TermLLMPCMCapture);
`;

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
  private socket: LiveAudioSocket | null = null;
  private mediaAbort: AbortController | null = null;
  private pcmInputReady = false;
  private inputQueue: Uint8Array[] = [];
  private inputQueuedBytes = 0;
  private inputSending = false;
  private inputTimer = 0;
  private audioContext: AudioContext | null = null;
  private audioResume: Promise<void> | null = null;
  private captureNode: AudioWorkletNode | null = null;
  private captureSource: MediaStreamAudioSourceNode | null = null;
  private captureSink: GainNode | null = null;
  private playbackNode: AudioWorkletNode | null = null;
  private playbackEpoch = 0;
  private playbackBytes = 0;
  private playbackPackets = 0;
  private playbackEnqueuedSamples = 0;
  private playbackQueuedSamples = 0;
  private playbackConsumedBytes = 0;
  private playbackConsumedPackets = 0;
  private playbackConsumedSamples = 0;
  private playbackReportedUnderruns = 0;
  private liveId = '';
  private cancelICEWait: (() => void) | null = null;
  private diagnostics: LiveDiagnosticState | null = null;
  private pcmArtifactDiagnostics: LivePCMArtifactCapture | null = null;
  private stoppedTracks = new WeakSet<MediaStreamTrack>();
  private disposed = false;

  constructor(
    private readonly startEndpoint: LiveStart,
    private readonly stopEndpoint: LiveStop,
    private readonly options: LiveCallOptions = {},
  ) {
    this.snapshotValue = {
      phase: 'idle',
      capability: liveCapability(this.transport),
      generation: 0,
      liveId: '',
      sessionId: '',
    };
  }

  private get transport(): LiveTransport {
    return this.options.transport || 'webrtc';
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

    this.cleanupMedia();
    if (this.transport === 'webrtc') this.createAutoplayTarget();

    const capability = liveCapability(this.transport);
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
      const pcmTransport = this.transport === 'websocket_pcm' || this.transport === 'http_pcm';
      const audioReady = pcmTransport ? this.preparePCMAudioContext() : null;
      const mediaRequest = navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
        },
      });
      let stream: MediaStream;
      if (audioReady) {
        const [audioResult, mediaResult] = await Promise.allSettled([audioReady, mediaRequest]);
        if (mediaResult.status === 'rejected') throw mediaResult.reason;
        if (audioResult.status === 'rejected') {
          this.stopStream(mediaResult.value);
          throw audioResult.reason;
        }
        stream = mediaResult.value;
      } else {
        stream = await mediaRequest;
      }
      if (!this.current(generation)) {
        this.stopStream(stream);
        return null;
      }
      this.stream = stream;
      this.update({ phase: 'connecting' }, generation);
      return pcmTransport
        ? await this.startPCM(stream, sessionId, generation)
        : await this.startWebRTC(stream, sessionId, generation);
    } catch (error) {
      if (this.current(generation)) this.fail(error, generation);
      return null;
    }
  }

  private async startWebRTC(
    stream: MediaStream,
    sessionId: string,
    generation: number,
  ): Promise<LiveStartResponse | null> {
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
    this.connected(started, sessionId, generation);
    return started;
  }

  private preparePCMAudioContext(): Promise<void> {
    const context = this.options.createAudioContext
      ? this.options.createAudioContext()
      : new AudioContext({ latencyHint: 'interactive' });
    this.audioContext = context;
    context.onstatechange = () => {
      // Permission prompts and route changes can suspend an already unlocked
      // iOS context. Resume only this call, never a context being torn down.
      if (this.audioContext === context && this.stream) this.recoverPCMAudioContext();
    };
    return context.resume();
  }

  private recoverPCMAudioContext(): void {
    const context = this.audioContext;
    if (!context || context.state === 'running' || context.state === 'closed' || this.audioResume)
      return;
    const generation = this.snapshotValue.generation;
    const pending = context.resume();
    this.audioResume = pending;
    void pending
      .catch((error: unknown) => {
        if (this.audioContext === context && this.current(generation)) this.fail(error, generation);
      })
      .finally(() => {
        if (this.audioResume === pending) this.audioResume = null;
      });
  }

  private async startPCM(
    stream: MediaStream,
    sessionId: string,
    generation: number,
  ): Promise<LiveStartResponse | null> {
    const context = this.audioContext;
    if (!context) throw new Error('The live PCM audio context was not initialized.');
    if (context.state !== 'running') await context.resume();
    if (!this.current(generation)) return null;
    const moduleURL = this.options.captureModuleURL || this.createCaptureModuleURL();
    try {
      await context.audioWorklet.addModule(moduleURL);
    } finally {
      if (!this.options.captureModuleURL) URL.revokeObjectURL(moduleURL);
    }
    if (!this.current(generation)) return null;

    const createWorkletNode = (name: string, options: AudioWorkletNodeOptions) =>
      this.options.createAudioWorkletNode
        ? this.options.createAudioWorkletNode(context, name, options)
        : new AudioWorkletNode(context, name, options);
    const capture = createWorkletNode('term-llm-pcm-capture', {
      numberOfInputs: 1,
      numberOfOutputs: 1,
      outputChannelCount: [1],
    });
    this.captureNode = capture;
    capture.port.onmessage = (event: MessageEvent<ArrayBuffer>) =>
      this.sendCapturedPCM(event.data, generation);

    const playback = createWorkletNode(PCM_OUTPUT_PROCESSOR_NAME, {
      numberOfInputs: 0,
      numberOfOutputs: 1,
      outputChannelCount: [1],
      processorOptions: {
        inputSampleRate: PCM_OUTPUT_RATE,
        maxQueuedSamples: PCM_PLAYBACK_MAX_SAMPLES,
        maxQueuedPackets: PCM_PLAYBACK_MAX_PACKETS,
      },
    });
    this.playbackNode = playback;
    this.playbackEpoch += 1;
    this.resetPlaybackAccounting();
    const playbackEpoch = this.playbackEpoch;
    playback.port.onmessage = (event: MessageEvent<PCMOutputEvent>) =>
      this.onPlaybackEvent(event.data, playback, generation);
    playback.port.postMessage({ type: 'reset', epoch: playbackEpoch });
    playback.connect(context.destination);

    const source = context.createMediaStreamSource(stream);
    this.captureSource = source;
    const sink = context.createGain();
    sink.gain.value = 0;
    this.captureSink = sink;
    source.connect(capture);
    capture.connect(sink);
    sink.connect(context.destination);

    const transport = this.transport === 'http_pcm' ? 'http_pcm' : 'websocket_pcm';
    const started = await this.startEndpoint('', sessionId, transport);
    if (!this.current(generation)) {
      if (started.live_id) void this.stopEndpoint(started.live_id).catch(() => undefined);
      return null;
    }
    if (!started.live_id || started.transport !== transport)
      throw new Error('The live voice server returned an incomplete PCM connection.');
    this.liveId = started.live_id;
    this.enableDiagnostics(started, generation);
    await this.enablePCMArtifactDiagnostics(started, context, generation);
    if (!this.current(generation)) return null;
    const renderedDestination = this.pcmArtifactDiagnostics?.renderedDestination;
    if (renderedDestination) playback.connect(renderedDestination);
    if (this.transport === 'http_pcm') {
      if (!started.audio_capability)
        throw new Error('The live voice server did not authorize HTTP audio.');
      await this.openPCMHTTP(started.live_id, started.audio_capability, generation);
    } else {
      if (!started.audio_url)
        throw new Error('The live voice server did not return a socket audio endpoint.');
      await this.openPCMWebSocket(started.audio_url, generation);
    }
    if (!this.current(generation)) return null;
    this.connected(started, sessionId, generation);
    return started;
  }

  private createCaptureModuleURL(): string {
    return URL.createObjectURL(
      new Blob([captureProcessorSource, '\n', pcmOutputProcessorSource], {
        type: 'text/javascript',
      }),
    );
  }

  private async openPCMHTTP(liveId: string, capability: string, generation: number): Promise<void> {
    const open = this.options.openPCMOutput;
    if (!open || !this.options.sendPCMInput)
      throw new Error('The live HTTP audio transport is unavailable.');
    const controller = new AbortController();
    this.mediaAbort = controller;
    const response = await open(liveId, capability, controller.signal);
    if (!this.current(generation)) {
      controller.abort();
      await response.body?.cancel().catch(() => undefined);
      return;
    }
    if (!response.ok || !response.body) {
      const body = await response.text();
      throw new Error(body || `The live audio stream returned ${response.status}.`);
    }
    // Capture starts before HTTP attachment. Do not upload until the server
    // confirms the output consumer, otherwise an early batch can receive 409.
    this.pcmInputReady = true;
    const diagnostics = this.diagnostics;
    if (diagnostics) diagnostics.streamOpen = true;
    void this.consumePCMHTTP(response.body, controller.signal, generation).then(
      () => {
        if (diagnostics) diagnostics.streamOpen = false;
        if (this.current(generation) && this.liveId)
          this.fail(new Error('The live audio stream closed.'), generation);
      },
      (error: unknown) => {
        if (diagnostics) diagnostics.streamOpen = false;
        if (this.current(generation) && this.liveId && !controller.signal.aborted)
          this.fail(error, generation);
      },
    );
  }

  private async consumePCMHTTP(
    body: ReadableStream<Uint8Array>,
    signal: AbortSignal,
    generation: number,
  ): Promise<void> {
    for await (const message of decodeSSE(body, signal)) {
      if (!this.current(generation)) return;
      if (message.event === 'interrupt') {
        this.flushPlayback('interrupt');
        continue;
      }
      if (message.event !== 'audio') continue;
      let encoded: string;
      try {
        encoded = String((JSON.parse(message.data || '{}') as { audio?: string }).audio || '');
      } catch {
        continue;
      }
      if (!encoded || encoded.length > 90 * 1024) continue;
      let binary: string;
      try {
        binary = atob(encoded);
      } catch {
        continue;
      }
      if (!binary.length || binary.length > 64 * 1024 || binary.length % 2 !== 0) continue;
      const pcm = new Uint8Array(binary.length);
      for (let index = 0; index < binary.length; index += 1) pcm[index] = binary.charCodeAt(index);
      this.playPCM(pcm.buffer);
    }
  }

  private openPCMWebSocket(path: string, generation: number): Promise<void> {
    const target = new URL(path, window.location.href);
    if (target.origin !== window.location.origin)
      return Promise.reject(new Error('The live audio endpoint was not same-origin.'));
    target.protocol = target.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = this.options.createWebSocket
      ? this.options.createWebSocket(target.toString())
      : openLiveAudioSocket(target.toString());
    this.socket = socket;
    socket.binaryType = 'arraybuffer';
    socket.onmessage = (event) => this.onPCMMessage(event, generation);
    return new Promise((resolve, reject) => {
      let settled = false;
      const timer = window.setTimeout(
        () => finish(new Error('The live audio connection timed out.')),
        PCM_CONNECT_TIMEOUT_MS,
      );
      const finish = (error?: Error) => {
        if (settled) return;
        settled = true;
        window.clearTimeout(timer);
        if (error) reject(error);
        else resolve();
      };
      socket.onopen = () => {
        if (this.diagnostics) this.diagnostics.streamOpen = true;
        finish();
      };
      socket.onerror = () => finish(new Error('The live audio connection failed.'));
      socket.onclose = () => {
        if (this.diagnostics) this.diagnostics.streamOpen = false;
        if (!settled) {
          finish(new Error('The live audio connection closed during startup.'));
          return;
        }
        if (this.current(generation) && this.liveId)
          this.fail(new Error('The live audio connection closed.'), generation);
      };
    });
  }

  private sendCapturedPCM(data: ArrayBuffer, generation: number): void {
    if (!this.current(generation) || data.byteLength === 0 || data.byteLength > 64 * 1024) return;
    const diagnostics = this.diagnostics;
    if (diagnostics) {
      diagnostics.inputPackets += 1;
      diagnostics.inputBytes += data.byteLength;
      diagnostics.lastInputAt = performance.now();
    }
    if (this.transport === 'websocket_pcm') {
      const socket = this.socket;
      if (
        !socket ||
        !liveAudioSocketOpen(socket) ||
        socket.bufferedAmount > PCM_SOCKET_BUFFER_LIMIT
      ) {
        if (diagnostics) diagnostics.inputQueueDrops += 1;
        return;
      }
      socket.send(data);
      return;
    }
    if (!this.pcmInputReady || !this.mediaAbort || this.mediaAbort.signal.aborted || !this.liveId) {
      if (diagnostics) diagnostics.inputQueueDrops += 1;
      return;
    }
    if (this.inputQueuedBytes + data.byteLength > PCM_INPUT_QUEUE_LIMIT) {
      if (diagnostics) diagnostics.inputQueueDrops += 1;
      this.fail(
        new Error(
          'Live microphone upload cannot keep up with this connection. Please start a new call.',
        ),
        generation,
      );
      return;
    }
    const chunk = new Uint8Array(data.slice(0));
    this.inputQueue.push(chunk);
    this.inputQueuedBytes += chunk.byteLength;
    if (this.inputQueuedBytes >= PCM_INPUT_BATCH_BYTES) void this.drainPCMInput(generation);
    else this.schedulePCMInput(generation);
  }

  private schedulePCMInput(generation: number): void {
    if (this.inputTimer || !this.inputQueuedBytes) return;
    this.inputTimer = window.setTimeout(() => {
      this.inputTimer = 0;
      void this.drainPCMInput(generation);
    }, PCM_INPUT_BATCH_MS);
  }

  private takePCMInputBatch(): ArrayBuffer {
    const size = Math.min(this.inputQueuedBytes, PCM_INPUT_POST_LIMIT);
    const batch = new Uint8Array(size);
    let offset = 0;
    while (offset < size) {
      const chunk = this.inputQueue[0];
      const take = Math.min(chunk.byteLength, size - offset);
      batch.set(chunk.subarray(0, take), offset);
      offset += take;
      if (take === chunk.byteLength) this.inputQueue.shift();
      else this.inputQueue[0] = chunk.subarray(take);
    }
    this.inputQueuedBytes -= size;
    return batch.buffer;
  }

  private async drainPCMInput(generation: number): Promise<void> {
    if (this.inputSending || !this.inputQueuedBytes || !this.current(generation)) return;
    const send = this.options.sendPCMInput;
    const controller = this.mediaAbort;
    const liveId = this.liveId;
    if (!send || !controller || controller.signal.aborted || !liveId) return;
    if (this.inputTimer) window.clearTimeout(this.inputTimer);
    this.inputTimer = 0;
    const batch = this.takePCMInputBatch();
    this.inputSending = true;
    const diagnostics = this.diagnostics;
    const postStarted = diagnostics ? performance.now() : 0;
    if (diagnostics) {
      diagnostics.postCount += 1;
      diagnostics.postInflight += 1;
    }
    try {
      const response = await send(liveId, batch, controller.signal);
      if (!response.ok) {
        const body = await response.text();
        throw new Error(body || `Live microphone input returned ${response.status}.`);
      }
    } catch (error) {
      if (diagnostics) diagnostics.postErrors += 1;
      if (this.current(generation) && !controller.signal.aborted) this.fail(error, generation);
      return;
    } finally {
      if (diagnostics) {
        const latency = Math.max(0, performance.now() - postStarted);
        diagnostics.postLatencyTotalMS += latency;
        diagnostics.postLatencyMaxMS = Math.max(diagnostics.postLatencyMaxMS, latency);
        diagnostics.postInflight = Math.max(0, diagnostics.postInflight - 1);
      }
      // A canceled request may settle after another call has started.
      if (this.mediaAbort === controller) this.inputSending = false;
    }
    if (!this.current(generation) || controller.signal.aborted) return;
    if (this.inputQueuedBytes >= PCM_INPUT_BATCH_BYTES) void this.drainPCMInput(generation);
    else this.schedulePCMInput(generation);
  }

  private onPCMMessage(event: MessageEvent, generation: number): void {
    if (!this.current(generation)) return;
    if (typeof event.data === 'string') {
      try {
        const control = JSON.parse(event.data) as { type?: string };
        if (control.type === 'interrupt') this.flushPlayback('interrupt');
      } catch {
        // Unknown control frames are forward-compatible and safely ignored.
      }
      return;
    }
    if (event.data instanceof ArrayBuffer) this.playPCM(event.data);
    else if (event.data instanceof Blob)
      void event.data.arrayBuffer().then((data) => {
        if (this.current(generation)) this.playPCM(data);
      });
  }

  private playPCM(data: ArrayBuffer): void {
    const context = this.audioContext;
    const playback = this.playbackNode;
    this.recoverPCMAudioContext();
    if (
      !context ||
      !playback ||
      data.byteLength === 0 ||
      data.byteLength % 2 !== 0 ||
      data.byteLength > 64 * 1024
    )
      return;
    if (
      this.playbackBytes + data.byteLength > PCM_PLAYBACK_MAX_BYTES ||
      this.playbackPackets + 1 > PCM_PLAYBACK_MAX_PACKETS
    ) {
      this.fail(
        new Error('Live audio playback capacity exceeded. Please start a new call.'),
        this.snapshotValue.generation,
      );
      return;
    }

    const diagnostics = this.diagnostics;
    if (diagnostics) {
      diagnostics.packetsReceived += 1;
      diagnostics.bytesReceived += data.byteLength;
      diagnostics.lastOutputAt = performance.now();
    }
    this.pcmArtifactDiagnostics?.captureIncomingPCM(data, context.currentTime, context.currentTime);

    const bytes = data.byteLength;
    this.playbackBytes += bytes;
    this.playbackPackets += 1;
    this.playbackEnqueuedSamples += bytes / 2;
    this.playbackQueuedSamples += bytes / 2;
    if (diagnostics) diagnostics.packetsScheduled += 1;
    try {
      playback.port.postMessage({ type: 'enqueue', epoch: this.playbackEpoch, pcm: data }, [data]);
    } catch (error) {
      this.playbackBytes -= bytes;
      this.playbackPackets -= 1;
      this.playbackEnqueuedSamples -= bytes / 2;
      this.playbackQueuedSamples -= bytes / 2;
      if (diagnostics) diagnostics.packetsScheduled -= 1;
      this.fail(error, this.snapshotValue.generation);
    }
  }

  private onPlaybackEvent(
    event: PCMOutputEvent,
    playback: AudioWorkletNode,
    generation: number,
  ): void {
    if (
      !this.current(generation) ||
      playback !== this.playbackNode ||
      event.epoch !== this.playbackEpoch
    )
      return;
    if (event.type === 'overflow') {
      this.fail(
        new Error('Live audio playback capacity exceeded. Please start a new call.'),
        generation,
      );
      return;
    }

    const consumedBytes = Math.min(
      Math.max(this.playbackConsumedBytes, event.consumedBytes),
      this.playbackConsumedBytes + this.playbackBytes,
    );
    const consumedPackets = Math.min(
      Math.max(this.playbackConsumedPackets, event.consumedPackets),
      this.playbackConsumedPackets + this.playbackPackets,
    );
    const consumedSamples = Math.min(
      Math.max(this.playbackConsumedSamples, event.consumedSamples),
      this.playbackEnqueuedSamples,
    );
    const byteDelta = consumedBytes - this.playbackConsumedBytes;
    const packetDelta = consumedPackets - this.playbackConsumedPackets;
    this.playbackConsumedBytes = consumedBytes;
    this.playbackConsumedPackets = consumedPackets;
    this.playbackConsumedSamples = consumedSamples;
    this.playbackBytes = Math.max(0, this.playbackBytes - byteDelta);
    this.playbackPackets = Math.max(0, this.playbackPackets - packetDelta);
    this.playbackQueuedSamples = this.playbackEnqueuedSamples - consumedSamples;

    const diagnostics = this.diagnostics;
    if (!diagnostics) return;
    diagnostics.packetsEnded += packetDelta;
    const underruns = Math.max(this.playbackReportedUnderruns, event.underruns);
    diagnostics.underruns += underruns - this.playbackReportedUnderruns;
    this.playbackReportedUnderruns = underruns;
  }

  private resetPlaybackAccounting(): void {
    this.playbackBytes = 0;
    this.playbackPackets = 0;
    this.playbackEnqueuedSamples = 0;
    this.playbackQueuedSamples = 0;
    this.playbackConsumedBytes = 0;
    this.playbackConsumedPackets = 0;
    this.playbackConsumedSamples = 0;
    this.playbackReportedUnderruns = 0;
  }

  private flushPlayback(reason?: 'interrupt'): void {
    const diagnostics = this.diagnostics;
    if (reason === 'interrupt')
      this.pcmArtifactDiagnostics?.captureInterrupt(this.audioContext?.currentTime || 0);
    if (diagnostics && reason === 'interrupt') diagnostics.flushInterrupts += 1;
    if (diagnostics && reason) console.info(`[live] playback flush: ${reason}`);
    this.playbackEpoch += 1;
    this.resetPlaybackAccounting();
    try {
      this.playbackNode?.port.postMessage({ type: 'reset', epoch: this.playbackEpoch });
    } catch {
      // Closing the context below is authoritative during cleanup.
    }
  }

  private async enablePCMArtifactDiagnostics(
    started: LiveStartResponse,
    context: AudioContext,
    generation: number,
  ): Promise<void> {
    if (!started.diagnostics || !pcmArtifactCaptureRequested() || !this.current(generation)) return;
    try {
      const { startLivePCMArtifactCapture } = await import('./live-pcm-diagnostics');
      if (!this.current(generation)) return;
      this.pcmArtifactDiagnostics = startLivePCMArtifactCapture(context, started.live_id);
    } catch (error) {
      console.warn(
        '[live] PCM artifact capture is unavailable',
        error instanceof Error ? error.message : 'unknown error',
      );
    }
  }

  private enableDiagnostics(started: LiveStartResponse, generation: number): void {
    const report = this.options.sendPCMInput;
    if (!started.diagnostics || !report || !this.current(generation)) return;
    const now = performance.now();
    const state: LiveDiagnosticState = {
      startedAt: now,
      lastInputAt: now,
      lastOutputAt: now,
      sequence: 0,
      streamOpen: false,
      packetsReceived: 0,
      bytesReceived: 0,
      packetsScheduled: 0,
      packetsEnded: 0,
      underruns: 0,
      flushInterrupts: 0,
      flushBufferResets: 0,
      inputPackets: 0,
      inputBytes: 0,
      inputQueueDrops: 0,
      postCount: 0,
      postErrors: 0,
      postLatencyTotalMS: 0,
      postLatencyMaxMS: 0,
      postInflight: 0,
      reportInflight: false,
      timer: 0,
      abort: new AbortController(),
    };
    this.diagnostics = state;
    console.info('[live] diagnostics enabled', {
      audio_context_state: this.audioContext?.state || 'unknown',
      transport: this.transport,
    });
    state.timer = window.setInterval(
      () => void this.reportDiagnosticSnapshot(state, generation),
      LIVE_DIAGNOSTICS_INTERVAL_MS,
    );
  }

  private async reportDiagnosticSnapshot(
    state: LiveDiagnosticState,
    generation: number,
  ): Promise<void> {
    const reportEndpoint = this.options.sendPCMInput;
    if (
      !reportEndpoint ||
      this.diagnostics !== state ||
      state.abort.signal.aborted ||
      state.reportInflight ||
      !this.current(generation) ||
      !this.liveId
    )
      return;
    const now = performance.now();
    const context = this.audioContext;
    const report: LiveDiagnosticsReport = {
      sequence: ++state.sequence,
      elapsed_ms: Math.max(0, now - state.startedAt),
      audio_context_state: context?.state || 'unknown',
      stream_open: state.streamOpen,
      packets_received: state.packetsReceived,
      bytes_received: state.bytesReceived,
      packets_scheduled: state.packetsScheduled,
      packets_ended: state.packetsEnded,
      playback_queued_seconds: this.playbackQueuedSamples / PCM_OUTPUT_RATE,
      underruns: state.underruns,
      flush_interrupts: state.flushInterrupts,
      flush_buffer_resets: state.flushBufferResets,
      input_packets: state.inputPackets,
      input_bytes: state.inputBytes,
      input_queue_drops: state.inputQueueDrops,
      post_count: state.postCount,
      post_errors: state.postErrors,
      post_latency_total_ms: state.postLatencyTotalMS,
      post_latency_max_ms: state.postLatencyMaxMS,
      post_inflight: state.postInflight,
      input_silence_ms: Math.max(0, now - state.lastInputAt),
      output_silence_ms: Math.max(0, now - state.lastOutputAt),
    };
    // Silence is reported as an observation while the stream remains open. It
    // never triggers a reconnect or failure: an idle, healthy call is normal.
    state.reportInflight = true;
    console.info('[live] diagnostics', report);
    try {
      const response = await reportEndpoint(
        this.liveId,
        new ArrayBuffer(0),
        state.abort.signal,
        report,
      );
      if (!response.ok && this.diagnostics === state)
        console.warn(`[live] diagnostics report returned ${response.status}`);
    } catch (error) {
      if (!state.abort.signal.aborted && this.diagnostics === state)
        console.warn(
          '[live] diagnostics report failed',
          error instanceof Error ? error.name : 'unknown',
        );
    } finally {
      state.reportInflight = false;
    }
  }

  private disableDiagnostics(): void {
    const diagnostics = this.diagnostics;
    this.diagnostics = null;
    if (!diagnostics) return;
    window.clearInterval(diagnostics.timer);
    diagnostics.abort.abort();
    diagnostics.streamOpen = false;
    console.info('[live] diagnostics stopped');
  }

  private connected(
    started: LiveStartResponse,
    requestedSessionId: string,
    generation: number,
  ): void {
    this.update(
      {
        phase: 'listening',
        liveId: started.live_id,
        sessionId: started.session_id || requestedSessionId,
        error: undefined,
        retryable: undefined,
      },
      generation,
    );
  }

  private createAutoplayTarget(): void {
    // Create this synchronously in the click gesture before microphone permission yields control.
    const audio = document.createElement('audio');
    audio.autoplay = true;
    audio.hidden = true;
    audio.setAttribute('aria-hidden', 'true');
    document.body.append(audio);
    this.audio = audio;
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
    this.disableDiagnostics();
    this.cancelICEWait?.();
    this.cancelICEWait = null;
    this.mediaAbort?.abort();
    this.mediaAbort = null;
    this.pcmInputReady = false;
    if (this.inputTimer) window.clearTimeout(this.inputTimer);
    this.inputTimer = 0;
    this.inputQueue = [];
    this.inputQueuedBytes = 0;
    this.inputSending = false;
    if (this.socket) {
      this.socket.onopen = null;
      this.socket.onmessage = null;
      this.socket.onerror = null;
      this.socket.onclose = null;
      try {
        this.socket.close();
      } catch {
        // Continue releasing local media resources.
      }
    }
    this.socket = null;
    this.flushPlayback();
    if (this.playbackNode) {
      this.playbackNode.port.onmessage = null;
      this.playbackNode.port.close();
      this.playbackNode.disconnect();
    }
    this.playbackNode = null;
    this.pcmArtifactDiagnostics?.stop('call_cleanup');
    this.pcmArtifactDiagnostics = null;
    if (this.captureNode) {
      this.captureNode.port.onmessage = null;
      this.captureNode.port.close();
      this.captureNode.disconnect();
    }
    this.captureNode = null;
    this.captureSource?.disconnect();
    this.captureSource = null;
    this.captureSink?.disconnect();
    this.captureSink = null;
    if (this.audioContext) {
      this.audioContext.onstatechange = null;
      void this.audioContext.close().catch(() => undefined);
    }
    this.audioContext = null;
    this.audioResume = null;
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

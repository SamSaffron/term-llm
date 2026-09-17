const PCM_SAMPLE_RATE = 24_000;
const PCM_CHANNELS = 1;
const PCM_BYTES_PER_SAMPLE = 2;
const MAX_CAPTURE_MS = 60_000;
const MAX_PCM_BYTES = PCM_SAMPLE_RATE * PCM_BYTES_PER_SAMPLE * (MAX_CAPTURE_MS / 1_000);
const MAX_RENDERED_BYTES = 8 * 1024 * 1024;
const MAX_BOUNDARIES = 4_096;
const PCM_SLAB_BYTES = 64 * 1024;
const EDGE_SAMPLE_COUNT = 8;
const RECORDER_TIMESLICE_MS = 1_000;
const RECORDER_FINALIZATION_TIMEOUT_MS = 5_000;
const DOWNLOAD_URL_REVOKE_DELAY_MS = 60_000;

const RENDERED_MIME_CANDIDATES = [
  'audio/webm;codecs=opus',
  'audio/webm',
  'audio/ogg;codecs=opus',
  'audio/ogg',
  'audio/mp4',
];

export const LIVE_PCM_CAPTURE_STORAGE_KEY = 'term-llm-live-pcm-capture';
export const LIVE_PCM_CAPTURE_MAX_MS = MAX_CAPTURE_MS;
export const LIVE_PCM_CAPTURE_MAX_BYTES = MAX_PCM_BYTES;
export const LIVE_PCM_RENDERED_MAX_BYTES = MAX_RENDERED_BYTES;

interface PCMChunkBoundary {
  index: number;
  elapsed_ms: number;
  received_bytes: number;
  retained_bytes: number;
  retained_byte_offset: number;
  context_time_seconds: number;
  scheduled_at_context_seconds: number;
  leading_samples: number[];
  trailing_samples: number[];
  min_sample: number | null;
  max_sample: number | null;
}

interface RenderedChunkBoundary {
  index: number;
  elapsed_ms: number;
  bytes: number;
  retained_byte_offset: number;
}

interface InterruptBoundary {
  elapsed_ms: number;
  retained_pcm_byte_offset: number;
  context_time_seconds: number;
}

export interface LivePCMArtifactMetadata {
  schema: 1;
  live_id: string;
  started_at: string;
  stopped_at: string | null;
  stop_reason: string | null;
  elapsed_ms: number;
  limits: {
    duration_ms: number;
    incoming_pcm_bytes: number;
    browser_rendered_bytes: number;
    chunk_boundaries: number;
  };
  incoming_pcm: {
    format: 'signed 16-bit little-endian mono';
    sample_rate_hz: number;
    received_bytes: number;
    retained_bytes: number;
    retained_samples: number;
    truncated: boolean;
    chunks_received: number;
    chunk_boundaries_retained: number;
    chunk_boundaries_dropped: number;
    min_sample: number | null;
    max_sample: number | null;
    leading_samples: number[];
    trailing_samples: number[];
    chunks: PCMChunkBoundary[];
  };
  browser_rendered: {
    supported: boolean;
    mime_type: string;
    retained_bytes: number;
    truncated: boolean;
    error: string | null;
    chunk_boundaries_dropped: number;
    chunks: RenderedChunkBoundary[];
  };
  interruptions: InterruptBoundary[];
}

export interface LivePCMArtifactStatus {
  active: boolean;
  available: boolean;
  live_id: string;
  incoming_pcm_bytes: number;
  browser_rendered_bytes: number;
  browser_rendered_supported: boolean;
  browser_rendered_mime_type: string;
  stopped: boolean;
}

export interface LivePCMArtifactDevTools {
  download(): Promise<string[]>;
  status(): LivePCMArtifactStatus;
  clear(): void;
}

export interface LivePCMArtifactCapture {
  readonly renderedDestination: AudioNode | null;
  captureIncomingPCM(data: ArrayBuffer, contextTime: number, scheduledAtContextTime: number): void;
  captureInterrupt(contextTime: number): void;
  stop(reason?: string): void;
}

declare global {
  interface Window {
    termLLMLivePCMArtifacts?: LivePCMArtifactDevTools;
  }
}

interface CaptureFinalization {
  readonly promise: Promise<void>;
  settled: boolean;
  resolve(): void;
}

interface RetainedCapture {
  liveId: string;
  startedAtISO: string;
  startedAtPerformance: number;
  stoppedAtISO: string | null;
  stoppedAtPerformance: number | null;
  stopReason: string | null;
  active: boolean;
  incomingChunks: Uint8Array[];
  incomingReceivedBytes: number;
  incomingRetainedBytes: number;
  incomingChunksReceived: number;
  incomingTruncated: boolean;
  incomingMin: number | null;
  incomingMax: number | null;
  incomingLeading: number[];
  incomingTrailing: number[];
  boundaries: PCMChunkBoundary[];
  boundariesDropped: number;
  interruptions: InterruptBoundary[];
  renderedSupported: boolean;
  renderedMimeType: string;
  renderedChunks: Blob[];
  renderedBytes: number;
  renderedTruncated: boolean;
  renderedError: string | null;
  renderedBoundaries: RenderedChunkBoundary[];
  renderedBoundariesDropped: number;
  renderedFinalization: CaptureFinalization;
}

let activeCapture: BrowserLivePCMArtifactCapture | null = null;
let retainedCapture: RetainedCapture | null = null;

function elapsed(state: RetainedCapture): number {
  const end = state.stoppedAtPerformance ?? performance.now();
  return Math.max(0, end - state.startedAtPerformance);
}

function recorderMIME(): string {
  if (typeof MediaRecorder === 'undefined' || typeof MediaRecorder.isTypeSupported !== 'function')
    return '';
  return RENDERED_MIME_CANDIDATES.find((mime) => MediaRecorder.isTypeSupported(mime)) || '';
}

function stopTracks(stream: MediaStream): void {
  for (const track of stream.getTracks()) track.stop();
}

function renderedExtension(mime: string): string {
  const type = mime.split(';', 1)[0].toLowerCase();
  if (type === 'audio/ogg') return 'ogg';
  if (type === 'audio/mp4') return 'mp4';
  if (type === 'audio/wav' || type === 'audio/x-wav') return 'wav';
  return 'webm';
}

function sampleEdges(
  view: DataView,
  sampleCount: number,
): {
  leading: number[];
  trailing: number[];
  min: number | null;
  max: number | null;
} {
  const leading: number[] = [];
  const trailing: number[] = [];
  let min: number | null = null;
  let max: number | null = null;
  for (let index = 0; index < sampleCount; index += 1) {
    const sample = view.getInt16(index * PCM_BYTES_PER_SAMPLE, true);
    if (index < EDGE_SAMPLE_COUNT) leading.push(sample);
    if (index >= Math.max(0, sampleCount - EDGE_SAMPLE_COUNT)) trailing.push(sample);
    min = min === null ? sample : Math.min(min, sample);
    max = max === null ? sample : Math.max(max, sample);
  }
  return { leading, trailing, min, max };
}

function appendPCM(state: RetainedCapture, data: ArrayBuffer, retainedBytes: number): void {
  let sourceOffset = 0;
  while (sourceOffset < retainedBytes) {
    const retainedOffset = state.incomingRetainedBytes + sourceOffset;
    const slabIndex = Math.floor(retainedOffset / PCM_SLAB_BYTES);
    const slabOffset = retainedOffset % PCM_SLAB_BYTES;
    let slab = state.incomingChunks[slabIndex];
    if (!slab) {
      slab = new Uint8Array(Math.min(PCM_SLAB_BYTES, MAX_PCM_BYTES - slabIndex * PCM_SLAB_BYTES));
      state.incomingChunks.push(slab);
    }
    const count = Math.min(retainedBytes - sourceOffset, slab.byteLength - slabOffset);
    slab.set(new Uint8Array(data, sourceOffset, count), slabOffset);
    sourceOffset += count;
  }
}

function retainedPCMChunks(state: RetainedCapture): Uint8Array[] {
  let remaining = state.incomingRetainedBytes;
  return state.incomingChunks.map((slab) => {
    const retained = slab.subarray(0, Math.min(slab.byteLength, remaining));
    remaining -= retained.byteLength;
    return retained;
  });
}

class BrowserLivePCMArtifactCapture implements LivePCMArtifactCapture {
  private destination: MediaStreamAudioDestinationNode | null = null;
  private recorder: MediaRecorder | null = null;
  private durationTimer = 0;
  private finalizationTimer = 0;
  private recorderStopRequested = false;
  private stopped = false;

  constructor(
    private readonly context: AudioContext,
    private readonly state: RetainedCapture,
  ) {
    this.startRenderedCapture();
    this.durationTimer = window.setTimeout(() => this.stop('duration_limit'), MAX_CAPTURE_MS);
  }

  get renderedDestination(): AudioNode | null {
    return this.stopped || this.recorder?.state !== 'recording' ? null : this.destination;
  }

  captureIncomingPCM(data: ArrayBuffer, contextTime: number, scheduledAtContextTime: number): void {
    if (this.stopped || data.byteLength === 0 || data.byteLength % PCM_BYTES_PER_SAMPLE !== 0)
      return;

    const state = this.state;
    state.incomingReceivedBytes += data.byteLength;
    state.incomingChunksReceived += 1;
    const remaining = Math.max(0, MAX_PCM_BYTES - state.incomingRetainedBytes);
    const retainedBytes = Math.min(data.byteLength, remaining) & ~1;
    const retainedOffset = state.incomingRetainedBytes;
    let edges: ReturnType<typeof sampleEdges> = {
      leading: [],
      trailing: [],
      min: null,
      max: null,
    };
    if (retainedBytes > 0) {
      appendPCM(state, data, retainedBytes);
      state.incomingRetainedBytes += retainedBytes;
      const view = new DataView(data, 0, retainedBytes);
      edges = sampleEdges(view, retainedBytes / PCM_BYTES_PER_SAMPLE);
      state.incomingMin =
        edges.min === null
          ? state.incomingMin
          : state.incomingMin === null
            ? edges.min
            : Math.min(state.incomingMin, edges.min);
      state.incomingMax =
        edges.max === null
          ? state.incomingMax
          : state.incomingMax === null
            ? edges.max
            : Math.max(state.incomingMax, edges.max);
      if (state.incomingLeading.length < EDGE_SAMPLE_COUNT)
        state.incomingLeading.push(
          ...edges.leading.slice(0, EDGE_SAMPLE_COUNT - state.incomingLeading.length),
        );
      state.incomingTrailing = [...state.incomingTrailing, ...edges.trailing].slice(
        -EDGE_SAMPLE_COUNT,
      );
    }
    if (retainedBytes < data.byteLength) state.incomingTruncated = true;

    const boundary: PCMChunkBoundary = {
      index: state.incomingChunksReceived - 1,
      elapsed_ms: elapsed(state),
      received_bytes: data.byteLength,
      retained_bytes: retainedBytes,
      retained_byte_offset: retainedOffset,
      context_time_seconds: contextTime,
      scheduled_at_context_seconds: scheduledAtContextTime,
      leading_samples: edges.leading,
      trailing_samples: edges.trailing,
      min_sample: edges.min,
      max_sample: edges.max,
    };
    if (state.boundaries.length < MAX_BOUNDARIES) state.boundaries.push(boundary);
    else state.boundariesDropped += 1;
  }

  captureInterrupt(contextTime: number): void {
    if (this.stopped || this.state.interruptions.length >= MAX_BOUNDARIES) return;
    this.state.interruptions.push({
      elapsed_ms: elapsed(this.state),
      retained_pcm_byte_offset: this.state.incomingRetainedBytes,
      context_time_seconds: contextTime,
    });
  }

  stop(reason = 'call_cleanup'): void {
    if (this.stopped) return;
    this.stopped = true;
    this.state.active = false;
    this.state.stopReason = reason;
    this.state.stoppedAtISO = new Date().toISOString();
    this.state.stoppedAtPerformance = performance.now();
    if (this.durationTimer) window.clearTimeout(this.durationTimer);
    this.durationTimer = 0;
    this.stopRenderedCapture();
    if (activeCapture === this) activeCapture = null;
  }

  stopIfRetaining(state: RetainedCapture, reason: string): void {
    if (this.state === state) this.stop(reason);
  }

  private stopRenderedCapture(): void {
    const recorder = this.recorder;
    if (!recorder || this.state.renderedFinalization.settled) {
      this.releaseDestination();
      return;
    }

    if (!this.recorderStopRequested && recorder.state === 'recording') {
      this.recorderStopRequested = true;
      try {
        recorder.stop();
      } catch (error) {
        this.state.renderedError ||=
          error instanceof Error ? error.message : 'Recorder stop failed.';
        this.finishRenderedCapture();
        return;
      }
    }

    if (this.state.renderedFinalization.settled) return;
    this.releaseDestination();
    if (!this.finalizationTimer) {
      this.finalizationTimer = window.setTimeout(() => {
        this.state.renderedError ||=
          'Timed out waiting for browser-rendered recording to finalize.';
        this.finishRenderedCapture();
      }, RECORDER_FINALIZATION_TIMEOUT_MS);
    }
  }

  private releaseDestination(): void {
    if (!this.destination) return;
    stopTracks(this.destination.stream);
    this.destination.disconnect();
    this.destination = null;
  }

  private finishRenderedCapture(): void {
    if (this.state.renderedFinalization.settled) return;
    if (this.finalizationTimer) window.clearTimeout(this.finalizationTimer);
    this.finalizationTimer = 0;
    const recorder = this.recorder;
    this.recorder = null;
    if (recorder) {
      recorder.ondataavailable = null;
      recorder.onerror = null;
      recorder.onstop = null;
    }
    this.releaseDestination();
    this.state.renderedFinalization.resolve();
  }

  private startRenderedCapture(): void {
    if (typeof this.context.createMediaStreamDestination !== 'function') {
      this.state.renderedError = 'MediaStreamAudioDestinationNode is unavailable.';
      this.finishRenderedCapture();
      return;
    }
    if (typeof MediaRecorder === 'undefined') {
      this.state.renderedError = 'MediaRecorder is unavailable.';
      this.finishRenderedCapture();
      return;
    }

    let destination: MediaStreamAudioDestinationNode | null = null;
    try {
      destination = this.context.createMediaStreamDestination();
      const selectedMIME = recorderMIME();
      const recorder = selectedMIME
        ? new MediaRecorder(destination.stream, { mimeType: selectedMIME })
        : new MediaRecorder(destination.stream);
      this.destination = destination;
      this.recorder = recorder;
      this.state.renderedSupported = true;
      this.state.renderedMimeType = recorder.mimeType || selectedMIME;
      recorder.ondataavailable = (event: BlobEvent) => this.retainRenderedChunk(event.data);
      recorder.onerror = (event: Event) => {
        const recorderError = (event as Event & { error?: DOMException }).error;
        this.state.renderedError = recorderError?.message || 'Browser-rendered recording failed.';
      };
      recorder.onstop = () => this.finishRenderedCapture();
      recorder.start(RECORDER_TIMESLICE_MS);
    } catch (error) {
      this.state.renderedSupported = false;
      this.state.renderedError =
        error instanceof Error ? error.message : 'Browser-rendered recording is unavailable.';
      if (destination && this.destination !== destination) {
        stopTracks(destination.stream);
        destination.disconnect();
      }
      this.finishRenderedCapture();
    }
  }

  private retainRenderedChunk(chunk: Blob): void {
    if (chunk.size === 0) return;
    const state = this.state;
    if (!state.renderedMimeType && chunk.type) state.renderedMimeType = chunk.type;
    if (
      state.renderedBoundaries.length >= MAX_BOUNDARIES ||
      state.renderedBytes + chunk.size > MAX_RENDERED_BYTES
    ) {
      state.renderedTruncated = true;
      state.renderedBoundariesDropped += 1;
      this.stopRenderedCapture();
      return;
    }
    state.renderedBoundaries.push({
      index: state.renderedChunks.length,
      elapsed_ms: elapsed(state),
      bytes: chunk.size,
      retained_byte_offset: state.renderedBytes,
    });
    state.renderedChunks.push(chunk);
    state.renderedBytes += chunk.size;
  }
}

function createCaptureFinalization(): CaptureFinalization {
  let resolvePromise!: () => void;
  const finalization: CaptureFinalization = {
    promise: new Promise<void>((resolve) => {
      resolvePromise = resolve;
    }),
    settled: false,
    resolve: () => {
      if (finalization.settled) return;
      finalization.settled = true;
      resolvePromise();
    },
  };
  return finalization;
}

function newRetainedCapture(liveId: string): RetainedCapture {
  return {
    liveId,
    startedAtISO: new Date().toISOString(),
    startedAtPerformance: performance.now(),
    stoppedAtISO: null,
    stoppedAtPerformance: null,
    stopReason: null,
    active: true,
    incomingChunks: [],
    incomingReceivedBytes: 0,
    incomingRetainedBytes: 0,
    incomingChunksReceived: 0,
    incomingTruncated: false,
    incomingMin: null,
    incomingMax: null,
    incomingLeading: [],
    incomingTrailing: [],
    boundaries: [],
    boundariesDropped: 0,
    interruptions: [],
    renderedSupported: false,
    renderedMimeType: '',
    renderedChunks: [],
    renderedBytes: 0,
    renderedTruncated: false,
    renderedError: null,
    renderedBoundaries: [],
    renderedBoundariesDropped: 0,
    renderedFinalization: createCaptureFinalization(),
  };
}

export function startLivePCMArtifactCapture(
  context: AudioContext,
  liveId: string,
): LivePCMArtifactCapture {
  activeCapture?.stop('replaced_by_new_call');
  const state = newRetainedCapture(liveId);
  retainedCapture = state;
  const capture = new BrowserLivePCMArtifactCapture(context, state);
  activeCapture = capture;
  console.info(
    '[live] PCM artifact capture enabled; after stopping the call run window.termLLMLivePCMArtifacts.download()',
  );
  return capture;
}

export function buildPCM16WAV(chunks: readonly Uint8Array[]): Blob {
  const dataBytes = chunks.reduce((sum, chunk) => sum + (chunk.byteLength & ~1), 0);
  const header = new ArrayBuffer(44);
  const view = new DataView(header);
  const writeASCII = (offset: number, value: string) => {
    for (let index = 0; index < value.length; index += 1)
      view.setUint8(offset + index, value.charCodeAt(index));
  };
  writeASCII(0, 'RIFF');
  view.setUint32(4, 36 + dataBytes, true);
  writeASCII(8, 'WAVE');
  writeASCII(12, 'fmt ');
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, PCM_CHANNELS, true);
  view.setUint32(24, PCM_SAMPLE_RATE, true);
  view.setUint32(28, PCM_SAMPLE_RATE * PCM_CHANNELS * PCM_BYTES_PER_SAMPLE, true);
  view.setUint16(32, PCM_CHANNELS * PCM_BYTES_PER_SAMPLE, true);
  view.setUint16(34, PCM_BYTES_PER_SAMPLE * 8, true);
  writeASCII(36, 'data');
  view.setUint32(40, dataBytes, true);
  return new Blob(
    [
      header,
      ...chunks.map((chunk) => new Uint8Array(chunk.subarray(0, chunk.byteLength & ~1)).buffer),
    ],
    { type: 'audio/wav' },
  );
}

function artifactMetadata(state: RetainedCapture): LivePCMArtifactMetadata {
  return {
    schema: 1,
    live_id: state.liveId,
    started_at: state.startedAtISO,
    stopped_at: state.stoppedAtISO,
    stop_reason: state.stopReason,
    elapsed_ms: elapsed(state),
    limits: {
      duration_ms: MAX_CAPTURE_MS,
      incoming_pcm_bytes: MAX_PCM_BYTES,
      browser_rendered_bytes: MAX_RENDERED_BYTES,
      chunk_boundaries: MAX_BOUNDARIES,
    },
    incoming_pcm: {
      format: 'signed 16-bit little-endian mono',
      sample_rate_hz: PCM_SAMPLE_RATE,
      received_bytes: state.incomingReceivedBytes,
      retained_bytes: state.incomingRetainedBytes,
      retained_samples: state.incomingRetainedBytes / PCM_BYTES_PER_SAMPLE,
      truncated: state.incomingTruncated,
      chunks_received: state.incomingChunksReceived,
      chunk_boundaries_retained: state.boundaries.length,
      chunk_boundaries_dropped: state.boundariesDropped,
      min_sample: state.incomingMin,
      max_sample: state.incomingMax,
      leading_samples: [...state.incomingLeading],
      trailing_samples: [...state.incomingTrailing],
      chunks: state.boundaries.map((boundary) => ({ ...boundary })),
    },
    browser_rendered: {
      supported: state.renderedSupported,
      mime_type: state.renderedMimeType,
      retained_bytes: state.renderedBytes,
      truncated: state.renderedTruncated,
      error: state.renderedError,
      chunk_boundaries_dropped: state.renderedBoundariesDropped,
      chunks: state.renderedBoundaries.map((boundary) => ({ ...boundary })),
    },
    interruptions: state.interruptions.map((boundary) => ({ ...boundary })),
  };
}

export function livePCMArtifactMetadata(): LivePCMArtifactMetadata | null {
  return retainedCapture ? artifactMetadata(retainedCapture) : null;
}

function status(): LivePCMArtifactStatus {
  const state = retainedCapture;
  return {
    active: state?.active || false,
    available: state !== null,
    live_id: state?.liveId || '',
    incoming_pcm_bytes: state?.incomingRetainedBytes || 0,
    browser_rendered_bytes: state?.renderedBytes || 0,
    browser_rendered_supported: state?.renderedSupported || false,
    browser_rendered_mime_type: state?.renderedMimeType || '',
    stopped: state !== null && !state.active,
  };
}

function triggerDownload(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;
  anchor.hidden = true;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), DOWNLOAD_URL_REVOKE_DELAY_MS);
}

export async function downloadLivePCMArtifacts(): Promise<string[]> {
  const state = retainedCapture;
  if (!state) throw new Error('No live PCM artifact capture is available.');
  if (state.active) activeCapture?.stopIfRetaining(state, 'artifact_download');
  await state.renderedFinalization.promise;

  const stamp = state.startedAtISO.replace(/[:.]/g, '-');
  const prefix = `term-llm-live-pcm-${stamp}`;
  const files: string[] = [];

  const wavName = `${prefix}-provider-mono24k.wav`;
  triggerDownload(buildPCM16WAV(retainedPCMChunks(state)), wavName);
  files.push(wavName);

  if (state.renderedChunks.length > 0) {
    const renderedName = `${prefix}-browser-rendered.${renderedExtension(state.renderedMimeType)}`;
    triggerDownload(
      new Blob(state.renderedChunks, {
        type: state.renderedMimeType || 'application/octet-stream',
      }),
      renderedName,
    );
    files.push(renderedName);
  }

  const metadataName = `${prefix}-timings.json`;
  triggerDownload(
    new Blob([JSON.stringify(artifactMetadata(state), null, 2)], { type: 'application/json' }),
    metadataName,
  );
  files.push(metadataName);
  return files;
}

export function clearLivePCMArtifacts(): void {
  activeCapture?.stop('cleared');
  activeCapture = null;
  retainedCapture = null;
}

if (typeof window !== 'undefined') {
  window.termLLMLivePCMArtifacts = {
    download: downloadLivePCMArtifacts,
    status,
    clear: clearLivePCMArtifacts,
  };
}

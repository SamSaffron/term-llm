export type VoicePhase =
  | 'idle'
  | 'requesting-permission'
  | 'recording'
  | 'preparing'
  | 'transcribing'
  | 'complete'
  | 'cancelled'
  | 'failed';

export interface VoiceCapability {
  supported: boolean;
  reason: string;
}

export interface VoiceSnapshot {
  phase: VoicePhase;
  capability: VoiceCapability;
  generation: number;
  owner: string;
  ownerRevision: number;
  durationMs: number;
  bytes: number;
  stage?: 'uploading' | 'processing' | 'stalled';
  loaded?: number;
  total?: number;
  elapsedMs?: number;
  transcript?: string;
  error?: string;
  retryable?: boolean;
  retainedBlob?: boolean;
}

export interface VoiceUploadControls {
  signal?: AbortSignal;
  onProgress?: (loaded: number, total?: number) => void;
}

export type VoiceUpload = (
  form: FormData,
  controls: VoiceUploadControls,
) => Promise<Record<string, unknown>>;

const MAX_RECORDING_BYTES = 25 * 1024 * 1024;
const RECORDING_STOP_BYTES = MAX_RECORDING_BYTES - 256 * 1024;
const FINALIZATION_TIMEOUT_MS = 5_000;
const STALL_TIMEOUT_MS = 15_000;
const OVERALL_TIMEOUT_MS = 120_000;

export const VOICE_MIME_CANDIDATES = [
  'audio/mp4;codecs=mp4a.40.2',
  'audio/mp4',
  'audio/webm;codecs=opus',
  'audio/webm',
  'audio/ogg;codecs=opus',
  'audio/ogg',
];

export function voiceCapability(): VoiceCapability {
  if (!globalThis.isSecureContext)
    return { supported: false, reason: 'Voice recording requires a secure HTTPS connection.' };
  if (!navigator.mediaDevices?.getUserMedia)
    return { supported: false, reason: 'This browser cannot access a microphone.' };
  if (!('MediaRecorder' in window))
    return { supported: false, reason: 'This browser cannot record microphone audio.' };
  if (
    typeof MediaRecorder.prototype.start !== 'function' ||
    typeof MediaRecorder.prototype.stop !== 'function'
  )
    return { supported: false, reason: 'This browser has an incomplete audio recorder.' };
  return { supported: true, reason: '' };
}

export function selectVoiceMIME(): string {
  if (typeof MediaRecorder === 'undefined') return '';
  if (typeof MediaRecorder.isTypeSupported !== 'function') return '';
  return VOICE_MIME_CANDIDATES.find((type) => MediaRecorder.isTypeSupported(type)) || '';
}

export function voiceFilename(type: string): string {
  const mediaType = type.split(';', 1)[0].trim().toLowerCase();
  if (mediaType === 'audio/mp4' || mediaType === 'audio/x-m4a' || mediaType === 'audio/m4a')
    return mediaType === 'audio/mp4' ? 'recording.mp4' : 'recording.m4a';
  if (mediaType === 'audio/ogg') return 'recording.ogg';
  if (mediaType === 'audio/wav' || mediaType === 'audio/x-wav') return 'recording.wav';
  return 'recording.webm';
}

function voiceError(error: unknown): { message: string; retryable: boolean } {
  const name = (error as { name?: string } | null)?.name || '';
  if (name === 'NotAllowedError' || name === 'SecurityError')
    return {
      message: 'Microphone access was denied. Enable it in browser or system settings.',
      retryable: true,
    };
  if (name === 'NotFoundError' || name === 'DevicesNotFoundError')
    return { message: 'No microphone was found.', retryable: true };
  if (name === 'AbortError') return { message: 'Recording cancelled.', retryable: false };
  if (name === 'TimeoutError')
    return { message: 'Transcription timed out. Retry the prepared recording.', retryable: true };
  return {
    message:
      error instanceof Error && error.message ? error.message : 'Voice transcription failed.',
    retryable: true,
  };
}

export class VoiceOperation {
  private snapshotValue: VoiceSnapshot;
  private listener: (snapshot: VoiceSnapshot) => void = () => {};
  private stream: MediaStream | null = null;
  private recorder: MediaRecorder | null = null;
  private chunks: Blob[] = [];
  private prepared: Blob | null = null;
  private preparedType = '';
  private abort: AbortController | null = null;
  private durationTimer = 0;
  private elapsedTimer = 0;
  private finalizationTimer = 0;
  private stallTimer = 0;
  private overallTimer = 0;
  private startedAt = 0;
  private transcribeStartedAt = 0;
  private stoppedTracks = new WeakSet<MediaStreamTrack>();
  private disposed = false;

  constructor(private readonly upload: VoiceUpload) {
    this.snapshotValue = {
      phase: 'idle',
      capability: voiceCapability(),
      generation: 0,
      owner: '',
      ownerRevision: 0,
      durationMs: 0,
      bytes: 0,
    };
  }

  get snapshot(): VoiceSnapshot {
    return this.snapshotValue;
  }

  subscribe(listener: (snapshot: VoiceSnapshot) => void): () => void {
    this.listener = listener;
    listener(this.snapshotValue);
    return () => {
      if (this.listener === listener) this.listener = () => {};
    };
  }

  private update(patch: Partial<VoiceSnapshot>, generation = this.snapshotValue.generation): void {
    if (this.disposed || generation !== this.snapshotValue.generation) return;
    this.snapshotValue = { ...this.snapshotValue, ...patch };
    this.listener(this.snapshotValue);
  }

  async start(owner: string, ownerRevision: number): Promise<void> {
    if (this.snapshotValue.phase !== 'idle' && this.snapshotValue.phase !== 'failed') return;
    if (!this.snapshotValue.capability.supported) {
      this.update({
        phase: 'failed',
        error: this.snapshotValue.capability.reason,
        retryable: false,
      });
      return;
    }
    this.cleanupOperation(false);
    const generation = this.snapshotValue.generation + 1;
    this.snapshotValue = {
      phase: 'requesting-permission',
      capability: voiceCapability(),
      generation,
      owner,
      ownerRevision,
      durationMs: 0,
      bytes: 0,
    };
    this.listener(this.snapshotValue);
    this.prepared = null;
    this.chunks = [];
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      if (this.disposed || generation !== this.snapshotValue.generation) {
        stream.getTracks().forEach((track) => track.stop());
        return;
      }
      this.stream = stream;
      const selectedType = selectVoiceMIME();
      const recorder = selectedType
        ? new MediaRecorder(stream, { mimeType: selectedType })
        : new MediaRecorder(stream);
      this.recorder = recorder;
      this.preparedType = recorder.mimeType || selectedType || 'audio/webm';
      recorder.addEventListener('dataavailable', (event) => this.onChunk(event, generation));
      recorder.addEventListener('stop', () => this.finalize(generation));
      recorder.addEventListener('error', (event) => {
        const recorderError = (event as Event & { error?: DOMException }).error;
        this.fail(
          recorderError || new Error('The audio recorder stopped unexpectedly.'),
          false,
          generation,
        );
      });
      stream.getTracks().forEach((track) => {
        track.addEventListener('ended', () => {
          if (generation !== this.snapshotValue.generation) return;
          if (this.snapshotValue.phase === 'recording') this.stop();
        });
      });
      recorder.start(500);
      this.startedAt = performance.now();
      this.update({ phase: 'recording', durationMs: 0 }, generation);
      this.durationTimer = window.setInterval(() => {
        this.update({ durationMs: Math.max(0, performance.now() - this.startedAt) }, generation);
      }, 1_000);
    } catch (error) {
      this.stopTracks();
      this.fail(error, false, generation);
    }
  }

  private onChunk(event: BlobEvent, generation: number): void {
    if (generation !== this.snapshotValue.generation || !event.data?.size) return;
    this.chunks.push(event.data);
    const bytes = this.chunks.reduce((sum, chunk) => sum + chunk.size, 0);
    this.update({ bytes }, generation);
    if (bytes >= RECORDING_STOP_BYTES && this.snapshotValue.phase === 'recording') this.stop();
  }

  stop(): void {
    if (this.snapshotValue.phase !== 'recording') return;
    const generation = this.snapshotValue.generation;
    this.update({ phase: 'preparing' }, generation);
    this.clearDurationTimer();
    try {
      if (this.recorder?.state !== 'inactive') {
        this.recorder?.requestData?.();
        this.recorder?.stop();
      } else {
        this.finalize(generation);
      }
    } catch (error) {
      this.fail(error, false, generation);
      return;
    }
    this.finalizationTimer = window.setTimeout(() => {
      this.fail(new Error('The browser did not finish the recording.'), false, generation);
    }, FINALIZATION_TIMEOUT_MS);
  }

  private finalize(generation: number): void {
    if (generation !== this.snapshotValue.generation || this.snapshotValue.phase !== 'preparing')
      return;
    window.clearTimeout(this.finalizationTimer);
    this.finalizationTimer = 0;
    this.stopTracks();
    const type =
      this.recorder?.mimeType || this.preparedType || this.chunks[0]?.type || 'audio/webm';
    const blob = new Blob(this.chunks, { type });
    if (!blob.size) {
      this.fail(
        new Error('The recording was empty. Try again and speak after recording starts.'),
        false,
        generation,
      );
      return;
    }
    if (blob.size > MAX_RECORDING_BYTES) {
      this.fail(
        new Error('The recording is too large. Record a shorter message.'),
        false,
        generation,
      );
      return;
    }
    this.prepared = blob;
    this.preparedType = blob.type || type;
    void this.transcribe(generation);
  }

  private async transcribe(generation: number): Promise<void> {
    const blob = this.prepared;
    if (!blob || !blob.size || generation !== this.snapshotValue.generation) return;
    this.abort?.abort();
    this.abort = new AbortController();
    const form = new FormData();
    form.append('file', blob, voiceFilename(this.preparedType));
    this.transcribeStartedAt = performance.now();
    this.update(
      {
        phase: 'transcribing',
        stage: 'uploading',
        loaded: 0,
        total: blob.size,
        elapsedMs: 0,
        error: undefined,
        retryable: undefined,
        retainedBlob: true,
      },
      generation,
    );
    this.elapsedTimer = window.setInterval(() => {
      this.update({ elapsedMs: performance.now() - this.transcribeStartedAt }, generation);
    }, 1_000);
    this.overallTimer = window.setTimeout(() => {
      this.abort?.abort(new DOMException('Transcription timed out', 'TimeoutError'));
    }, OVERALL_TIMEOUT_MS);
    const resetStall = () => {
      window.clearTimeout(this.stallTimer);
      this.stallTimer = window.setTimeout(() => {
        if (generation !== this.snapshotValue.generation) return;
        this.abort?.abort(new DOMException('Upload stalled', 'TimeoutError'));
        this.update({ stage: 'stalled' }, generation);
      }, STALL_TIMEOUT_MS);
    };
    resetStall();
    try {
      const result = await this.upload(form, {
        signal: this.abort.signal,
        onProgress: (loaded, total) => {
          if (generation !== this.snapshotValue.generation) return;
          resetStall();
          const uploadTotal = total || blob.size;
          this.update(
            {
              loaded,
              total: uploadTotal,
              stage: loaded >= uploadTotal ? 'processing' : 'uploading',
            },
            generation,
          );
          if (loaded >= uploadTotal) window.clearTimeout(this.stallTimer);
        },
      });
      if (generation !== this.snapshotValue.generation) return;
      const transcript = String(result.text || '').trim();
      if (!transcript) throw new Error('The transcription was empty. Retry or record again.');
      this.clearTranscribeTimers();
      this.prepared = null;
      this.update(
        {
          phase: 'complete',
          transcript,
          stage: undefined,
          error: undefined,
          retryable: false,
          retainedBlob: false,
        },
        generation,
      );
    } catch (error) {
      if (generation !== this.snapshotValue.generation) return;
      this.clearTranscribeTimers();
      if (
        (error as { name?: string })?.name === 'AbortError' &&
        this.snapshotValue.phase === 'cancelled'
      )
        return;
      this.fail(error, true, generation);
    }
  }

  retry(): void {
    if (this.snapshotValue.phase !== 'failed' || !this.snapshotValue.retryable) return;
    if (this.prepared?.size) {
      const generation = this.snapshotValue.generation + 1;
      this.snapshotValue = {
        ...this.snapshotValue,
        generation,
        phase: 'preparing',
        error: undefined,
      };
      this.listener(this.snapshotValue);
      void this.transcribe(generation);
      return;
    }
    void this.start(this.snapshotValue.owner, this.snapshotValue.ownerRevision);
  }

  cancel(): void {
    if (this.snapshotValue.phase === 'idle' || this.snapshotValue.phase === 'cancelled') return;
    const generation = this.snapshotValue.generation + 1;
    this.cleanupOperation(false);
    this.prepared = null;
    this.chunks = [];
    this.snapshotValue = {
      ...this.snapshotValue,
      generation,
      phase: 'cancelled',
      transcript: undefined,
      error: undefined,
      retryable: false,
      retainedBlob: false,
      stage: undefined,
    };
    this.listener(this.snapshotValue);
  }

  discard(): void {
    this.cancel();
  }

  settle(): void {
    if (this.snapshotValue.phase !== 'complete' && this.snapshotValue.phase !== 'cancelled') return;
    this.snapshotValue = {
      phase: 'idle',
      capability: voiceCapability(),
      generation: this.snapshotValue.generation,
      owner: '',
      ownerRevision: 0,
      durationMs: 0,
      bytes: 0,
    };
    this.listener(this.snapshotValue);
  }

  private fail(error: unknown, retainedBlob: boolean, generation: number): void {
    if (generation !== this.snapshotValue.generation) return;
    this.clearDurationTimer();
    this.clearTranscribeTimers();
    this.stopTracks();
    const classified = voiceError(error);
    this.update(
      {
        phase: 'failed',
        error: classified.message,
        retryable: classified.retryable,
        retainedBlob: retainedBlob && Boolean(this.prepared?.size),
        stage: undefined,
      },
      generation,
    );
  }

  private stopTracks(): void {
    this.stream?.getTracks().forEach((track) => {
      if (this.stoppedTracks.has(track)) return;
      this.stoppedTracks.add(track);
      track.stop();
    });
    this.stream = null;
  }

  private clearDurationTimer(): void {
    window.clearInterval(this.durationTimer);
    this.durationTimer = 0;
  }

  private clearTranscribeTimers(): void {
    window.clearInterval(this.elapsedTimer);
    window.clearTimeout(this.stallTimer);
    window.clearTimeout(this.overallTimer);
    this.elapsedTimer = this.stallTimer = this.overallTimer = 0;
  }

  private cleanupOperation(abortAsCancel: boolean): void {
    this.clearDurationTimer();
    this.clearTranscribeTimers();
    window.clearTimeout(this.finalizationTimer);
    this.finalizationTimer = 0;
    if (this.recorder?.state !== 'inactive') {
      try {
        this.recorder?.stop();
      } catch {
        /* Recorder cleanup is best effort. */
      }
    }
    if (abortAsCancel) this.abort?.abort(new DOMException('Upload cancelled', 'AbortError'));
    else this.abort?.abort();
    this.abort = null;
    this.stopTracks();
    this.recorder = null;
  }

  dispose(): void {
    if (this.disposed) return;
    this.cleanupOperation(true);
    this.disposed = true;
    this.listener = () => {};
  }
}

// Compatibility alias for integrations that imported the low-level Preact-era name.
export { VoiceOperation as VoiceRecorder };

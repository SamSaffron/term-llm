import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  LIVE_PCM_CAPTURE_MAX_BYTES,
  LIVE_PCM_CAPTURE_MAX_MS,
  LIVE_PCM_RENDERED_MAX_BYTES,
  buildPCM16WAV,
  clearLivePCMArtifacts,
  downloadLivePCMArtifacts,
  livePCMArtifactMetadata,
  startLivePCMArtifactCapture,
} from './live-pcm-diagnostics';

class FakeMediaRecorder {
  static instances: FakeMediaRecorder[] = [];
  static finalizeOnStop = true;
  static isTypeSupported = vi.fn((mime: string) => mime === 'audio/webm;codecs=opus');

  state: RecordingState = 'inactive';
  readonly mimeType: string;
  ondataavailable: ((this: MediaRecorder, event: BlobEvent) => unknown) | null = null;
  onerror: ((this: MediaRecorder, event: Event) => unknown) | null = null;
  onstop: ((this: MediaRecorder, event: Event) => unknown) | null = null;
  readonly start = vi.fn((_timeslice?: number) => {
    this.state = 'recording';
  });
  readonly stop = vi.fn(() => {
    this.state = 'inactive';
    if (FakeMediaRecorder.finalizeOnStop) queueMicrotask(() => this.finish());
  });

  constructor(
    readonly stream: MediaStream,
    options?: MediaRecorderOptions,
  ) {
    this.mimeType = options?.mimeType || 'audio/webm';
    FakeMediaRecorder.instances.push(this);
  }

  emit(data: Blob): void {
    this.ondataavailable?.call(this as unknown as MediaRecorder, { data } as unknown as BlobEvent);
  }

  finish(finalData?: Blob): void {
    if (finalData) this.emit(finalData);
    this.onstop?.call(this as unknown as MediaRecorder, new Event('stop'));
  }
}

function fakeContext() {
  const track = { stop: vi.fn() };
  const destination = {
    stream: { getTracks: () => [track] },
    disconnect: vi.fn(),
  };
  const context = {
    createMediaStreamDestination: vi.fn(
      () => destination as unknown as MediaStreamAudioDestinationNode,
    ),
  } as unknown as AudioContext;
  return { context, destination, track };
}

function mockArtifactDownloads(): { blobs: Blob[]; revokeObjectURL: ReturnType<typeof vi.fn> } {
  const blobs: Blob[] = [];
  vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
    blobs.push(blob as Blob);
    return `blob:artifact-${blobs.length}`;
  });
  const revokeObjectURL = vi.fn();
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(revokeObjectURL);
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
  return { blobs, revokeObjectURL };
}

beforeEach(() => {
  FakeMediaRecorder.instances = [];
  FakeMediaRecorder.finalizeOnStop = true;
  vi.stubGlobal('MediaRecorder', FakeMediaRecorder);
});

afterEach(() => {
  clearLivePCMArtifacts();
  for (const recorder of FakeMediaRecorder.instances) recorder.finish();
  vi.clearAllTimers();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('live PCM artifact diagnostics', () => {
  it('writes an exact mono 24 kHz PCM16 WAV', async () => {
    const wav = buildPCM16WAV([
      new Uint8Array([0x34, 0x12, 0x00, 0x80]),
      new Uint8Array([0xff, 0x7f]),
    ]);
    const bytes = new Uint8Array(await wav.arrayBuffer());
    const view = new DataView(bytes.buffer);

    expect(new TextDecoder().decode(bytes.subarray(0, 4))).toBe('RIFF');
    expect(view.getUint32(4, true)).toBe(42);
    expect(new TextDecoder().decode(bytes.subarray(8, 12))).toBe('WAVE');
    expect(view.getUint16(20, true)).toBe(1);
    expect(view.getUint16(22, true)).toBe(1);
    expect(view.getUint32(24, true)).toBe(24_000);
    expect(view.getUint32(28, true)).toBe(48_000);
    expect(view.getUint16(34, true)).toBe(16);
    expect(view.getUint32(40, true)).toBe(6);
    expect([...bytes.subarray(44)]).toEqual([0x34, 0x12, 0x00, 0x80, 0xff, 0x7f]);
  });

  it('preserves stopped capture for the user-invoked DevTools download API', async () => {
    vi.stubGlobal('MediaRecorder', undefined);
    const { context } = fakeContext();
    const capture = startLivePCMArtifactCapture(context, 'live-download');
    capture.captureIncomingPCM(new Int16Array([0x1234, -2]).buffer, 1, 1.01);
    capture.stop('call_cleanup');
    const { blobs } = mockArtifactDownloads();

    expect(window.termLLMLivePCMArtifacts?.status()).toMatchObject({
      active: false,
      available: true,
      live_id: 'live-download',
      incoming_pcm_bytes: 4,
      stopped: true,
    });
    const files = await downloadLivePCMArtifacts();

    expect(files).toHaveLength(2);
    expect(files[0]).toMatch(/-provider-mono24k\.wav$/);
    expect(files[1]).toMatch(/-timings\.json$/);
    expect(blobs.map((blob) => blob.type)).toEqual(['audio/wav', 'application/json']);
    const wav = new Uint8Array(await blobs[0].arrayBuffer());
    expect([...wav.subarray(44)]).toEqual([0x34, 0x12, 0xfe, 0xff]);
    expect(JSON.parse(await blobs[1].text())).toMatchObject({
      live_id: 'live-download',
      stop_reason: 'call_cleanup',
      incoming_pcm: { retained_bytes: 4 },
    });
  });

  it('waits for asynchronous final recorder data when downloading an active capture', async () => {
    FakeMediaRecorder.finalizeOnStop = false;
    const { context, destination, track } = fakeContext();
    const capture = startLivePCMArtifactCapture(context, 'live-active-download');
    capture.captureIncomingPCM(new Int16Array([11, -12]).buffer, 1, 1.01);
    const recorder = FakeMediaRecorder.instances[0];
    const { blobs } = mockArtifactDownloads();

    const download = downloadLivePCMArtifacts();

    expect(recorder.stop).toHaveBeenCalledOnce();
    expect(track.stop).toHaveBeenCalledOnce();
    expect(destination.disconnect).toHaveBeenCalledOnce();
    expect(blobs).toEqual([]);
    expect(livePCMArtifactMetadata()).toMatchObject({
      live_id: 'live-active-download',
      stop_reason: 'artifact_download',
      browser_rendered: { retained_bytes: 0 },
    });

    recorder.finish(new Blob([new Uint8Array([1, 2, 3])], { type: 'audio/webm' }));
    const files = await download;

    expect(files).toHaveLength(3);
    expect(blobs.map((blob) => blob.type)).toEqual([
      'audio/wav',
      'audio/webm;codecs=opus',
      'application/json',
    ]);
    expect([...new Uint8Array(await blobs[1].arrayBuffer())]).toEqual([1, 2, 3]);
    expect(JSON.parse(await blobs[2].text())).toMatchObject({
      live_id: 'live-active-download',
      stop_reason: 'artifact_download',
      browser_rendered: { retained_bytes: 3 },
    });
  });

  it('includes recorder tail data when export starts immediately after stop', async () => {
    FakeMediaRecorder.finalizeOnStop = false;
    const { context } = fakeContext();
    const capture = startLivePCMArtifactCapture(context, 'live-immediate-export');
    const recorder = FakeMediaRecorder.instances[0];
    recorder.emit(new Blob([new Uint8Array([4])], { type: 'audio/webm' }));
    capture.stop('call_cleanup');
    const { blobs } = mockArtifactDownloads();

    const download = downloadLivePCMArtifacts();
    expect(blobs).toEqual([]);

    recorder.finish(new Blob([new Uint8Array([5, 6])], { type: 'audio/webm' }));
    await download;

    expect([...new Uint8Array(await blobs[1].arrayBuffer())]).toEqual([4, 5, 6]);
    expect(JSON.parse(await blobs[2].text())).toMatchObject({
      live_id: 'live-immediate-export',
      browser_rendered: { retained_bytes: 3 },
    });
  });

  it('downloads without waiting when MediaRecorder is unavailable and revokes URLs later', async () => {
    vi.useFakeTimers();
    vi.stubGlobal('MediaRecorder', undefined);
    const { context } = fakeContext();
    startLivePCMArtifactCapture(context, 'live-no-recorder-download');
    const { blobs, revokeObjectURL } = mockArtifactDownloads();

    const files = await downloadLivePCMArtifacts();

    expect(files).toHaveLength(2);
    expect(blobs).toHaveLength(2);
    expect(revokeObjectURL).not.toHaveBeenCalled();
    expect(livePCMArtifactMetadata()).toMatchObject({
      stop_reason: 'artifact_download',
      browser_rendered: { supported: false, error: 'MediaRecorder is unavailable.' },
    });

    await vi.runAllTimersAsync();
    expect(revokeObjectURL).toHaveBeenCalledTimes(2);
  });

  it('exports the requested capture consistently if a replacement starts while finalizing', async () => {
    FakeMediaRecorder.finalizeOnStop = false;
    const first = fakeContext();
    startLivePCMArtifactCapture(first.context, 'live-exported');
    const firstRecorder = FakeMediaRecorder.instances[0];
    const { blobs } = mockArtifactDownloads();
    const download = downloadLivePCMArtifacts();

    const second = fakeContext();
    const secondCapture = startLivePCMArtifactCapture(second.context, 'live-latest');
    const secondRecorder = FakeMediaRecorder.instances[1];
    firstRecorder.finish(new Blob([new Uint8Array([7, 8])], { type: 'audio/webm' }));
    await download;

    expect(JSON.parse(await blobs[2].text())).toMatchObject({
      live_id: 'live-exported',
      stop_reason: 'artifact_download',
      browser_rendered: { retained_bytes: 2 },
    });
    expect(livePCMArtifactMetadata()).toMatchObject({ live_id: 'live-latest', stopped_at: null });
    expect(secondRecorder.stop).not.toHaveBeenCalled();

    secondCapture.stop();
    secondRecorder.finish();
  });

  it('finishes an in-flight export consistently after artifacts are cleared', async () => {
    FakeMediaRecorder.finalizeOnStop = false;
    const { context } = fakeContext();
    startLivePCMArtifactCapture(context, 'live-cleared-during-export');
    const recorder = FakeMediaRecorder.instances[0];
    const { blobs } = mockArtifactDownloads();
    const download = downloadLivePCMArtifacts();

    clearLivePCMArtifacts();
    expect(livePCMArtifactMetadata()).toBeNull();
    recorder.finish(new Blob([new Uint8Array([9])], { type: 'audio/webm' }));
    await download;

    expect(JSON.parse(await blobs[2].text())).toMatchObject({
      live_id: 'live-cleared-during-export',
      stop_reason: 'artifact_download',
      browser_rendered: { retained_bytes: 1 },
    });
    expect(livePCMArtifactMetadata()).toBeNull();
  });

  it('bounds finalization if the recorder never dispatches stop', async () => {
    vi.useFakeTimers();
    FakeMediaRecorder.finalizeOnStop = false;
    const { context, destination, track } = fakeContext();
    startLivePCMArtifactCapture(context, 'live-finalization-timeout');
    const recorder = FakeMediaRecorder.instances[0];
    const { blobs } = mockArtifactDownloads();
    const download = downloadLivePCMArtifacts();

    await vi.advanceTimersToNextTimerAsync();
    const files = await download;

    expect(files).toHaveLength(2);
    expect(track.stop).toHaveBeenCalledOnce();
    expect(destination.disconnect).toHaveBeenCalledOnce();
    expect(recorder.ondataavailable).toBeNull();
    expect(recorder.onstop).toBeNull();
    expect(JSON.parse(await blobs[1].text())).toMatchObject({
      live_id: 'live-finalization-timeout',
      browser_rendered: {
        retained_bytes: 0,
        error: 'Timed out waiting for browser-rendered recording to finalize.',
      },
    });

    recorder.finish(new Blob([new Uint8Array([10])], { type: 'audio/webm' }));
    expect(livePCMArtifactMetadata()?.browser_rendered.retained_bytes).toBe(0);
  });

  it('bounds provider PCM while retaining useful sample and chunk details', () => {
    vi.stubGlobal('MediaRecorder', undefined);
    const { context } = fakeContext();
    const capture = startLivePCMArtifactCapture(context, 'live-bounded');
    const pcm = new Int16Array(LIVE_PCM_CAPTURE_MAX_BYTES / 2 + 2);
    pcm[0] = -123;
    pcm[1] = 456;
    pcm[pcm.length - 1] = 789;

    capture.captureIncomingPCM(pcm.buffer, 4, 4.01);

    expect(livePCMArtifactMetadata()).toMatchObject({
      limits: {
        duration_ms: 60_000,
        incoming_pcm_bytes: LIVE_PCM_CAPTURE_MAX_BYTES,
      },
      incoming_pcm: {
        received_bytes: LIVE_PCM_CAPTURE_MAX_BYTES + 4,
        retained_bytes: LIVE_PCM_CAPTURE_MAX_BYTES,
        retained_samples: LIVE_PCM_CAPTURE_MAX_BYTES / 2,
        truncated: true,
        chunks_received: 1,
        min_sample: -123,
        max_sample: 456,
        leading_samples: [-123, 456, 0, 0, 0, 0, 0, 0],
      },
      browser_rendered: {
        supported: false,
        error: 'MediaRecorder is unavailable.',
      },
    });
    expect(livePCMArtifactMetadata()?.incoming_pcm.chunks[0]).toMatchObject({
      context_time_seconds: 4,
      scheduled_at_context_seconds: 4.01,
      received_bytes: LIVE_PCM_CAPTURE_MAX_BYTES + 4,
      retained_bytes: LIVE_PCM_CAPTURE_MAX_BYTES,
    });
  });

  it('records only the provided playback destination and cleans recorder resources on stop', () => {
    const { context, destination, track } = fakeContext();
    const capture = startLivePCMArtifactCapture(context, 'live-rendered');
    const recorder = FakeMediaRecorder.instances[0];

    expect(capture.renderedDestination).toBe(destination);
    expect(recorder.stream).toBe(destination.stream);
    expect(recorder.start).toHaveBeenCalledWith(1_000);
    recorder.emit(new Blob([new Uint8Array([1, 2, 3])]));
    capture.captureIncomingPCM(new Int16Array([-32_768, 32_767]).buffer, 1, 1.01);
    capture.captureInterrupt(1.02);
    capture.stop('test_stop');

    expect(recorder.stop).toHaveBeenCalledOnce();
    expect(track.stop).toHaveBeenCalledOnce();
    expect(destination.disconnect).toHaveBeenCalledOnce();
    expect(capture.renderedDestination).toBeNull();
    expect(livePCMArtifactMetadata()).toMatchObject({
      stop_reason: 'test_stop',
      incoming_pcm: {
        min_sample: -32_768,
        max_sample: 32_767,
        trailing_samples: [-32_768, 32_767],
      },
      browser_rendered: {
        supported: true,
        mime_type: 'audio/webm;codecs=opus',
        retained_bytes: 3,
      },
      interruptions: [{ retained_pcm_byte_offset: 4, context_time_seconds: 1.02 }],
    });
  });

  it('stops browser rendering capture before retaining data beyond its byte cap', () => {
    const { context, destination, track } = fakeContext();
    startLivePCMArtifactCapture(context, 'live-rendered-cap');
    const recorder = FakeMediaRecorder.instances[0];

    recorder.emit({
      size: LIVE_PCM_RENDERED_MAX_BYTES + 1,
      type: 'audio/webm',
    } as Blob);

    expect(recorder.stop).toHaveBeenCalledOnce();
    expect(track.stop).toHaveBeenCalledOnce();
    expect(destination.disconnect).toHaveBeenCalledOnce();
    expect(livePCMArtifactMetadata()).toMatchObject({
      browser_rendered: {
        retained_bytes: 0,
        truncated: true,
        chunk_boundaries_dropped: 1,
        chunks: [],
      },
    });
  });

  it('stops at sixty seconds and replaces rather than accumulating captures', () => {
    vi.useFakeTimers();
    const first = fakeContext();
    startLivePCMArtifactCapture(first.context, 'live-first');
    const firstRecorder = FakeMediaRecorder.instances[0];
    const second = fakeContext();

    startLivePCMArtifactCapture(second.context, 'live-second');
    expect(firstRecorder.stop).toHaveBeenCalledOnce();
    expect(first.track.stop).toHaveBeenCalledOnce();
    expect(livePCMArtifactMetadata()?.live_id).toBe('live-second');

    vi.advanceTimersByTime(LIVE_PCM_CAPTURE_MAX_MS);
    expect(FakeMediaRecorder.instances[1].stop).toHaveBeenCalledOnce();
    expect(second.track.stop).toHaveBeenCalledOnce();
    expect(livePCMArtifactMetadata()).toMatchObject({
      live_id: 'live-second',
      stop_reason: 'duration_limit',
    });
  });

  it('keeps provider capture usable when browser-rendered recording is unsupported', () => {
    vi.stubGlobal('MediaRecorder', undefined);
    const { context } = fakeContext();

    const capture = startLivePCMArtifactCapture(context, 'live-no-recorder');
    expect(capture.renderedDestination).toBeNull();
    capture.captureIncomingPCM(new Int16Array([7, -8]).buffer, 0, 0.01);
    capture.stop();

    expect(livePCMArtifactMetadata()).toMatchObject({
      incoming_pcm: { retained_bytes: 4, leading_samples: [7, -8] },
      browser_rendered: {
        supported: false,
        retained_bytes: 0,
        error: 'MediaRecorder is unavailable.',
      },
    });
  });
});

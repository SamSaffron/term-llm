import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { VoiceOperation, selectVoiceMIME, voiceCapability, voiceFilename } from './voice';

class FakeRecorder extends EventTarget {
  static supported: string[] = [];
  static instances: FakeRecorder[] = [];
  static isTypeSupported(type: string) {
    return this.supported.includes(type);
  }
  state: RecordingState = 'inactive';
  mimeType: string;
  constructor(
    readonly stream: MediaStream,
    options?: MediaRecorderOptions,
  ) {
    super();
    this.mimeType = options?.mimeType || 'audio/webm';
    FakeRecorder.instances.push(this);
  }
  start() {
    this.state = 'recording';
  }
  requestData() {
    const event = new Event('dataavailable') as BlobEvent;
    Object.defineProperty(event, 'data', { value: new Blob(['voice'], { type: this.mimeType }) });
    this.dispatchEvent(event);
  }
  stop() {
    this.state = 'inactive';
    queueMicrotask(() => this.dispatchEvent(new Event('stop')));
  }
}

const track = () => {
  const target = new EventTarget() as MediaStreamTrack;
  Object.defineProperty(target, 'stop', { value: vi.fn() });
  return target;
};

const stream = (mediaTrack = track()) =>
  ({ getTracks: () => [mediaTrack] }) as unknown as MediaStream;

beforeEach(() => {
  FakeRecorder.instances = [];
  FakeRecorder.supported = ['audio/mp4;codecs=mp4a.40.2', 'audio/webm;codecs=opus'];
  Object.defineProperty(globalThis, 'isSecureContext', { configurable: true, value: true });
  Object.defineProperty(window, 'MediaRecorder', { configurable: true, value: FakeRecorder });
  Object.defineProperty(globalThis, 'MediaRecorder', { configurable: true, value: FakeRecorder });
});

afterEach(() => vi.restoreAllMocks());

describe('voice platform operation', () => {
  it('reports capability prerequisites and negotiates Safari-compatible MP4 first', () => {
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn() },
    });
    expect(voiceCapability()).toEqual({ supported: true, reason: '' });
    expect(selectVoiceMIME()).toBe('audio/mp4;codecs=mp4a.40.2');
    expect(voiceFilename('audio/mp4;codecs=mp4a.40.2')).toBe('recording.mp4');
    expect(voiceFilename('audio/x-m4a')).toBe('recording.m4a');
    expect(voiceFilename('audio/webm;codecs=opus')).toBe('recording.webm');
    expect(voiceFilename('audio/ogg')).toBe('recording.ogg');
  });

  it('invalidates pending permission and stops a stream that arrives after cancellation', async () => {
    let resolvePermission!: (value: MediaStream) => void;
    const permission = new Promise<MediaStream>((resolve) => {
      resolvePermission = resolve;
    });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(() => permission) },
    });
    const upload = vi.fn();
    const operation = new VoiceOperation(upload);
    const start = operation.start('draft:one', 0);
    expect(operation.snapshot.phase).toBe('requesting-permission');
    operation.cancel();
    const mediaTrack = track();
    resolvePermission(stream(mediaTrack));
    await start;
    expect(mediaTrack.stop).toHaveBeenCalledOnce();
    expect(operation.snapshot.phase).toBe('cancelled');
    expect(upload).not.toHaveBeenCalled();
  });

  it('uploads a MIME-correct retained blob and completes with a non-empty transcript', async () => {
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => stream()) },
    });
    let uploadedName = '';
    const upload = vi.fn(
      async (form: FormData, controls: { onProgress?: (n: number, t: number) => void }) => {
        const file = form.get('file') as File;
        uploadedName = file.name;
        controls.onProgress?.(file.size, file.size);
        return { text: 'hello from voice' };
      },
    );
    const operation = new VoiceOperation(upload);
    await operation.start('session-one', 3);
    expect(operation.snapshot.phase).toBe('recording');
    operation.stop();
    await vi.waitFor(() => expect(operation.snapshot.phase).toBe('complete'));
    expect(uploadedName).toBe('recording.mp4');
    expect(operation.snapshot.transcript).toBe('hello from voice');
    operation.dispose();
  });
});

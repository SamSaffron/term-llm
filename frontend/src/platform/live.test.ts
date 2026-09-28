import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  clearLivePCMArtifacts,
  livePCMArtifactMetadata,
  type LivePCMArtifactCapture,
} from './live-pcm-diagnostics';
import { LiveCall, type LiveStartResponse } from './live';
import { liveCapability } from './voice';

class FakeDataChannel {
  onmessage: ((event: MessageEvent) => unknown) | null = null;
  close = vi.fn();
}

class FakePeerConnection extends EventTarget {
  static instances: FakePeerConnection[] = [];
  iceGatheringState: RTCIceGatheringState = 'complete';
  connectionState: RTCPeerConnectionState = 'new';
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;
  ontrack: ((event: RTCTrackEvent) => unknown) | null = null;
  onconnectionstatechange: (() => unknown) | null = null;
  readonly channel = new FakeDataChannel();
  readonly addTrack = vi.fn();
  readonly close = vi.fn();
  readonly createOffer = vi.fn(async () => ({ type: 'offer' as const, sdp: 'offer-sdp' }));
  readonly setLocalDescription = vi.fn(async (description: RTCSessionDescriptionInit) => {
    this.localDescription = description;
  });
  readonly setRemoteDescription = vi.fn(async (description: RTCSessionDescriptionInit) => {
    this.remoteDescription = description;
  });
  readonly createDataChannel = vi.fn(() => this.channel as unknown as RTCDataChannel);

  constructor() {
    super();
    FakePeerConnection.instances.push(this);
  }
}

class FakePCMWebSocket {
  static OPEN = 1;
  binaryType: BinaryType = 'blob';
  readyState = 0;
  bufferedAmount = 0;
  onopen: (() => unknown) | null = null;
  onmessage: ((event: MessageEvent) => unknown) | null = null;
  onerror: (() => unknown) | null = null;
  onclose: (() => unknown) | null = null;
  readonly send = vi.fn();
  readonly close = vi.fn(() => {
    this.readyState = 3;
  });

  open() {
    this.readyState = FakePCMWebSocket.OPEN;
    this.onopen?.();
  }

  message(data: string | ArrayBuffer) {
    this.onmessage?.(new MessageEvent('message', { data }));
  }
}

class FakeWorkletNode {
  readonly port = {
    onmessage: null as ((event: MessageEvent<unknown>) => unknown) | null,
    postMessage: vi.fn((_message: unknown, _transfer?: Transferable[]) => undefined),
    close: vi.fn(),
  };
  readonly connect = vi.fn();
  readonly disconnect = vi.fn();
}

class FakeBufferSource {
  buffer: AudioBuffer | null = null;
  onended: (() => unknown) | null = null;
  readonly connect = vi.fn();
  readonly disconnect = vi.fn();
  readonly start = vi.fn();
  readonly stop = vi.fn();
}

class FakeRenderedMediaRecorder {
  static isTypeSupported = vi.fn(() => true);
  state: RecordingState = 'inactive';
  readonly mimeType = 'audio/webm;codecs=opus';
  ondataavailable: ((this: MediaRecorder, event: BlobEvent) => unknown) | null = null;
  onerror: ((this: MediaRecorder, event: Event) => unknown) | null = null;
  onstop: ((this: MediaRecorder, event: Event) => unknown) | null = null;
  readonly start = vi.fn(() => {
    this.state = 'recording';
  });
  readonly stop = vi.fn(() => {
    this.state = 'inactive';
    this.onstop?.call(this as unknown as MediaRecorder, new Event('stop'));
  });
}

class FakeAudioContext {
  currentTime = 1;
  state: AudioContextState = 'running';
  readonly destination = {} as AudioDestinationNode;
  readonly audioWorklet = { addModule: vi.fn(async () => undefined) };
  readonly sources: FakeBufferSource[] = [];
  readonly mediaSource = { connect: vi.fn(), disconnect: vi.fn() };
  readonly gain = { gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() };
  readonly renderedTrack = { stop: vi.fn() };
  readonly renderedDestination = {
    stream: { getTracks: () => [this.renderedTrack] },
    disconnect: vi.fn(),
  };
  readonly resume = vi.fn(async (): Promise<void> => undefined);
  readonly close = vi.fn(async () => undefined);
  readonly createMediaStreamDestination = vi.fn(
    () => this.renderedDestination as unknown as MediaStreamAudioDestinationNode,
  );
  readonly createMediaStreamSource = vi.fn(
    () => this.mediaSource as unknown as MediaStreamAudioSourceNode,
  );
  readonly createGain = vi.fn(() => this.gain as unknown as GainNode);
  readonly createBuffer = vi.fn((_channels: number, samples: number, rate: number) => {
    const channel = new Float32Array(samples);
    return {
      duration: samples / rate,
      getChannelData: () => channel,
    } as unknown as AudioBuffer;
  });
  readonly createBufferSource = vi.fn(() => {
    const source = new FakeBufferSource();
    this.sources.push(source);
    return source as unknown as AudioBufferSourceNode;
  });
}

const mediaTrack = () => ({ stop: vi.fn() }) as unknown as MediaStreamTrack;
const mediaStream = (track = mediaTrack()) =>
  ({ getTracks: () => [track] }) as unknown as MediaStream;

function workletFactory(capture: FakeWorkletNode, playback: FakeWorkletNode) {
  return vi.fn(
    (_context: AudioContext, name: string) =>
      (name === 'term-llm-pcm-output' ? playback : capture) as unknown as AudioWorkletNode,
  );
}

function attachPlayback(
  call: LiveCall,
  context: FakeAudioContext,
  playback: FakeWorkletNode,
): void {
  const internals = call as unknown as {
    audioContext: AudioContext;
    playbackNode: AudioWorkletNode;
    playbackEpoch: number;
    onPlaybackEvent(
      event: {
        type: 'state' | 'overflow';
        epoch: number;
        consumedBytes?: number;
        consumedPackets?: number;
        consumedSamples?: number;
        queuedSamples?: number;
        underruns?: number;
      },
      node: AudioWorkletNode,
      generation: number,
    ): void;
  };
  internals.audioContext = context as unknown as AudioContext;
  internals.playbackNode = playback as unknown as AudioWorkletNode;
  internals.playbackEpoch = 1;
  const generation = call.snapshot.generation;
  playback.port.onmessage = (event) =>
    internals.onPlaybackEvent(
      event.data as Parameters<typeof internals.onPlaybackEvent>[0],
      playback as unknown as AudioWorkletNode,
      generation,
    );
}

beforeEach(() => {
  FakePeerConnection.instances = [];
  localStorage.removeItem('term-llm-live-pcm-capture');
  Object.defineProperty(globalThis, 'isSecureContext', { configurable: true, value: true });
  vi.stubGlobal('RTCPeerConnection', FakePeerConnection);
});

afterEach(() => {
  clearLivePCMArtifacts();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('LiveCall', () => {
  it('creates an offer, posts it, applies the answer, and starts listening', async () => {
    const stream = mediaStream();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => stream) },
    });
    const startEndpoint = vi.fn(async () => ({
      live_id: 'live_one',
      session_id: 'session-one',
      sdp: 'answer-sdp',
    }));
    const stopEndpoint = vi.fn(async (liveId: string) => ({
      live_id: liveId,
      status: 'ended' as const,
    }));
    const call = new LiveCall(startEndpoint, stopEndpoint);

    await expect(call.start('session-one')).resolves.toMatchObject({ live_id: 'live_one' });

    const peer = FakePeerConnection.instances[0];
    expect(peer.createDataChannel).toHaveBeenCalledWith('oai-events', { ordered: true });
    expect(peer.channel.onmessage).toBeNull();
    expect(peer.addTrack).toHaveBeenCalledWith(stream.getTracks()[0], stream);
    expect(startEndpoint).toHaveBeenCalledWith('offer-sdp', 'session-one');
    expect(peer.setRemoteDescription).toHaveBeenCalledWith({
      type: 'answer',
      sdp: 'answer-sdp',
    });
    expect(call.snapshot).toMatchObject({
      phase: 'listening',
      liveId: 'live_one',
      sessionId: 'session-one',
    });
    call.dispose();
  });

  it('posts the best available offer when ICE gathering reaches its timeout', async () => {
    vi.useFakeTimers();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => mediaStream()) },
    });
    const startEndpoint = vi.fn(async () => ({
      live_id: 'live_timeout',
      session_id: 'session-one',
      sdp: 'answer-sdp',
    }));
    const call = new LiveCall(
      startEndpoint,
      async (liveId) => ({ live_id: liveId, status: 'ended' }),
      {
        createPeerConnection: () => {
          const peer = new FakePeerConnection();
          peer.iceGatheringState = 'gathering';
          return peer as unknown as RTCPeerConnection;
        },
      },
    );

    const starting = call.start('session-one');
    await vi.advanceTimersByTimeAsync(4_999);
    expect(startEndpoint).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await starting;

    expect(startEndpoint).toHaveBeenCalledWith('offer-sdp', 'session-one');
    expect(call.snapshot.phase).toBe('listening');
    expect(vi.getTimerCount()).toBe(0);
    call.dispose();
  });

  it('distinguishes denied microphone permission and removes the autoplay target', async () => {
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: {
        getUserMedia: vi.fn(async () => {
          throw new DOMException('blocked', 'NotAllowedError');
        }),
      },
    });
    const startEndpoint = vi.fn();
    const call = new LiveCall(startEndpoint, vi.fn());

    await call.start('session-one');

    expect(call.snapshot.phase).toBe('failed');
    expect(call.snapshot.error).toMatch(/Microphone access was denied/);
    expect(call.snapshot.retryable).toBe(true);
    expect(document.body.querySelector('audio')).toBeNull();
    expect(startEndpoint).not.toHaveBeenCalled();
    call.dispose();
  });

  it('activates PCM audio before microphone permission and closes it on denial', async () => {
    const order: string[] = [];
    const context = new FakeAudioContext();
    context.resume.mockImplementation(async () => {
      order.push('resume');
    });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: {
        getUserMedia: vi.fn(() => {
          order.push('permission');
          return Promise.reject(new DOMException('blocked', 'NotAllowedError'));
        }),
      },
    });
    vi.stubGlobal('WebSocket', FakePCMWebSocket);
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const call = new LiveCall(vi.fn(), vi.fn(), {
      transport: 'websocket_pcm',
      createAudioContext: () => {
        order.push('create');
        return context as unknown as AudioContext;
      },
    });

    const starting = call.start('session-pcm');
    expect(order).toEqual(['create', 'resume', 'permission']);
    await starting;

    expect(call.snapshot.phase).toBe('failed');
    expect(call.snapshot.error).toMatch(/Microphone access was denied/);
    expect(context.close).toHaveBeenCalledOnce();
    call.dispose();
  });

  it('closes preactivated PCM audio when stopped during the permission prompt', async () => {
    const context = new FakeAudioContext();
    const track = mediaTrack();
    let grantPermission: ((stream: MediaStream) => void) | undefined;
    const permission = new Promise<MediaStream>((resolve) => {
      grantPermission = resolve;
    });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(() => permission) },
    });
    vi.stubGlobal('WebSocket', FakePCMWebSocket);
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const startEndpoint = vi.fn();
    const call = new LiveCall(startEndpoint, vi.fn(), {
      transport: 'websocket_pcm',
      createAudioContext: () => context as unknown as AudioContext,
    });

    const starting = call.start('session-pcm');
    expect(context.resume).toHaveBeenCalledOnce();
    await call.stop();
    expect(context.close).toHaveBeenCalledOnce();

    grantPermission?.(mediaStream(track));
    await expect(starting).resolves.toBeNull();
    expect(track.stop).toHaveBeenCalledOnce();
    expect(startEndpoint).not.toHaveBeenCalled();
  });

  it('stops microphone tracks, closes the peer, deletes the session, and removes audio', async () => {
    const track = mediaTrack();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => mediaStream(track)) },
    });
    const stopEndpoint = vi.fn(async (liveId: string) => ({
      live_id: liveId,
      status: 'ended' as const,
    }));
    const call = new LiveCall(
      async () => ({ live_id: 'live_one', session_id: 'session-one', sdp: 'answer-sdp' }),
      stopEndpoint,
    );
    await call.start('session-one');
    const peer = FakePeerConnection.instances[0];
    expect(document.body.querySelector('audio')).not.toBeNull();

    await call.stop();

    expect(stopEndpoint).toHaveBeenCalledWith('live_one');
    expect(track.stop).toHaveBeenCalledOnce();
    expect(peer.channel.close).toHaveBeenCalledOnce();
    expect(peer.close).toHaveBeenCalledOnce();
    expect(document.body.querySelector('audio')).toBeNull();
    expect(call.snapshot.phase).toBe('ended');
  });

  it('streams bounded PCM over the authenticated socket and flushes playback on interrupt', async () => {
    const track = mediaTrack();
    const stream = mediaStream(track);
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => stream) },
    });
    vi.stubGlobal('WebSocket', FakePCMWebSocket);
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const socket = new FakePCMWebSocket();
    const context = new FakeAudioContext();
    const capture = new FakeWorkletNode();
    const playback = new FakeWorkletNode();
    const createWebSocket = vi.fn((_url: string) => socket as unknown as WebSocket);
    const startEndpoint = vi.fn(async () => ({
      live_id: 'live_pcm',
      session_id: 'session-pcm',
      transport: 'websocket_pcm' as const,
      audio_url: '/chat/v1/live/sessions/live_pcm/audio?token=one-use',
    }));
    const stopEndpoint = vi.fn(async (liveId: string) => ({
      live_id: liveId,
      status: 'ended' as const,
    }));
    const call = new LiveCall(startEndpoint, stopEndpoint, {
      transport: 'websocket_pcm',
      createWebSocket,
      createAudioContext: () => context as unknown as AudioContext,
      createAudioWorkletNode: workletFactory(capture, playback),
      captureModuleURL: '/capture-worklet.js',
    });

    const starting = call.start('session-pcm');
    await vi.waitFor(() => expect(createWebSocket).toHaveBeenCalledOnce());
    expect(startEndpoint).toHaveBeenCalledWith('', 'session-pcm', 'websocket_pcm');
    expect(createWebSocket.mock.calls[0][0]).toMatch(
      /^ws:\/\/localhost(?::\d+)?\/chat\/v1\/live\/sessions\/live_pcm\/audio\?token=one-use$/,
    );
    socket.open();
    await expect(starting).resolves.toMatchObject({ live_id: 'live_pcm' });
    expect(call.snapshot.phase).toBe('listening');
    expect(context.audioWorklet.addModule).toHaveBeenCalledWith('/capture-worklet.js');
    expect(context.mediaSource.connect).toHaveBeenCalledWith(capture);
    expect(playback.connect).toHaveBeenCalledWith(context.destination);
    expect(playback.port.postMessage).toHaveBeenCalledWith({ type: 'reset', epoch: 2 });

    const captured = new Int16Array([1, -2]).buffer;
    capture.port.onmessage?.(new MessageEvent('message', { data: captured }));
    expect(socket.send).toHaveBeenCalledWith(captured);
    socket.bufferedAmount = 300 * 1024;
    capture.port.onmessage?.(new MessageEvent('message', { data: new Int16Array([3]).buffer }));
    expect(socket.send).toHaveBeenCalledTimes(1);

    const output = new Int16Array([16_384, -16_384]).buffer;
    socket.message(output);
    expect(context.createBufferSource).not.toHaveBeenCalled();
    expect(playback.port.postMessage).toHaveBeenLastCalledWith(
      { type: 'enqueue', epoch: 2, pcm: output },
      [output],
    );
    socket.message('{"type":"interrupt"}');
    expect(playback.port.postMessage).toHaveBeenLastCalledWith({ type: 'reset', epoch: 3 });

    await call.stop();
    expect(socket.close).toHaveBeenCalledOnce();
    expect(capture.port.close).toHaveBeenCalledOnce();
    expect(playback.port.close).toHaveBeenCalledOnce();
    expect(context.close).toHaveBeenCalledOnce();
    expect(track.stop).toHaveBeenCalledOnce();
    expect(stopEndpoint).toHaveBeenCalledWith('live_pcm');
  });

  it('enqueues a rapidly generated paragraph completely and in order on one output node', () => {
    const context = new FakeAudioContext();
    const output = new FakeWorkletNode();
    const call = new LiveCall(vi.fn(), vi.fn());
    attachPlayback(call, context, output);
    const internals = call as unknown as { playPCM(data: ArrayBuffer): void };
    const packets = Array.from(
      { length: 10 },
      (_, index) => new Int16Array(24_000).fill(index + 1).buffer,
    );

    packets.forEach((packet) => internals.playPCM(packet));

    expect(context.createBufferSource).not.toHaveBeenCalled();
    expect(output.port.postMessage).toHaveBeenCalledTimes(10);
    output.port.postMessage.mock.calls.forEach(([message], index) => {
      expect(message).toEqual({ type: 'enqueue', epoch: 1, pcm: packets[index] });
    });
    call.dispose();
    expect(output.port.postMessage).toHaveBeenLastCalledWith({ type: 'reset', epoch: 2 });
    expect(output.disconnect).toHaveBeenCalledOnce();
  });

  it.each(['interrupt', 'stop', 'dispose'] as const)(
    'clears continuous queued playback on %s and ignores stale worklet callbacks',
    async (action) => {
      const context = new FakeAudioContext();
      const output = new FakeWorkletNode();
      const call = new LiveCall(vi.fn(), vi.fn());
      attachPlayback(call, context, output);
      const internals = call as unknown as {
        playbackBytes: number;
        playPCM(data: ArrayBuffer): void;
        flushPlayback(reason: 'interrupt'): void;
      };
      for (let index = 0; index < 10; index += 1) internals.playPCM(new Int16Array(24_000).buffer);
      const staleMessage = output.port.onmessage;

      if (action === 'interrupt') internals.flushPlayback('interrupt');
      else await call[action]();
      expect(output.port.postMessage).toHaveBeenLastCalledWith({ type: 'reset', epoch: 2 });
      expect(internals.playbackBytes).toBe(0);
      staleMessage?.(
        new MessageEvent('message', {
          data: {
            type: 'state',
            epoch: 1,
            consumedBytes: 48_000,
            consumedPackets: 1,
            consumedSamples: 24_000,
            queuedSamples: 0,
            underruns: 1,
          },
        }),
      );
      expect(internals.playbackBytes).toBe(0);

      if (action === 'interrupt') {
        internals.playPCM(new Int16Array(24_000).buffer);
        expect(output.port.postMessage).toHaveBeenLastCalledWith(
          expect.objectContaining({ type: 'enqueue', epoch: 2 }),
          expect.any(Array),
        );
      } else {
        expect(output.port.close).toHaveBeenCalledOnce();
        expect(output.disconnect).toHaveBeenCalledOnce();
      }
      call.dispose();
    },
  );

  it.each([
    { samples: 24_000, capacity: 600 },
    { samples: 1, capacity: 16_384 },
  ])(
    'bounds retained audio and packet metadata without silently skipping ($samples samples)',
    ({ samples, capacity }) => {
      const context = new FakeAudioContext();
      const output = new FakeWorkletNode();
      const stop = vi.fn(async (liveId: string) => ({ live_id: liveId, status: 'ended' as const }));
      const call = new LiveCall(vi.fn(), stop);
      attachPlayback(call, context, output);
      const internals = call as unknown as {
        liveId: string;
        playPCM(data: ArrayBuffer): void;
      };
      internals.liveId = 'live_capacity';
      for (let index = 0; index < capacity; index += 1)
        internals.playPCM(new Int16Array(samples).buffer);
      expect(output.port.postMessage).toHaveBeenCalledTimes(capacity);
      expect(context.createBufferSource).not.toHaveBeenCalled();

      // Aggregate worklet acknowledgements release both byte and packet capacity.
      output.port.onmessage?.(
        new MessageEvent('message', {
          data: {
            type: 'state',
            epoch: 1,
            consumedBytes: samples * 2,
            consumedPackets: 1,
            consumedSamples: samples,
            queuedSamples: samples * (capacity - 1),
            underruns: 0,
          },
        }),
      );
      internals.playPCM(new Int16Array(samples).buffer);
      expect(output.port.postMessage).toHaveBeenCalledTimes(capacity + 1);
      internals.playPCM(new Int16Array(samples).buffer);
      expect(call.snapshot).toMatchObject({
        phase: 'failed',
        error: 'Live audio playback capacity exceeded. Please start a new call.',
      });
      expect(stop).toHaveBeenCalledWith('live_capacity');
      expect(context.close).toHaveBeenCalledOnce();
      expect(output.port.close).toHaveBeenCalledOnce();
      call.dispose();
    },
  );

  it('batches opt-in PCM counters, flush reasons, silence, and stops reporting on cleanup', async () => {
    const track = mediaTrack();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => mediaStream(track)) },
    });
    vi.stubGlobal('WebSocket', FakePCMWebSocket);
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const info = vi.spyOn(console, 'info').mockImplementation(() => undefined);
    const socket = new FakePCMWebSocket();
    const context = new FakeAudioContext();
    const capture = new FakeWorkletNode();
    const playback = new FakeWorkletNode();
    const reports: Array<{
      report: Record<string, number | string | boolean>;
      signal: AbortSignal;
    }> = [];
    const reportDiagnostics = vi.fn(
      async (_liveId: string, _pcm: ArrayBuffer, signal: AbortSignal, report?: unknown) => {
        reports.push({ report: report as Record<string, number | string | boolean>, signal });
        return new Response(null, { status: 204 });
      },
    );
    const call = new LiveCall(
      async () => ({
        live_id: 'live_diag',
        session_id: 'session-diag',
        transport: 'websocket_pcm' as const,
        audio_url: '/v1/live/sessions/live_diag/audio?token=hidden',
        diagnostics: true,
      }),
      async (liveId) => ({ live_id: liveId, status: 'ended' as const }),
      {
        transport: 'websocket_pcm',
        createWebSocket: () => socket as unknown as WebSocket,
        createAudioContext: () => context as unknown as AudioContext,
        createAudioWorkletNode: workletFactory(capture, playback),
        captureModuleURL: '/capture-worklet.js',
        sendPCMInput: reportDiagnostics,
      },
    );

    const starting = call.start('session-diag');
    await vi.waitFor(() => expect(socket.onopen).not.toBeNull());
    socket.open();
    await starting;

    capture.port.onmessage?.(new MessageEvent('message', { data: new Int16Array([1, 2]).buffer }));
    socket.bufferedAmount = 300 * 1024;
    capture.port.onmessage?.(new MessageEvent('message', { data: new Int16Array([3]).buffer }));

    socket.message(new Int16Array([100, -100]).buffer);
    context.currentTime = 2;
    socket.message(new Int16Array([200, -200]).buffer);
    playback.port.onmessage?.(
      new MessageEvent('message', {
        data: {
          type: 'state',
          epoch: 2,
          consumedBytes: 4,
          consumedPackets: 1,
          consumedSamples: 2,
          queuedSamples: 2,
          underruns: 1,
        },
      }),
    );
    socket.message('{"type":"interrupt"}');

    const internals = call as unknown as {
      diagnostics: object;
      reportDiagnosticSnapshot(state: object, generation: number): Promise<void>;
    };
    const diagnosticState = internals.diagnostics;
    await internals.reportDiagnosticSnapshot(diagnosticState, call.snapshot.generation);

    expect(reportDiagnostics).toHaveBeenCalledOnce();
    expect(reports[0].report).toMatchObject({
      audio_context_state: 'running',
      stream_open: true,
      packets_received: 2,
      bytes_received: 8,
      packets_scheduled: 2,
      packets_ended: 1,
      underruns: 1,
      flush_interrupts: 1,
      flush_buffer_resets: 0,
      input_packets: 2,
      input_bytes: 6,
      input_queue_drops: 1,
    });
    expect(Number(reports[0].report.input_silence_ms)).toBeGreaterThanOrEqual(0);
    expect(Number(reports[0].report.output_silence_ms)).toBeGreaterThanOrEqual(0);
    expect(info).not.toHaveBeenCalledWith('[live] playback flush: buffer_reset');
    expect(info).toHaveBeenCalledWith('[live] playback flush: interrupt');

    await call.stop();
    expect(reports[0].signal.aborted).toBe(true);
    await internals.reportDiagnosticSnapshot(diagnosticState, call.snapshot.generation);
    expect(reportDiagnostics).toHaveBeenCalledOnce();
    info.mockRestore();
  });

  it('does not allocate or report diagnostics unless the server opts in', async () => {
    const track = mediaTrack();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => mediaStream(track)) },
    });
    vi.stubGlobal('WebSocket', FakePCMWebSocket);
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const socket = new FakePCMWebSocket();
    const reportDiagnostics = vi.fn(async () => new Response(null, { status: 204 }));
    const call = new LiveCall(
      async () => ({
        live_id: 'live_no_diag',
        session_id: 'session-no-diag',
        transport: 'websocket_pcm' as const,
        audio_url: '/v1/live/sessions/live_no_diag/audio?token=hidden',
      }),
      async (liveId) => ({ live_id: liveId, status: 'ended' as const }),
      {
        transport: 'websocket_pcm',
        createWebSocket: () => socket as unknown as WebSocket,
        createAudioContext: () => new FakeAudioContext() as unknown as AudioContext,
        createAudioWorkletNode: () => new FakeWorkletNode() as unknown as AudioWorkletNode,
        captureModuleURL: '/capture-worklet.js',
        sendPCMInput: reportDiagnostics,
      },
    );

    const starting = call.start('session-no-diag');
    await vi.waitFor(() => expect(socket.onopen).not.toBeNull());
    socket.open();
    await starting;
    expect((call as unknown as { diagnostics: unknown }).diagnostics).toBeNull();
    await call.stop();
    expect(reportDiagnostics).not.toHaveBeenCalled();
  });

  it('requires both server diagnostics and the explicit local PCM capture opt-in', async () => {
    const context = new FakeAudioContext();
    const output = new FakeWorkletNode();
    const call = new LiveCall(vi.fn(), vi.fn());
    attachPlayback(call, context, output);
    const internals = call as unknown as {
      audioContext: AudioContext;
      pcmArtifactDiagnostics: LivePCMArtifactCapture | null;
      enablePCMArtifactDiagnostics(
        started: LiveStartResponse,
        context: AudioContext,
        generation: number,
      ): Promise<void>;
      playPCM(data: ArrayBuffer): void;
    };
    internals.audioContext = context as unknown as AudioContext;
    const started: LiveStartResponse = {
      live_id: 'live-artifacts',
      session_id: 'session-artifacts',
      diagnostics: true,
    };

    await internals.enablePCMArtifactDiagnostics(started, internals.audioContext, 0);
    expect(internals.pcmArtifactDiagnostics).toBeNull();
    localStorage.setItem('term-llm-live-pcm-capture', '1');
    await internals.enablePCMArtifactDiagnostics(
      { ...started, diagnostics: false },
      internals.audioContext,
      0,
    );
    expect(internals.pcmArtifactDiagnostics).toBeNull();

    await internals.enablePCMArtifactDiagnostics(started, internals.audioContext, 0);
    expect(internals.pcmArtifactDiagnostics).not.toBeNull();
    internals.playPCM(new Int16Array([100, -200]).buffer);
    expect(livePCMArtifactMetadata()).toMatchObject({
      live_id: 'live-artifacts',
      incoming_pcm: {
        retained_bytes: 4,
        leading_samples: [100, -200],
      },
    });

    call.dispose();
    expect(internals.pcmArtifactDiagnostics).toBeNull();
    expect(livePCMArtifactMetadata()).toMatchObject({
      live_id: 'live-artifacts',
      stop_reason: 'call_cleanup',
      incoming_pcm: { retained_bytes: 4 },
    });
  });

  it('tees opt-in rendered artifacts only from the continuous output worklet', async () => {
    localStorage.setItem('term-llm-live-pcm-capture', '1');
    vi.stubGlobal('MediaRecorder', FakeRenderedMediaRecorder);
    vi.stubGlobal('WebSocket', FakePCMWebSocket);
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const track = mediaTrack();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => mediaStream(track)) },
    });
    const socket = new FakePCMWebSocket();
    const context = new FakeAudioContext();
    const capture = new FakeWorkletNode();
    const playback = new FakeWorkletNode();
    const call = new LiveCall(
      async () => ({
        live_id: 'live-rendered-tee',
        session_id: 'session-rendered-tee',
        transport: 'websocket_pcm' as const,
        audio_url: '/v1/live/sessions/live-rendered-tee/audio?token=hidden',
        diagnostics: true,
      }),
      async (liveId) => ({ live_id: liveId, status: 'ended' as const }),
      {
        transport: 'websocket_pcm',
        createWebSocket: () => socket as unknown as WebSocket,
        createAudioContext: () => context as unknown as AudioContext,
        createAudioWorkletNode: workletFactory(capture, playback),
        captureModuleURL: '/capture-worklet.js',
      },
    );

    const starting = call.start('session-rendered-tee');
    await vi.waitFor(() => expect(socket.onopen).not.toBeNull());
    socket.open();
    await starting;

    expect(context.createMediaStreamDestination).toHaveBeenCalledOnce();
    expect(playback.connect).toHaveBeenCalledWith(context.destination);
    expect(playback.connect).toHaveBeenCalledWith(context.renderedDestination);
    expect(capture.connect).not.toHaveBeenCalledWith(context.renderedDestination);
    expect(context.mediaSource.connect).toHaveBeenCalledWith(capture);
    socket.message(new Int16Array([101, -202]).buffer);
    expect(livePCMArtifactMetadata()).toMatchObject({
      live_id: 'live-rendered-tee',
      incoming_pcm: { retained_bytes: 4, leading_samples: [101, -202] },
      browser_rendered: { supported: true },
    });

    await call.stop();
    expect(context.renderedTrack.stop).toHaveBeenCalledOnce();
    expect(playback.disconnect).toHaveBeenCalledOnce();
    expect(track.stop).toHaveBeenCalledOnce();
  });

  it('preserves every microphone sample over repeated 800 ms HTTP round trips', async () => {
    const sent: Int16Array[] = [];
    let release!: () => void;
    const sendPCMInput = vi.fn(async (_id: string, pcm: ArrayBuffer) => {
      sent.push(new Int16Array(pcm));
      await new Promise<void>((resolve) => {
        release = resolve;
      });
      return new Response(null, { status: 204 });
    });
    const call = new LiveCall(
      vi.fn(),
      vi.fn(async (id) => ({ live_id: id, status: 'ended' as const })),
      { transport: 'http_pcm', sendPCMInput },
    );
    const input = call as unknown as {
      pcmInputReady: boolean;
      mediaAbort: AbortController;
      liveId: string;
      sendCapturedPCM(data: ArrayBuffer, generation: number): void;
    };
    input.pcmInputReady = true;
    input.mediaAbort = new AbortController();
    input.liveId = 'slow-link';
    let packet = 0;
    const capture = (count: number) => {
      for (let i = 0; i < count; i += 1) {
        input.sendCapturedPCM(new Int16Array(320).fill(packet++).buffer, call.snapshot.generation);
      }
    };
    capture(5);
    expect(sent[0]).toHaveLength(1600);
    for (let round = 0; round < 12; round += 1) {
      // 40 x 20 ms packets captured while the preceding request is in flight.
      capture(40);
      expect(sent).toHaveLength(round + 1);
      release();
      await vi.waitFor(() => expect(sent).toHaveLength(round + 2), { interval: 1 });
      expect(sent.at(-1)).toHaveLength(12_800);
    }
    const combined = sent.flatMap((part) => Array.from(part));
    expect(combined).toHaveLength(packet * 320);
    // One assertion, not one per sample: ~155k expect() calls blow the test
    // timeout under full-suite load.
    const mismatch = combined.findIndex((value, index) => value !== Math.floor(index / 320));
    expect(mismatch).toBe(-1);
    release();
    await call.stop();
  });

  it.each([200, 260])(
    'bounds catch-up requests and fails visibly on a stalled link (%i packets)',
    async (packets) => {
      let release!: () => void;
      const sent: number[] = [];
      const stop = vi.fn(async (id: string) => ({ live_id: id, status: 'ended' as const }));
      const call = new LiveCall(vi.fn(), stop, {
        transport: 'http_pcm',
        sendPCMInput: async (_id, pcm) => {
          sent.push(pcm.byteLength);
          await new Promise<void>((resolve) => {
            release = resolve;
          });
          return new Response(null, { status: 204 });
        },
      });
      const input = call as unknown as {
        pcmInputReady: boolean;
        mediaAbort: AbortController;
        liveId: string;
        sendCapturedPCM(data: ArrayBuffer, generation: number): void;
      };
      const controller = new AbortController();
      input.pcmInputReady = true;
      input.mediaAbort = controller;
      input.liveId = 'stalled-link';
      for (let i = 0; i < packets; i += 1)
        input.sendCapturedPCM(new Int16Array(320).buffer, call.snapshot.generation);
      if (packets === 260) {
        expect(call.snapshot.phase).toBe('failed');
        expect(call.snapshot.error).toContain('microphone upload cannot keep up');
        expect(controller.signal.aborted).toBe(true);
        expect(stop).toHaveBeenCalledWith('stalled-link');
        release();
      } else {
        release();
        await vi.waitFor(() => expect(sent).toHaveLength(2));
        expect(sent[1]).toBe(64 * 1024);
        release();
        await vi.waitFor(() => expect(sent).toHaveLength(3));
        expect(sent.reduce((sum, size) => sum + size, 0)).toBe(packets * 640);
        release();
      }
      await call.stop();
    },
  );

  it('surfaces PCM context resume failures without leaving playback resources alive', async () => {
    const context = new FakeAudioContext();
    const playback = new FakeWorkletNode();
    const call = new LiveCall(vi.fn(), vi.fn());
    attachPlayback(call, context, playback);
    context.state = 'suspended';
    context.resume.mockRejectedValue(new Error('Audio output requires a new user gesture.'));
    const input = call as unknown as { playPCM(data: ArrayBuffer): void };
    input.playPCM(new Int16Array([1, 2]).buffer);
    await vi.waitFor(() => expect(call.snapshot.phase).toBe('failed'));
    expect(call.snapshot.error).toBe('Audio output requires a new user gesture.');
    expect(context.close).toHaveBeenCalledOnce();
    expect(playback.disconnect).toHaveBeenCalledOnce();
    call.dispose();
  });

  it('resumes PCM audio after microphone permission and later route interruptions', async () => {
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    vi.stubGlobal('WebSocket', FakePCMWebSocket);
    const context = new FakeAudioContext();
    const socket = new FakePCMWebSocket();
    const capture = new FakeWorkletNode();
    const playback = new FakeWorkletNode();
    context.resume.mockImplementation(async () => {
      context.state = 'running';
    });
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: {
        getUserMedia: vi.fn(async () => {
          context.state = 'suspended';
          return mediaStream();
        }),
      },
    });
    const call = new LiveCall(
      async () => ({
        live_id: 'resume',
        session_id: 'session',
        transport: 'websocket_pcm',
        audio_url: '/audio',
      }),
      async (id) => ({ live_id: id, status: 'ended' }),
      {
        transport: 'websocket_pcm',
        createAudioContext: () => context as unknown as AudioContext,
        createWebSocket: () => socket as unknown as WebSocket,
        createAudioWorkletNode: workletFactory(capture, playback),
        captureModuleURL: '/capture.js',
      },
    );
    const starting = call.start('session');
    await vi.waitFor(() => expect(socket.onopen).not.toBeNull());
    socket.open();
    await starting;
    expect(context.resume).toHaveBeenCalledTimes(2);
    let finish!: () => void;
    context.resume.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve;
        }),
    );
    context.state = 'suspended';
    const audio = context as unknown as AudioContext;
    audio.onstatechange?.call(audio, new Event('statechange'));
    socket.message(new Int16Array([1, 2]).buffer);
    socket.message(new Int16Array([3, 4]).buffer);
    expect(context.resume).toHaveBeenCalledTimes(3);
    await call.stop();
    expect(audio.onstatechange).toBeNull();
    finish();
    await Promise.resolve();
    expect(call.snapshot.phase).toBe('ended');
  });

  it('streams Hub-safe HTTP PCM, serializes bounded microphone batches, and aborts on stop', async () => {
    const track = mediaTrack();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => mediaStream(track)) },
    });
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const context = new FakeAudioContext();
    const capture = new FakeWorkletNode();
    const playback = new FakeWorkletNode();
    let outputController!: ReadableStreamDefaultController<Uint8Array>;
    const output = new ReadableStream<Uint8Array>({
      start(controller) {
        outputController = controller;
      },
    });
    const outputSignals: AbortSignal[] = [];
    let attachOutput!: () => void;
    const attachment = new Promise<void>((resolve) => {
      attachOutput = resolve;
    });
    const openPCMOutput = vi.fn(
      async (_liveId: string, _capability: string, signal: AbortSignal) => {
        outputSignals.push(signal);
        await attachment;
        return new Response(output, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        });
      },
    );
    let releaseFirst!: () => void;
    const firstInput = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    const inputCalls: Array<{ pcm: ArrayBuffer; signal: AbortSignal }> = [];
    const sendPCMInput = vi.fn(
      async (_liveId: string, pcm: ArrayBuffer, signal: AbortSignal): Promise<Response> => {
        inputCalls.push({ pcm, signal });
        if (inputCalls.length === 1) await firstInput;
        else await new Promise<void>(() => undefined);
        return new Response(null, { status: 204 });
      },
    );
    const stopEndpoint = vi.fn(async (liveId: string) => ({
      live_id: liveId,
      status: 'ended' as const,
    }));
    const call = new LiveCall(
      async () => ({
        live_id: 'live_http',
        session_id: 'session-http',
        transport: 'http_pcm' as const,
        audio_capability: 'one-use',
      }),
      stopEndpoint,
      {
        transport: 'http_pcm',
        openPCMOutput,
        sendPCMInput,
        createAudioContext: () => context as unknown as AudioContext,
        createAudioWorkletNode: workletFactory(capture, playback),
        captureModuleURL: '/capture-worklet.js',
      },
    );

    const starting = call.start('session-http');
    await vi.waitFor(() => expect(openPCMOutput).toHaveBeenCalledOnce());
    // A slow Hub attachment must not race microphone uploads against the
    // server's requirement for an attached output consumer.
    for (let index = 0; index < 5; index += 1)
      capture.port.onmessage?.(new MessageEvent('message', { data: new Int16Array(320).buffer }));
    expect(sendPCMInput).not.toHaveBeenCalled();
    attachOutput();
    await expect(starting).resolves.toMatchObject({ live_id: 'live_http' });
    expect(openPCMOutput).toHaveBeenCalledWith('live_http', 'one-use', expect.any(AbortSignal));
    expect(call.snapshot.phase).toBe('listening');

    const encoded = btoa(String.fromCharCode(0, 64, 0, 192));
    outputController.enqueue(
      new TextEncoder().encode(`event: audio\ndata: {"audio":"${encoded}"}\n\n`),
    );
    await vi.waitFor(() =>
      expect(playback.port.postMessage).toHaveBeenCalledWith(
        expect.objectContaining({ type: 'enqueue', epoch: 2 }),
        expect.any(Array),
      ),
    );
    outputController.enqueue(new TextEncoder().encode('event: interrupt\ndata: {}\n\n'));
    await vi.waitFor(() =>
      expect(playback.port.postMessage).toHaveBeenLastCalledWith({ type: 'reset', epoch: 3 }),
    );

    for (let index = 0; index < 10; index += 1) {
      const pcm = new Int16Array(320);
      pcm.fill(index);
      capture.port.onmessage?.(new MessageEvent('message', { data: pcm.buffer }));
    }
    await vi.waitFor(() => expect(sendPCMInput).toHaveBeenCalledTimes(1));
    expect(inputCalls[0].pcm.byteLength).toBe(3_200);
    releaseFirst();
    await vi.waitFor(() => expect(sendPCMInput).toHaveBeenCalledTimes(2));
    expect(inputCalls[1].pcm.byteLength).toBe(3_200);

    await call.stop();
    expect(outputSignals[0].aborted).toBe(true);
    expect(inputCalls[1].signal.aborted).toBe(true);
    expect(stopEndpoint).toHaveBeenCalledWith('live_http');
    expect(track.stop).toHaveBeenCalledOnce();
    expect(call.snapshot.phase).toBe('ended');
  });

  it('fails and cleans up when the HTTP PCM output attach is rejected', async () => {
    const track = mediaTrack();
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn(async () => mediaStream(track)) },
    });
    vi.stubGlobal('AudioContext', FakeAudioContext);
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
    const context = new FakeAudioContext();
    const stopEndpoint = vi.fn(async (liveId: string) => ({
      live_id: liveId,
      status: 'ended' as const,
    }));
    const call = new LiveCall(
      async () => ({
        live_id: 'live_rejected',
        session_id: 'session-http',
        transport: 'http_pcm' as const,
        audio_capability: 'expired',
      }),
      stopEndpoint,
      {
        transport: 'http_pcm',
        openPCMOutput: async () => new Response('expired capability', { status: 401 }),
        sendPCMInput: vi.fn(),
        createAudioContext: () => context as unknown as AudioContext,
        createAudioWorkletNode: () => new FakeWorkletNode() as unknown as AudioWorkletNode,
        captureModuleURL: '/capture-worklet.js',
      },
    );

    await expect(call.start('session-http')).resolves.toBeNull();
    expect(call.snapshot).toMatchObject({ phase: 'failed', error: 'expired capability' });
    expect(stopEndpoint).toHaveBeenCalledWith('live_rejected');
    expect(track.stop).toHaveBeenCalledOnce();
    expect(context.close).toHaveBeenCalledOnce();
  });

  it('preserves 48 kHz capture phase across worklet input blocks', () => {
    let processorSource = '';
    const NativeBlob = Blob;
    class CapturingBlob extends NativeBlob {
      constructor(parts?: BlobPart[], options?: BlobPropertyBag) {
        super(parts, options);
        processorSource = (parts || []).map((part) => String(part)).join('');
      }
    }
    vi.stubGlobal('Blob', CapturingBlob);
    const createObjectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:capture-test');
    const call = new LiveCall(vi.fn(), vi.fn());
    (
      call as unknown as {
        createCaptureModuleURL(): string;
      }
    ).createCaptureModuleURL();
    expect(createObjectURL).toHaveBeenCalledOnce();

    interface ProcessorInstance {
      input: number[];
      position: number;
      process(inputs: Float32Array[][]): boolean;
    }
    type ProcessorConstructor = new () => ProcessorInstance;
    let Processor: ProcessorConstructor | undefined;
    class FakeAudioWorkletProcessor {
      readonly port = { postMessage: vi.fn() };
    }
    const evaluate = new Function(
      'AudioWorkletProcessor',
      'sampleRate',
      'registerProcessor',
      processorSource,
    );
    evaluate(
      FakeAudioWorkletProcessor,
      48_000,
      (name: string, constructor: ProcessorConstructor) => {
        if (name === 'term-llm-pcm-capture') Processor = constructor;
      },
    );
    if (!Processor) throw new Error('capture processor was not registered');

    const processor = new Processor();
    expect(processor.process([[new Float32Array(128)]])).toBe(true);
    // The next 16 kHz sample is source frame 129. Retaining frame 127 leaves
    // that target at offset 2 when the following 128-frame block arrives.
    expect(processor.input).toHaveLength(1);
    expect(processor.position).toBe(2);
    call.dispose();
  });

  it('does not require MediaRecorder support', () => {
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn() },
    });
    vi.stubGlobal('MediaRecorder', undefined);
    expect(liveCapability()).toEqual({ supported: true, reason: '' });
  });
});

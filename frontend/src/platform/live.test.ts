import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LiveCall } from './live';
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

const mediaTrack = () => ({ stop: vi.fn() }) as unknown as MediaStreamTrack;
const mediaStream = (track = mediaTrack()) =>
  ({ getTracks: () => [track] }) as unknown as MediaStream;

beforeEach(() => {
  FakePeerConnection.instances = [];
  Object.defineProperty(globalThis, 'isSecureContext', { configurable: true, value: true });
  vi.stubGlobal('RTCPeerConnection', FakePeerConnection);
});

afterEach(() => {
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

  it('does not require MediaRecorder support', () => {
    Object.defineProperty(navigator, 'mediaDevices', {
      configurable: true,
      value: { getUserMedia: vi.fn() },
    });
    vi.stubGlobal('MediaRecorder', undefined);
    expect(liveCapability()).toEqual({ supported: true, reason: '' });
  });
});

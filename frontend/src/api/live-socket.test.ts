import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  liveAudioSocketOpen,
  liveAudioSocketSupported,
  openLiveAudioSocket,
  type LiveAudioSocket,
} from './live-socket';

class FakeSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static lastURL = '';
  readyState = FakeSocket.OPEN;
  bufferedAmount = 0;
  binaryType: BinaryType = 'blob';
  constructor(url: string) {
    FakeSocket.lastURL = url;
  }
  send(): void {}
  close(): void {}
}

const socketWithState = (readyState: number): LiveAudioSocket =>
  ({ readyState }) as unknown as LiveAudioSocket;

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('live audio socket transport', () => {
  it('opens the socket at the requested URL', () => {
    vi.stubGlobal('WebSocket', FakeSocket);
    const socket = openLiveAudioSocket('wss://example.test/v1/live/sessions/abc/audio');
    expect(FakeSocket.lastURL).toBe('wss://example.test/v1/live/sessions/abc/audio');
    expect(socket).toBeInstanceOf(FakeSocket);
  });

  it('treats only the open ready state as sendable', () => {
    vi.stubGlobal('WebSocket', FakeSocket);
    expect(liveAudioSocketOpen(socketWithState(FakeSocket.OPEN))).toBe(true);
    for (const state of [FakeSocket.CONNECTING, FakeSocket.CLOSING, FakeSocket.CLOSED])
      expect(liveAudioSocketOpen(socketWithState(state))).toBe(false);
  });

  it('reports support from the browser global', () => {
    vi.stubGlobal('WebSocket', FakeSocket);
    expect(liveAudioSocketSupported()).toBe(true);
    vi.stubGlobal('WebSocket', undefined);
    expect(liveAudioSocketSupported()).toBe(false);
  });
});

// Socket transport for live PCM audio. Raw WebSocket construction stays in the
// api layer, alongside every other browser transport, so platform code depends
// on the narrow interface below instead of a global.

// LiveAudioSocket is the subset of WebSocket the live call actually uses. A real
// WebSocket satisfies it structurally, so tests can still substitute a fake.
export interface LiveAudioSocket {
  binaryType: BinaryType;
  readonly readyState: number;
  readonly bufferedAmount: number;
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  onerror: ((event: Event) => void) | null;
  onclose: ((event: CloseEvent) => void) | null;
  send(data: ArrayBuffer): void;
  close(): void;
}

// openLiveAudioSocket connects the binary audio socket. Callers pass an absolute
// ws:// or wss:// URL they have already confirmed is same-origin.
export function openLiveAudioSocket(url: string): LiveAudioSocket {
  return new WebSocket(url);
}

// liveAudioSocketOpen reports whether the socket can accept a frame right now.
// The readyState constant is resolved here so callers never touch the global.
export function liveAudioSocketOpen(socket: LiveAudioSocket): boolean {
  return socket.readyState === WebSocket.OPEN;
}

// liveAudioSocketSupported reports whether this browser can open the socket
// transport at all, which gates the websocket_pcm live voice capability.
export function liveAudioSocketSupported(): boolean {
  return typeof globalThis.WebSocket === 'function';
}

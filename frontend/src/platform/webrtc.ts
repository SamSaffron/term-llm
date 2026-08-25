// Browser WebRTC frame protocol bridge.
interface SignalingSession {
  session_id: string;
  stun_url?: string;
  turn_url?: string;
  turn_username?: string;
  turn_credential?: string;
}
interface SignalingAnswer {
  type: 'answer';
  sdp: string;
}
interface WebRTCFrame {
  id: string;
  type: 'headers' | 'chunk' | 'done';
  headers?: HeadersInit;
  status?: number;
  data?: string;
}
interface WebRTCRequestInit extends RequestInit {
  __termLLMRetrySafe?: boolean;
}
interface PendingRequest {
  onHeaders(headers: HeadersInit, status: number): void;
  onChunk(fragment: string): void;
  onChunkFailure(error: unknown): void;
  onDone(status: number): void;
  fallback(): void;
}
const errorMessage = (error: unknown): string =>
  error instanceof Error ? error.message : String(error);

//
// When window.__WEBRTC_ENABLED__ is set (injected by the server at startup),
// this module attempts to establish a WebRTC data channel directly to the
// home peer, bypassing the intermediate relay for all /v1/ API calls.
//
// Two-tier timeout:
//   1. First-frame timeout: read-only requests fall back after 1 s; mutations get
//      5 s because their handlers may legitimately perform classification or
//      other work before sending headers.
//   2. Stream watchdog (30 s):  once streaming begins, if no frame (chunk,
//      done, or server keepalive) arrives for 30 s the stream is closed and
//      the channel renegotiated.  The app-layer resume logic reconnects via
//      HTTPS from the last sequence number — no data is lost.
//
// AbortSignal:  the caller's AbortController.signal (e.g. from the heartbeat
// monitor) is wired through — aborting it closes the WebRTC stream the same
// way a normal fetch abort would, letting app-layer recovery take over.
//
// If ICE negotiation does not complete within 8 seconds the browser silently
// falls back to HTTPS and keeps renegotiating in the background with bounded
// backoff. Online, visibility, and pageshow wake that retry immediately. Data
// channel disconnects follow the same path while HTTPS remains available.
//
// Diagnostics mode: set window.__WEBRTC_DIAGNOSTICS__ = true (or pass
// ?webrtc_diag=1 in the URL) to enable console.log timeline output:
//   [webrtc] connection lifecycle events with timestamps
//   [webrtc] per-request: method, path, body size, status, latency
//   [webrtc] pending queue size and HTTPS/stream-close fallback state
//
// Force TURN relay: pass ?webrtc_turn=1 to set iceTransportPolicy=relay,
// which forces all traffic through the TURN server (ignores host/srflx).

export function installWebRTC(): () => void {
  'use strict';

  const READ_RESPONSE_TIMEOUT_MS = 1000;
  const MUTATION_RESPONSE_TIMEOUT_MS = 5000;
  const responseTimeoutForMethod = (method: string): number => {
    const normalized = String(method || 'GET').toUpperCase();
    return normalized === 'GET' || normalized === 'HEAD' || normalized === 'OPTIONS'
      ? READ_RESPONSE_TIMEOUT_MS
      : MUTATION_RESPONSE_TIMEOUT_MS;
  };

  if (window.__TERM_LLM_WEBRTC_TESTING__) {
    window.__TERM_LLM_WEBRTC_TEST_HOOKS__ = Object.freeze({ responseTimeoutForMethod });
  }
  if (!window.__WEBRTC_ENABLED__) return () => undefined;
  if (new URLSearchParams(window.location.search).has('no_webrtc')) return () => undefined;

  const SIGNALING_URL = window.__WEBRTC_SIGNALING_URL__ || '';
  const UI_PREFIX = window.TERM_LLM_UI_PREFIX || '/ui';
  const ICE_TIMEOUT_MS = 8000;

  // Read-only requests should fail over quickly when UDP is dead. Mutations get
  // longer because endpoints such as /interrupt can classify input before they
  // emit headers. Retried mutations still need endpoint-level idempotency; the
  // interrupt endpoint uses its stable interjection ID for that purpose.

  // Once streaming has started, if no frame arrives within this window,
  // assume the channel silently died.  The backend sends keepalive pings
  // every ~20 s, so 30 s gives 10 s of grace before declaring death.
  const STREAM_WATCHDOG_MS = 30000;

  const originalFetch: typeof fetch = window.fetch.bind(window);
  const encoder = new TextEncoder();

  const pendingRequests = new Map<string, PendingRequest>();

  let dataChannel: RTCDataChannel | null = null;
  let disposed = false;
  let renegotiating = false;
  let renegotiationTimer: ReturnType<typeof setTimeout> | null = null;
  let renegotiationAttempt = 0;
  const RENEGOTIATION_BACKOFF_MS = [2000, 5000, 10000, 30000, 60000];

  // ---------------------------------------------------------------------------
  // Diagnostics
  // ---------------------------------------------------------------------------

  const _params = new URLSearchParams(window.location.search);
  const diagEnabled = !!(window.__WEBRTC_DIAGNOSTICS__ || _params.has('webrtc_diag'));
  const forceTurn = _params.has('webrtc_turn');

  // t0 is the timestamp when initWebRTC() starts, used for relative timings.
  let diagT0 = 0;

  function diag(msg: string): void {
    if (!diagEnabled) return;
    const elapsed = diagT0 ? (performance.now() - diagT0) | 0 : 0;
    console.log('[webrtc] +' + elapsed + 'ms ' + msg);
  }

  function diagQueue(state: string, fallback: string): void {
    diag(
      'queue state=' +
        state +
        ' pending=' +
        pendingRequests.size +
        ' fallback=' +
        (fallback || 'none'),
    );
  }

  function noteTransportState(transportState: string, detail: string): void {
    diag(`transport=${transportState} detail=${detail} attempt=${renegotiationAttempt}`);
  }

  function maybeReloadForUIVersion(response: Response): void {
    const serverVersion = response.headers.get('X-Term-LLM-UI-Version');
    const currentVersion = window.TERM_LLM_UI_VERSION || '';
    if (serverVersion && currentVersion && serverVersion !== currentVersion) location.reload();
  }

  function restoreHTTPSFetch(reason: string): void {
    const wasWebRTCTransport = window.fetch === patchedFetch;
    window.fetch = originalFetch;
    if (!wasWebRTCTransport) return;
    diag(reason + ' — restoring original fetch');
    window.dispatchEvent(new CustomEvent('term-llm:transport-fallback', { detail: { reason } }));
  }

  // ---------------------------------------------------------------------------
  // Initialisation
  // ---------------------------------------------------------------------------

  async function initWebRTC(): Promise<boolean> {
    diagT0 = performance.now();
    diag('init signaling=' + SIGNALING_URL);
    let pc: RTCPeerConnection | null = null;
    try {
      // 1. Request a signaling session (no auth — session_id gates routing).
      const sessStart = performance.now();
      const sessResp = await originalFetch(SIGNALING_URL + '/session', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      });
      if (!sessResp.ok) {
        diag('session request failed status=' + sessResp.status);
        return false;
      }
      const sess = (await sessResp.json()) as SignalingSession;
      diag(
        'session created id=' +
          sess.session_id +
          (sess.turn_url ? ' turn=' + sess.turn_url : ' no-turn') +
          ' (' +
          ((performance.now() - sessStart) | 0) +
          'ms)',
      );

      // 2. Build ICE server list from session response.
      const iceServers: RTCIceServer[] = [
        { urls: sess.stun_url || 'stun:stun.l.google.com:19302' },
      ];
      if (sess.turn_url) {
        iceServers.push({
          urls: sess.turn_url,
          username: sess.turn_username,
          credential: sess.turn_credential,
        });
      }

      const pcConfig: RTCConfiguration = { iceServers };
      if (forceTurn) {
        pcConfig.iceTransportPolicy = 'relay';
        diag('FORCED TURN — iceTransportPolicy=relay');
      }
      const peer = new RTCPeerConnection(pcConfig);
      pc = peer;

      peer.oniceconnectionstatechange = () => {
        diag('ICE state=' + peer.iceConnectionState);
      };

      // Log each ICE candidate as it is gathered.
      peer.onicecandidate = (e) => {
        if (e.candidate) {
          diag(
            'ICE candidate: ' +
              e.candidate.type +
              ' ' +
              e.candidate.protocol +
              ' ' +
              e.candidate.address +
              ':' +
              e.candidate.port +
              (e.candidate.relatedAddress
                ? ' raddr=' + e.candidate.relatedAddress + ':' + e.candidate.relatedPort
                : ''),
          );
        } else {
          diag('ICE candidate gathering done (null sentinel)');
        }
      };

      // 3. Browser creates the data channel (ordered, reliable).
      const dc = peer.createDataChannel('api', { ordered: true });

      // 4. Generate offer and wait for ICE gathering to complete so the SDP
      //    includes all candidates (vanilla ICE — no trickle).
      const offer = await peer.createOffer();
      await peer.setLocalDescription(offer);
      diag('ICE gathering started');

      // Wait for ICE gathering to complete, but cap at 4 s so a slow/broken
      // STUN or TURN server (e.g. IPv6 timeout) never stalls the handshake.
      // Whatever candidates are ready at that point are included in the offer.
      await Promise.race([
        new Promise<void>((resolve) => {
          if (peer.iceGatheringState === 'complete') {
            resolve();
            return;
          }
          peer.onicegatheringstatechange = () => {
            if (peer.iceGatheringState === 'complete') resolve();
          };
        }),
        new Promise<void>((resolve) => setTimeout(resolve, 4000)),
      ]);
      diag('ICE gathering complete');

      // 5. Send the completed offer to the signaling server.
      const offerStart = performance.now();
      const sendResp = await originalFetch(SIGNALING_URL + '/signal', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: sess.session_id,
          type: 'offer',
          sdp: peer.localDescription?.sdp || offer.sdp,
        }),
      });
      if (!sendResp.ok) {
        diag('offer post failed status=' + sendResp.status);
        try {
          peer.close();
        } catch {
          /* ignore */
        }
        return false;
      }
      diag('offer sent (' + ((performance.now() - offerStart) | 0) + 'ms)');

      // 6. Poll for the home peer's answer (8-second timeout).
      const answer = await pollForAnswer(sess.session_id, ICE_TIMEOUT_MS);
      if (!answer) {
        diag('answer timeout — falling back to HTTPS');
        try {
          peer.close();
        } catch {
          /* ignore */
        }
        return false; // timed out — HTTPS remains available while retrying
      }
      diag('answer received');

      await peer.setRemoteDescription({ type: 'answer', sdp: answer.sdp });

      // 7. Wait for ICE connectivity and the data channel to open.
      await Promise.race([
        waitForDataChannelOpen(dc),
        new Promise<never>((_, reject) =>
          setTimeout(() => reject(new Error('WebRTC connect timeout')), ICE_TIMEOUT_MS),
        ),
      ]);

      // 8. Connected — wire up handlers and patch fetch.
      if (disposed) {
        dc.close();
        peer.close();
        return false;
      }
      dataChannel = dc;
      dc.onmessage = handleMessage;
      dc.onclose = () => onChannelClose(dc);
      dc.onerror = () => onChannelClose(dc);

      window.fetch = patchedFetch;
      renegotiationAttempt = 0;
      noteTransportState('connected', 'data-channel-open');

      diag('data channel open — fetch patched');
      return true;
    } catch (error) {
      const message = errorMessage(error);
      diag('init error: ' + message);
      if (pc) {
        try {
          pc.close();
        } catch {
          /* ignore */
        }
      }
      noteTransportState('https', message || 'negotiation-failed');
      return false;
    }
  }

  function waitForDataChannelOpen(dc: RTCDataChannel): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      if (dc.readyState === 'open') {
        resolve();
        return;
      }
      dc.onopen = () => resolve();
      dc.onerror = () => reject(new Error('WebRTC data channel failed to open'));
    });
  }

  async function pollForAnswer(
    sessionId: string,
    timeoutMs: number,
  ): Promise<SignalingAnswer | null> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      try {
        const resp = await originalFetch(
          SIGNALING_URL + '/signal?session_id=' + encodeURIComponent(sessionId),
          { signal: AbortSignal.timeout(Math.min(remaining, 12000)) },
        );
        if (resp.status === 204 || resp.status === 408) continue;
        if (!resp.ok) return null;
        const msg = (await resp.json()) as SignalingAnswer;
        if (msg.type === 'answer') return msg;
      } catch (_e) {
        return null;
      }
    }
    return null;
  }

  // ---------------------------------------------------------------------------
  // Data channel message handling
  // ---------------------------------------------------------------------------

  function handleMessage(event: MessageEvent<string>): void {
    let frame: WebRTCFrame;
    try {
      frame = JSON.parse(event.data) as WebRTCFrame;
    } catch {
      return;
    }

    const pending = pendingRequests.get(frame.id);
    if (!pending) return;

    if (frame.type === 'headers') {
      pending.onHeaders(frame.headers || {}, frame.status || 200);
    } else if (frame.type === 'chunk') {
      try {
        pending.onChunk(frame.data || '');
      } catch (error) {
        pending.onChunkFailure(error);
      }
    } else if (frame.type === 'done') {
      pending.onDone(frame.status || 200);
      pendingRequests.delete(frame.id);
    }
  }

  // ---------------------------------------------------------------------------
  // Drain all pending requests to HTTPS
  // ---------------------------------------------------------------------------

  // Called when the channel dies (close/error/timeout). Retry-safe requests
  // that have not received a response frame are rescued via HTTPS; unsafe
  // mutations report an unknown outcome instead of being replayed. Requests
  // already streaming are closed for app-layer cursor resume.
  function drainPendingToHTTPS(reason: string): void {
    if (pendingRequests.size === 0) return;
    diagQueue('draining', reason + ':https-before-response-or-stream-close');

    // Snapshot the entries; fallback() deletes its own key.
    const entries = Array.from(pendingRequests.values());
    for (const entry of entries) {
      entry.fallback();
    }
  }

  // ---------------------------------------------------------------------------
  // Channel close / error
  // ---------------------------------------------------------------------------

  function onChannelClose(closedChannel: RTCDataChannel): void {
    if (closedChannel !== dataChannel) return;
    dataChannel = null;
    restoreHTTPSFetch('data channel closed');
    drainPendingToHTTPS('channel closed');
    scheduleRenegotiation('channel-closed');
  }

  // ---------------------------------------------------------------------------
  // Background renegotiation
  // ---------------------------------------------------------------------------

  function scheduleRenegotiation(reason: string, immediate = false): void {
    if (disposed) return;
    if (dataChannel && dataChannel.readyState === 'open') return;
    if (renegotiating) return;
    if (renegotiationTimer) {
      if (!immediate) return;
      clearTimeout(renegotiationTimer);
      renegotiationTimer = null;
    }
    if (typeof navigator !== 'undefined' && navigator.onLine === false) {
      noteTransportState('waiting-online', reason);
      return;
    }
    const index = Math.min(renegotiationAttempt, RENEGOTIATION_BACKOFF_MS.length - 1);
    const delay = immediate ? 0 : RENEGOTIATION_BACKOFF_MS[index];
    noteTransportState('retrying', reason + ':' + delay + 'ms');
    diag(
      'renegotiation scheduled reason=' +
        reason +
        ' delay=' +
        delay +
        'ms attempt=' +
        (renegotiationAttempt + 1),
    );
    renegotiationTimer = setTimeout(async () => {
      renegotiationTimer = null;
      if (typeof navigator !== 'undefined' && navigator.onLine === false) {
        noteTransportState('waiting-online', reason);
        return;
      }
      renegotiating = true;
      let connected: boolean;
      try {
        connected = await initWebRTC();
      } finally {
        renegotiating = false;
      }
      if (connected) {
        renegotiationAttempt = 0;
        return;
      }
      renegotiationAttempt += 1;
      scheduleRenegotiation('background-retry', false);
    }, delay);
  }

  function triggerRenegotiation() {
    // Tear down the current channel so new requests route to HTTPS immediately.
    const previousChannel = dataChannel;
    dataChannel = null;
    if (previousChannel) {
      try {
        previousChannel.close();
      } catch (_e) {
        /* ignore */
      }
    }
    restoreHTTPSFetch('data channel degraded');

    // Rescue any other in-flight requests stuck on the dead channel.
    drainPendingToHTTPS('renegotiation');
    diag('renegotiating — new requests use HTTPS');
    scheduleRenegotiation('transport-degraded', false);
  }

  // ---------------------------------------------------------------------------
  // Patched fetch — routes /v1/ API calls over the data channel
  // ---------------------------------------------------------------------------

  const patchedFetch: typeof fetch = (
    input: RequestInfo | URL,
    options: WebRTCRequestInit = {},
  ): Promise<Response> => {
    const urlStr = input instanceof Request ? input.url : input.toString();
    if (dataChannel && dataChannel.readyState === 'open' && isAPIPath(urlStr)) {
      // Bodies over 100 KiB expand again inside one data-channel frame, so use HTTPS.
      if (
        (options.body === undefined || typeof options.body === 'string') &&
        (!options.body || new Blob([options.body]).size <= 100 * 1024)
      ) {
        return webrtcFetch(urlStr, options);
      }
    }
    return originalFetch(input, options);
  };

  function isAPIPath(urlStr: string): boolean {
    try {
      const url = new URL(urlStr, window.location.origin);
      return url.origin === window.location.origin && url.pathname.startsWith(UI_PREFIX + '/v1/');
    } catch (_e) {
      return false;
    }
  }

  function webrtcFetch(urlStr: string, options: WebRTCRequestInit): Promise<Response> {
    return new Promise<Response>((resolve, reject) => {
      const reqId = crypto.randomUUID();
      const requestChannel = dataChannel!;
      let streamController: ReadableStreamDefaultController<Uint8Array> | null = null;
      let resolved = false;
      let gotResponse = false;
      let requestSent = false,
        serverDone = false;
      let cleaned = false;
      const reqStart = performance.now();

      const urlObj = new URL(urlStr, window.location.origin);
      const method = (options.method || 'GET').toUpperCase();
      const transportRetrySafe =
        responseTimeoutForMethod(method) === READ_RESPONSE_TIMEOUT_MS ||
        options.__termLLMRetrySafe === true;
      const responseTimeoutMs = responseTimeoutForMethod(method);
      const path = urlObj.pathname + (urlObj.search || '');
      const signal = options.signal;
      let abortHandler: (() => void) | null = null;
      const bodySize = typeof options.body === 'string' ? new Blob([options.body]).size : 0;

      diag('→ ' + method + ' ' + path + ' (' + bodySize + 'b)');

      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          streamController = controller;
        },
        cancel() {
          cleanup('stream-cancel');
        },
      });

      let responseBytes = 0;
      let streamWatchdogId: ReturnType<typeof setTimeout> | null = null;

      function resolveOnce(response: Response | Promise<Response>): void {
        if (!resolved) {
          resolved = true;
          resolve(response);
        }
      }

      function rejectOnce(error: unknown): void {
        if (!resolved) {
          resolved = true;
          reject(error);
        }
      }

      // Central cleanup — idempotent, called from every exit path. If the
      // browser abandons a request while the data channel remains usable, tell
      // the peer to cancel the matching HTTP request context and release its
      // concurrency slot.
      function cleanup(reason: string): void {
        if (cleaned) return;
        cleaned = true;
        clearTimeout(responseTimer);
        if (streamWatchdogId !== null) clearTimeout(streamWatchdogId);
        if (
          requestSent &&
          !serverDone &&
          (gotResponse || transportRetrySafe) &&
          requestChannel?.readyState === 'open'
        ) {
          try {
            requestChannel.send(JSON.stringify({ id: reqId, type: 'cancel' }));
            diag('↯ cancel ' + method + ' ' + path + ' reason=' + reason);
          } catch (_e) {
            /* channel shutdown will cancel the peer context */
          }
        }
        pendingRequests.delete(reqId);
        if (abortHandler && signal) {
          try {
            signal.removeEventListener('abort', abortHandler);
          } catch {
            /* ignore */
          }
        }
        diag('cleanup ' + method + ' ' + path + ' reason=' + reason);
        diagQueue(
          'settled',
          reason.includes('fallback') || reason.includes('timeout') ? 'https' : 'none',
        );
      }

      function closeStream(): void {
        if (streamController) {
          try {
            streamController.close();
          } catch (_e) {
            /* ignore */
          }
          streamController = null;
        }
      }

      function errorStream(error: unknown): void {
        if (streamController) {
          try {
            streamController.error(error);
          } catch {
            /* ignore */
          }
          streamController = null;
        }
      }

      // --- Stream watchdog: resets on every frame after first response ---
      function resetStreamWatchdog(): void {
        if (streamWatchdogId !== null) clearTimeout(streamWatchdogId);
        streamWatchdogId = setTimeout(() => {
          diag(
            '⚠ stream watchdog (' +
              STREAM_WATCHDOG_MS +
              'ms) ' +
              method +
              ' ' +
              path +
              ' — closing stale stream',
          );
          cleanup('stream-watchdog');
          closeStream();
          triggerRenegotiation();
        }, STREAM_WATCHDOG_MS);
      }

      // --- 1 s timeout: if no response frame arrives, fall back to HTTPS ---
      const responseTimer = setTimeout(() => {
        if (gotResponse) return; // already got data, all good

        diag(
          '⚠ timeout (' +
            responseTimeoutMs +
            'ms) ' +
            method +
            ' ' +
            path +
            ' — falling back to HTTPS',
        );

        cleanup('first-frame-timeout');
        closeStream();

        if (transportRetrySafe) {
          resolveOnce(originalFetch(urlStr, options));
        } else {
          const error = new Error(
            'WebRTC mutation outcome is unknown; the request was not replayed over HTTPS.',
          );
          error.name = 'UnknownMutationOutcomeError';
          rejectOnce(error);
        }

        // Mark the channel as degraded and renegotiate in the background.
        // This also drains any other stuck pending requests.
        triggerRenegotiation();
      }, responseTimeoutMs);

      function markGotResponse(): void {
        if (!gotResponse) {
          gotResponse = true;
          clearTimeout(responseTimer);
          // Start the rolling stream watchdog now that data is flowing.
          resetStreamWatchdog();
        } else {
          // Reset watchdog on every subsequent frame.
          resetStreamWatchdog();
        }
      }

      // --- AbortSignal wiring (heartbeat monitor, user cancel, etc.) ---
      if (signal) {
        if (signal.aborted) {
          // Already aborted — don't even start the WebRTC request.
          cleanup('pre-aborted');
          closeStream();
          resolveOnce(originalFetch(urlStr, options));
          return;
        }
        abortHandler = () => {
          diag('⚠ abort signal ' + method + ' ' + path);
          cleanup('abort-signal');
          if (!resolved) {
            closeStream();
            // Delegate to original fetch which will also throw AbortError.
            resolveOnce(originalFetch(urlStr, options));
          } else {
            // Already streaming — error the stream so reader.read() rejects
            // with AbortError, triggering the app-layer recovery path.
            errorStream(new DOMException('The operation was aborted.', 'AbortError'));
          }
        };
        signal.addEventListener('abort', abortHandler, { once: true });
      }

      // Null-body statuses per Fetch spec — Response constructor forbids a body.
      const nullBodyStatus = (status: number): boolean =>
        status === 101 || status === 204 || status === 205 || status === 304;

      // fallback: called by drainPendingToHTTPS() when the channel dies.
      function fallback(): void {
        const fallbackState = !gotResponse ? 'https' : 'stream-close';
        diagQueue('fallback', fallbackState);
        cleanup('drain-fallback');
        if (!gotResponse) {
          closeStream();
          if (transportRetrySafe) {
            diag('↩ fallback ' + method + ' ' + path);
            resolveOnce(originalFetch(urlStr, options));
          } else {
            const error = new Error(
              'WebRTC mutation outcome is unknown; the request was not replayed over HTTPS.',
            );
            error.name = 'UnknownMutationOutcomeError';
            rejectOnce(error);
          }
        } else {
          // Already streaming — close the stream; consumer sees truncation.
          // App-layer resume logic will reconnect via HTTPS.
          closeStream();
        }
      }

      pendingRequests.set(reqId, {
        onHeaders(headers, status) {
          markGotResponse();
          const response = new Response(nullBodyStatus(status) ? null : stream, {
            status,
            headers: new Headers(headers),
          });
          maybeReloadForUIVersion(response);
          resolveOnce(response);
        },
        onChunk(fragment) {
          markGotResponse();
          if (!resolved) {
            resolveOnce(new Response(stream, { status: 200 })); // 200 is never null-body
          }
          const chunk = typeof fragment === 'string' ? fragment : '';
          responseBytes += chunk.length;
          if (streamController) streamController.enqueue(encoder.encode(chunk));
        },
        onChunkFailure(error) {
          diag('chunk delivery failed: ' + errorMessage(error));
          cleanup('chunk-delivery-failure');
          errorStream(error);
          triggerRenegotiation();
        },
        onDone(status) {
          serverDone = true;
          markGotResponse();
          const latency = (performance.now() - reqStart) | 0;
          diag(
            '← ' +
              status +
              ' ' +
              method +
              ' ' +
              path +
              ' (' +
              responseBytes +
              'b, ' +
              latency +
              'ms)',
          );
          cleanup('done');
          if (!resolved) {
            resolveOnce(new Response(nullBodyStatus(status) ? null : stream, { status }));
          }
          closeStream();
        },
        fallback,
      });
      diagQueue('queued', 'none');

      // Build and send the request frame.
      const headersObj: Record<string, string> = {};

      // Carry over all request headers (Authorization, session_id, Content-Type, …).
      if (options.headers) {
        const h =
          options.headers instanceof Headers ? options.headers : new Headers(options.headers);
        for (const [k, v] of h.entries()) headersObj[k] = v;
      }

      const frame = {
        id: reqId,
        method,
        path,
        headers: Object.keys(headersObj).length ? headersObj : undefined,
        body:
          typeof options.body === 'string' && options.body ? strToBase64(options.body) : undefined,
      };

      try {
        requestChannel.send(JSON.stringify(frame));
        requestSent = true;
      } catch (error) {
        diag('send error: ' + errorMessage(error));
        cleanup('send-error');
        closeStream();
        // A synchronous send failure proves that no frame left this browser,
        // so even a non-idempotent mutation is safe to send once over HTTPS.
        resolveOnce(originalFetch(urlStr, options));
        triggerRenegotiation();
      }
    });
  }

  // UTF-8–safe base64 encoding (handles multi-byte characters).
  function strToBase64(str: string): string {
    const bytes = encoder.encode(str);
    let binary = '';
    for (let i = 0; i < bytes.length; i++) {
      binary += String.fromCharCode(bytes[i]);
    }
    return btoa(binary);
  }

  // ---------------------------------------------------------------------------
  // Kick off
  // ---------------------------------------------------------------------------

  const startPersistentWebRTC = async (): Promise<void> => {
    if (disposed || renegotiating || (dataChannel && dataChannel.readyState === 'open')) return;
    renegotiating = true;
    let connected: boolean;
    try {
      connected = await initWebRTC();
    } finally {
      renegotiating = false;
    }
    if (!connected && !disposed) {
      renegotiationAttempt += 1;
      scheduleRenegotiation('startup-retry', false);
    }
  };
  const onOnline = (): void => scheduleRenegotiation('online', true);
  const onPageShow = (): void => scheduleRenegotiation('pageshow', true);
  const onVisibility = (): void => {
    if (document.visibilityState === 'visible') scheduleRenegotiation('visibility', false);
  };
  if (typeof window.addEventListener === 'function') {
    window.addEventListener('online', onOnline);
    window.addEventListener('pageshow', onPageShow);
  }
  if (typeof document.addEventListener === 'function')
    document.addEventListener('visibilitychange', onVisibility);
  if (document.readyState === 'loading')
    document.addEventListener('DOMContentLoaded', startPersistentWebRTC);
  else void startPersistentWebRTC();

  return () => {
    if (disposed) return;
    disposed = true;
    window.removeEventListener('online', onOnline);
    window.removeEventListener('pageshow', onPageShow);
    document.removeEventListener('visibilitychange', onVisibility);
    document.removeEventListener('DOMContentLoaded', startPersistentWebRTC);
    if (renegotiationTimer !== null) clearTimeout(renegotiationTimer);
    renegotiationTimer = null;
    const channel = dataChannel;
    dataChannel = null;
    if (channel) {
      try {
        channel.close();
      } catch {
        /* ignore */
      }
    }
    if (window.fetch === patchedFetch) window.fetch = originalFetch;
    for (const request of [...pendingRequests.values()]) request.fallback();
    pendingRequests.clear();
  };
}

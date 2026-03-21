// app-webrtc.js — WebRTC direct-routing client.
//
// When window.__WEBRTC_ENABLED__ is set (injected by the server at startup),
// this module attempts to establish a WebRTC data channel directly to the
// home peer, bypassing the intermediate relay for all /v1/ API calls.
//
// Two-tier timeout:
//   1. First-frame timeout (1 s):  if no response frame arrives within 1 s,
//      the request seamlessly falls back to HTTPS and the channel is torn down
//      and renegotiated in the background.
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
// falls back to the normal HTTPS path; no user-visible error is shown.
// When the data channel later disconnects or errors the same silent fallback
// applies — all pending requests are rescued via HTTPS.
//
// Diagnostics mode: set window.__WEBRTC_DIAGNOSTICS__ = true (or pass
// ?webrtc_diag=1 in the URL) to enable console.log timeline output:
//   [webrtc] connection lifecycle events with timestamps
//   [webrtc] per-request: method, path, body size, status, latency
//
// Force TURN relay: pass ?webrtc_turn=1 to set iceTransportPolicy=relay,
// which forces all traffic through the TURN server (ignores host/srflx).

(function () {
  'use strict';

  if (!window.__WEBRTC_ENABLED__) return;

  const SIGNALING_URL = window.__WEBRTC_SIGNALING_URL__ || '';
  const UI_PREFIX = window.TERM_LLM_UI_PREFIX || '/ui';
  const ICE_TIMEOUT_MS = 8000;

  // If no response frame (headers/chunk/done) arrives within this window,
  // assume UDP is dead: fall back to HTTPS and renegotiate in the background.
  const RESPONSE_TIMEOUT_MS = 1000;

  // Once streaming has started, if no frame arrives within this window,
  // assume the channel silently died.  The backend sends keepalive pings
  // every ~20 s, so 30 s gives 10 s of grace before declaring death.
  const STREAM_WATCHDOG_MS = 30000;

  const originalFetch = window.fetch.bind(window);
  const encoder = new TextEncoder();

  // pendingRequests maps request-id → { onHeaders, onChunk, onDone, fallback, cleanup }
  const pendingRequests = new Map();

  let dataChannel = null;
  let renegotiating = false;

  // ---------------------------------------------------------------------------
  // Diagnostics
  // ---------------------------------------------------------------------------

  const _params = new URLSearchParams(window.location.search);
  const diagEnabled = !!(
    window.__WEBRTC_DIAGNOSTICS__ || _params.has('webrtc_diag')
  );
  const forceTurn = _params.has('webrtc_turn');

  // t0 is the timestamp when initWebRTC() starts, used for relative timings.
  let diagT0 = 0;

  function diag(msg) {
    if (!diagEnabled) return;
    const elapsed = diagT0 ? ((performance.now() - diagT0) | 0) : 0;
    console.log('[webrtc] +' + elapsed + 'ms ' + msg);
  }

  // ---------------------------------------------------------------------------
  // Initialisation
  // ---------------------------------------------------------------------------

  async function initWebRTC() {
    diagT0 = performance.now();
    diag('init signaling=' + SIGNALING_URL);
    try {
      // 1. Request a signaling session (no auth — session_id gates routing).
      const sessStart = performance.now();
      const sessResp = await originalFetch(SIGNALING_URL + '/session', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      });
      if (!sessResp.ok) {
        diag('session request failed status=' + sessResp.status);
        return;
      }
      const sess = await sessResp.json();
      diag('session created id=' + sess.session_id +
        (sess.turn_url ? ' turn=' + sess.turn_url : ' no-turn') +
        ' (' + ((performance.now() - sessStart) | 0) + 'ms)');

      // 2. Build ICE server list from session response.
      const iceServers = [
        { urls: sess.stun_url || 'stun:stun.l.google.com:19302' },
      ];
      if (sess.turn_url) {
        iceServers.push({
          urls: sess.turn_url,
          username: sess.turn_username,
          credential: sess.turn_credential,
        });
      }

      const pcConfig = { iceServers };
      if (forceTurn) {
        pcConfig.iceTransportPolicy = 'relay';
        diag('FORCED TURN — iceTransportPolicy=relay');
      }
      const pc = new RTCPeerConnection(pcConfig);

      pc.oniceconnectionstatechange = () => {
        diag('ICE state=' + pc.iceConnectionState);
      };

      // Log each ICE candidate as it is gathered.
      pc.onicecandidate = (e) => {
        if (e.candidate) {
          diag('ICE candidate: ' + e.candidate.type + ' ' +
            e.candidate.protocol + ' ' + e.candidate.address +
            ':' + e.candidate.port +
            (e.candidate.relatedAddress
              ? ' raddr=' + e.candidate.relatedAddress + ':' + e.candidate.relatedPort
              : ''));
        } else {
          diag('ICE candidate gathering done (null sentinel)');
        }
      };

      // 3. Browser creates the data channel (ordered, reliable).
      const dc = pc.createDataChannel('api', { ordered: true });

      // 4. Generate offer and wait for ICE gathering to complete so the SDP
      //    includes all candidates (vanilla ICE — no trickle).
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      diag('ICE gathering started');

      // Wait for ICE gathering to complete, but cap at 4 s so a slow/broken
      // STUN or TURN server (e.g. IPv6 timeout) never stalls the handshake.
      // Whatever candidates are ready at that point are included in the offer.
      await Promise.race([
        new Promise((resolve) => {
          if (pc.iceGatheringState === 'complete') { resolve(); return; }
          pc.onicegatheringstatechange = () => {
            if (pc.iceGatheringState === 'complete') resolve();
          };
        }),
        new Promise((resolve) => setTimeout(resolve, 4000)),
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
          sdp: pc.localDescription.sdp,
        }),
      });
      if (!sendResp.ok) {
        diag('offer post failed status=' + sendResp.status);
        return;
      }
      diag('offer sent (' + ((performance.now() - offerStart) | 0) + 'ms)');

      // 6. Poll for the home peer's answer (8-second timeout).
      const answer = await pollForAnswer(sess.session_id, ICE_TIMEOUT_MS);
      if (!answer) {
        diag('answer timeout — falling back to HTTPS');
        return; // timed out — fall back to HTTPS silently
      }
      diag('answer received');

      await pc.setRemoteDescription({ type: 'answer', sdp: answer.sdp });

      // 7. Wait for ICE connectivity and the data channel to open.
      await Promise.race([
        waitForDataChannelOpen(dc),
        new Promise((_, reject) =>
          setTimeout(() => reject(new Error('WebRTC connect timeout')), ICE_TIMEOUT_MS)
        ),
      ]);

      // 8. Connected — wire up handlers and patch fetch.
      dataChannel = dc;
      dc.onmessage = handleMessage;
      dc.onclose = onChannelClose;
      dc.onerror = onChannelClose;

      window.fetch = patchedFetch;

      diag('data channel open — fetch patched');

      if (typeof setConnectionState === 'function') {
        setConnectionState('\u26A1 direct', 'ok');
      }
    } catch (_e) {
      diag('init error: ' + (_e && _e.message ? _e.message : String(_e)));
      // Silent fallback — HTTPS continues to work for all requests.
    }
  }

  function waitForDataChannelOpen(dc) {
    return new Promise((resolve, reject) => {
      if (dc.readyState === 'open') { resolve(); return; }
      dc.onopen = resolve;
      dc.onerror = reject;
    });
  }

  async function pollForAnswer(sessionId, timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      try {
        const resp = await originalFetch(
          SIGNALING_URL + '/signal?session_id=' + encodeURIComponent(sessionId),
          { signal: AbortSignal.timeout(Math.min(remaining, 12000)) }
        );
        if (resp.status === 204 || resp.status === 408) continue;
        if (!resp.ok) return null;
        const msg = await resp.json();
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

  function handleMessage(event) {
    let frame;
    try { frame = JSON.parse(event.data); } catch (_e) { return; }

    const pending = pendingRequests.get(frame.id);
    if (!pending) return;

    if (frame.type === 'headers') {
      pending.onHeaders(frame.headers || {}, frame.status || 200);
    } else if (frame.type === 'chunk') {
      pending.onChunk(frame.data);
    } else if (frame.type === 'done') {
      pending.onDone(frame.status || 200);
      pendingRequests.delete(frame.id);
    }
  }

  // ---------------------------------------------------------------------------
  // Drain all pending requests to HTTPS
  // ---------------------------------------------------------------------------

  // Called when the channel dies (close/error/timeout).  Every in-flight
  // request that hasn't received its first response frame yet gets retried
  // via HTTPS.  Requests that already started streaming are closed — the
  // consumer will see a truncated stream and can retry at the app layer.
  function drainPendingToHTTPS(reason) {
    if (pendingRequests.size === 0) return;
    diag(reason + ' — draining ' + pendingRequests.size + ' pending request(s) to HTTPS');

    // Snapshot the entries; fallback() deletes its own key.
    const entries = Array.from(pendingRequests.values());
    for (const entry of entries) {
      entry.fallback();
    }
  }

  // ---------------------------------------------------------------------------
  // Channel close / error
  // ---------------------------------------------------------------------------

  function onChannelClose() {
    dataChannel = null;
    diag('data channel closed — restoring original fetch');
    window.fetch = originalFetch;
    if (typeof setConnectionState === 'function') {
      setConnectionState('Connected', 'ok');
    }
    drainPendingToHTTPS('channel closed');
  }

  // ---------------------------------------------------------------------------
  // Background renegotiation
  // ---------------------------------------------------------------------------

  function triggerRenegotiation() {
    if (renegotiating) return;
    renegotiating = true;

    // Tear down the current channel so new requests route to HTTPS immediately.
    if (dataChannel) {
      try { dataChannel.close(); } catch (_e) { /* ignore */ }
    }
    dataChannel = null;
    window.fetch = originalFetch;

    if (typeof setConnectionState === 'function') {
      setConnectionState('Connected', 'ok');
    }

    // Rescue any other in-flight requests stuck on the dead channel.
    drainPendingToHTTPS('renegotiation');

    diag('renegotiating — new requests use HTTPS');

    // Small delay before renegotiating to avoid tight loops if the network
    // is genuinely down.  2 s is enough to not spam but short enough that
    // if the issue was transient, WebRTC comes back quickly.
    setTimeout(async () => {
      try {
        await initWebRTC();
      } catch (_e) {
        diag('renegotiation failed: ' + (_e && _e.message ? _e.message : String(_e)));
      } finally {
        renegotiating = false;
      }
    }, 2000);
  }

  // ---------------------------------------------------------------------------
  // Patched fetch — routes /v1/ API calls over the data channel
  // ---------------------------------------------------------------------------

  function patchedFetch(url, options) {
    const urlStr = typeof url === 'string' ? url : url.toString();
    if (dataChannel && dataChannel.readyState === 'open' && isAPIPath(urlStr)) {
      // Only route string bodies (all term-llm API calls use JSON strings).
      if (!options || options.body === undefined || typeof options.body === 'string') {
        return webrtcFetch(urlStr, options || {});
      }
    }
    return originalFetch(url, options);
  }

  function isAPIPath(urlStr) {
    try {
      const path = new URL(urlStr, window.location.origin).pathname;
      return path.startsWith(UI_PREFIX + '/v1/');
    } catch (_e) {
      return false;
    }
  }

  function webrtcFetch(urlStr, options) {
    return new Promise((resolve) => {
      const reqId = crypto.randomUUID();
      let streamController;
      let resolved = false;
      let gotResponse = false;
      let cleaned = false;
      const reqStart = performance.now();

      const urlObj = new URL(urlStr, window.location.origin);
      const method = options.method || 'GET';
      const path = urlObj.pathname + (urlObj.search || '');
      const bodySize = options.body ? new Blob([options.body]).size : 0;

      diag('→ ' + method + ' ' + path + ' (' + bodySize + 'b)');

      const stream = new ReadableStream({
        start(ctrl) { streamController = ctrl; },
        cancel() { cleanup('stream-cancel'); },
      });

      let responseBytes = 0;
      let streamWatchdogId = null;

      function resolveOnce(response) {
        if (!resolved) { resolved = true; resolve(response); }
      }

      // Central cleanup — idempotent, called from every exit path.
      function cleanup(reason) {
        if (cleaned) return;
        cleaned = true;
        clearTimeout(responseTimer);
        clearTimeout(streamWatchdogId);
        pendingRequests.delete(reqId);
        if (abortHandler) {
          try { options.signal.removeEventListener('abort', abortHandler); } catch (_e) { /* */ }
        }
        diag('cleanup ' + method + ' ' + path + ' reason=' + reason);
      }

      function closeStream() {
        if (streamController) {
          try { streamController.close(); } catch (_e) { /* ignore */ }
          streamController = null;
        }
      }

      function errorStream(err) {
        if (streamController) {
          try { streamController.error(err); } catch (_e) { /* ignore */ }
          streamController = null;
        }
      }

      // --- Stream watchdog: resets on every frame after first response ---
      function resetStreamWatchdog() {
        clearTimeout(streamWatchdogId);
        streamWatchdogId = setTimeout(() => {
          diag('⚠ stream watchdog (' + STREAM_WATCHDOG_MS + 'ms) ' +
            method + ' ' + path + ' — closing stale stream');
          cleanup('stream-watchdog');
          closeStream();
          triggerRenegotiation();
        }, STREAM_WATCHDOG_MS);
      }

      // --- 1 s timeout: if no response frame arrives, fall back to HTTPS ---
      const responseTimer = setTimeout(() => {
        if (gotResponse) return; // already got data, all good

        diag('⚠ timeout (' + RESPONSE_TIMEOUT_MS + 'ms) ' + method + ' ' + path + ' — falling back to HTTPS');

        cleanup('first-frame-timeout');
        closeStream();

        // Fall back to HTTPS for this request.
        resolveOnce(originalFetch(urlStr, options));

        // Mark the channel as degraded and renegotiate in the background.
        // This also drains any other stuck pending requests.
        triggerRenegotiation();
      }, RESPONSE_TIMEOUT_MS);

      function markGotResponse() {
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
      let abortHandler = null;
      if (options.signal) {
        if (options.signal.aborted) {
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
        options.signal.addEventListener('abort', abortHandler, { once: true });
      }

      // fallback: called by drainPendingToHTTPS() when the channel dies.
      function fallback() {
        cleanup('drain-fallback');
        if (!gotResponse) {
          // Haven't received anything yet — retry cleanly via HTTPS.
          closeStream();
          diag('↩ fallback ' + method + ' ' + path);
          resolveOnce(originalFetch(urlStr, options));
        } else {
          // Already streaming — close the stream; consumer sees truncation.
          // App-layer resume logic will reconnect via HTTPS.
          closeStream();
        }
      }

      pendingRequests.set(reqId, {
        onHeaders(headers, status) {
          markGotResponse();
          resolveOnce(new Response(stream, { status, headers: new Headers(headers) }));
        },
        onChunk(line) {
          markGotResponse();
          if (!resolved) {
            resolveOnce(new Response(stream, { status: 200 }));
          }
          responseBytes += (line ? line.length : 0) + 1; // +1 for the \n
          if (streamController) {
            streamController.enqueue(encoder.encode(line + '\n'));
          }
        },
        onDone(status) {
          markGotResponse();
          const latency = (performance.now() - reqStart) | 0;
          diag('← ' + status + ' ' + method + ' ' + path +
            ' (' + responseBytes + 'b, ' + latency + 'ms)');
          cleanup('done');
          if (!resolved) {
            resolveOnce(new Response(stream, { status }));
          }
          closeStream();
        },
        fallback,
      });

      // Build and send the request frame.
      const headersObj = {};

      // Carry over all request headers (Authorization, session_id, Content-Type, …).
      if (options.headers) {
        const h = options.headers instanceof Headers
          ? options.headers
          : new Headers(options.headers);
        for (const [k, v] of h.entries()) headersObj[k] = v;
      }

      const frame = {
        id: reqId,
        method,
        path,
        headers: Object.keys(headersObj).length ? headersObj : undefined,
        body: options.body ? strToBase64(options.body) : undefined,
      };

      try {
        dataChannel.send(JSON.stringify(frame));
      } catch (_e) {
        diag('send error: ' + (_e && _e.message ? _e.message : String(_e)));
        cleanup('send-error');
        closeStream();
        resolveOnce(originalFetch(urlStr, options));
        triggerRenegotiation();
      }
    });
  }

  // UTF-8–safe base64 encoding (handles multi-byte characters).
  function strToBase64(str) {
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

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initWebRTC);
  } else {
    initWebRTC();
  }
}());

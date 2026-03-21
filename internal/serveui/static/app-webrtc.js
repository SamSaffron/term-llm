// app-webrtc.js — WebRTC direct-routing client.
//
// When window.__WEBRTC_ENABLED__ is set (injected by the server at startup),
// this module attempts to establish a WebRTC data channel directly to the
// home peer, bypassing the intermediate relay for all /v1/ API calls.
//
// If ICE negotiation does not complete within 8 seconds the browser silently
// falls back to the normal HTTPS path; no user-visible error is shown.
// When the data channel later disconnects or errors the same silent fallback
// applies to all subsequent requests.
//
// Diagnostics mode: set window.__WEBRTC_DIAGNOSTICS__ = true (or pass
// ?webrtc_diag=1 in the URL) to enable console.log timeline output:
//   [webrtc] connection lifecycle events with timestamps
//   [webrtc] per-request: method, path, body size, status, latency

(function () {
  'use strict';

  if (!window.__WEBRTC_ENABLED__) return;

  const SIGNALING_URL = window.__WEBRTC_SIGNALING_URL__ || '';
  const UI_PREFIX = window.TERM_LLM_UI_PREFIX || '/ui';
  const ICE_TIMEOUT_MS = 8000;

  const originalFetch = window.fetch.bind(window);
  const encoder = new TextEncoder();

  // pendingRequests maps request-id → { onHeaders, onChunk, onDone }
  const pendingRequests = new Map();

  let dataChannel = null;

  // ---------------------------------------------------------------------------
  // Diagnostics
  // ---------------------------------------------------------------------------

  const diagEnabled = !!(
    window.__WEBRTC_DIAGNOSTICS__ ||
    new URLSearchParams(window.location.search).has('webrtc_diag')
  );

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
        (sess.turn_url ? ' turn=' + sess.turn_url : ' no-turn'));

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

      const pc = new RTCPeerConnection({ iceServers });

      pc.oniceconnectionstatechange = () => {
        diag('ICE state=' + pc.iceConnectionState);
      };

      // 3. Browser creates the data channel (ordered, reliable).
      const dc = pc.createDataChannel('api', { ordered: true });

      // 4. Generate offer and wait for ICE gathering to complete so the SDP
      //    includes all candidates (vanilla ICE — no trickle).
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      diag('ICE gathering started');

      await new Promise((resolve) => {
        if (pc.iceGatheringState === 'complete') { resolve(); return; }
        pc.onicegatheringstatechange = () => {
          if (pc.iceGatheringState === 'complete') resolve();
        };
      });
      diag('ICE gathering complete');

      // 5. Send the completed offer to the signaling server.
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
      diag('offer sent');

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

  function onChannelClose() {
    dataChannel = null;
    diag('data channel closed — restoring original fetch');
    // Restore native fetch so subsequent requests use HTTPS.
    window.fetch = originalFetch;
    if (typeof setConnectionState === 'function') {
      setConnectionState('Connected', 'ok');
    }
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
      const reqStart = performance.now();

      const urlObj = new URL(urlStr, window.location.origin);
      const method = options.method || 'GET';
      const path = urlObj.pathname + (urlObj.search || '');
      const bodySize = options.body ? new Blob([options.body]).size : 0;

      diag('→ ' + method + ' ' + path + ' (' + bodySize + 'b)');

      const stream = new ReadableStream({
        start(ctrl) { streamController = ctrl; },
        cancel() { pendingRequests.delete(reqId); },
      });

      let responseBytes = 0;

      function resolveOnce(response) {
        if (!resolved) { resolved = true; resolve(response); }
      }

      pendingRequests.set(reqId, {
        onHeaders(headers, status) {
          resolveOnce(new Response(stream, { status, headers: new Headers(headers) }));
        },
        onChunk(line) {
          resolveOnce(new Response(stream, { status: 200 }));
          responseBytes += (line ? line.length : 0) + 1; // +1 for the \n
          if (streamController) {
            streamController.enqueue(encoder.encode(line + '\n'));
          }
        },
        onDone(status) {
          const latency = (performance.now() - reqStart) | 0;
          diag('← ' + status + ' ' + method + ' ' + path +
            ' (' + responseBytes + 'b, ' + latency + 'ms)');
          resolveOnce(new Response(stream, { status }));
          if (streamController) streamController.close();
          pendingRequests.delete(reqId);
        },
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
        // Channel error — fall back to HTTPS for this request.
        pendingRequests.delete(reqId);
        if (streamController) streamController.close();
        resolve(originalFetch(urlStr, options));
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

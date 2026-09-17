import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import type { LiveStore } from '../stores/live-store';

export function LiveStatus({ live }: { live: LiveStore }) {
  const phase = live.phase.value;
  const working = live.working.value;
  const error = live.lastError.value;
  const recentTurns = live.recentTurns.value;
  const transcriptTurns = live.transcriptTurns;
  const partialUser = live.partialUser.value;
  const partialAssistant = live.partialAssistant.value;
  const lastTurn = recentTurns.at(-1);
  const text = partialAssistant || partialUser || lastTurn?.text || '';
  const role = partialAssistant ? 'assistant' : partialUser ? 'user' : lastTurn?.role;
  const interrupted = !partialAssistant && !partialUser && lastTurn?.interrupted;
  const [expanded, setExpanded] = useState(false);
  const [following, setFollowing] = useState(true);
  const viewport = useRef<HTMLDivElement>(null);
  const follow = useRef(true);
  // The call owns the transcript, not the session: a switch inside one
  // continuous call must not collapse or re-scroll what the user was reading.
  const owner = live.liveId.value;
  const sessionNumber = live.sessionNumber.value;
  const sessionTitle = live.sessionTitle.value;
  const binding = [sessionNumber > 0 ? `#${sessionNumber}` : '', sessionTitle]
    .filter(Boolean)
    .join(' · ');

  useLayoutEffect(() => {
    follow.current = true;
    setFollowing(true);
    setExpanded(false);
  }, [owner]);
  useLayoutEffect(() => {
    const element = viewport.current;
    if (element && follow.current) element.scrollTop = element.scrollHeight;
  }, [text, expanded, owner, recentTurns, partialUser, partialAssistant]);

  const latest = () => {
    follow.current = true;
    setFollowing(true);
    if (viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight;
  };
  if (phase === 'idle' || phase === 'ended') return null;
  return (
    <section id="liveStatus" class={`live-status live-status-${phase}`} aria-label="Live voice">
      <div class="live-status-header">
        <span
          class={working || phase === 'connecting' ? 'live-status-spinner' : 'live-status-dot'}
          aria-hidden="true"
        />
        <span class="live-status-copy" role="status" aria-live="polite">
          {phase === 'requesting-permission' && 'Requesting microphone access…'}
          {phase === 'connecting' && 'Connecting live voice…'}
          {phase === 'listening' && 'Listening…'}
          {phase === 'speaking' && 'Speaking…'}
          {phase === 'working' && 'Working on your request…'}
          {phase === 'failed' && 'Live voice needs attention.'}
          {/* Inside the polite live region: a move between sessions changes
              this text, which is how a screen reader learns the context did. */}
          {binding && <span class="live-status-binding">Working in {binding}</span>}
        </span>
        {live.active.value && (
          <button type="button" class="btn live-status-stop" onClick={() => void live.stop()}>
            Stop
          </button>
        )}
      </div>
      {text && (
        <>
          <div
            id="liveTranscript"
            ref={viewport}
            class={`live-status-transcript${expanded ? ' live-status-transcript-expanded' : ''}`}
            role="region"
            aria-label="Live transcript"
            tabIndex={0}
            onScroll={() => {
              const element = viewport.current;
              if (!element) return;
              const atBottom = element.scrollHeight - element.clientHeight - element.scrollTop <= 8;
              follow.current = atBottom;
              setFollowing(atBottom);
            }}
          >
            {expanded ? (
              <>
                {transcriptTurns.map((turn, index) => (
                  <div class="live-status-turn" key={`${index}-${turn.role}`}>
                    <span class="live-status-speaker">
                      {turn.role === 'user' ? 'You: ' : 'Assistant: '}
                    </span>
                    {turn.text}
                    {turn.interrupted && (
                      <span class="live-status-interrupted"> (Interrupted)</span>
                    )}
                  </div>
                ))}
              </>
            ) : (
              <>
                <span class="live-status-speaker">{role === 'user' ? 'You: ' : 'Assistant: '}</span>
                {text}
                {interrupted && <span class="live-status-interrupted"> (Interrupted)</span>}
              </>
            )}
          </div>
          <div class="live-status-transcript-actions">
            {!following && (
              <button type="button" class="btn" onClick={latest}>
                ↓ Latest
              </button>
            )}
            <button
              type="button"
              class="btn"
              aria-controls="liveTranscript"
              aria-expanded={expanded}
              onClick={() => setExpanded(!expanded)}
            >
              {expanded ? 'Collapse transcript' : 'Expand transcript'}
            </button>
          </div>
        </>
      )}
      {error && (
        <span class="live-status-error" role="alert">
          {error}
        </span>
      )}
    </section>
  );
}

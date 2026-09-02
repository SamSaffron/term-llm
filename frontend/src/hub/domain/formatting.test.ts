import { describe, expect, it } from 'vitest';
import {
  activeSessionCount,
  buildRegistrationCommand,
  nodeResumePath,
  relativeSessionTime,
  sessionMetaText,
  shellQuote,
} from './formatting';
import type { HubNode } from './types';

const node = (sessions: HubNode['sessions']): HubNode =>
  ({ id: 'n', name: 'N', status: { reachable: true }, sessions }) as HubNode;

describe('Hub formatting', () => {
  it('formats relative times and session metadata', () => {
    const now = new Date('2026-09-02T12:00:00Z').getTime();
    expect(relativeSessionTime(now - 10_000, now)).toBe('just now');
    expect(relativeSessionTime(now - 5 * 60_000, now)).toBe('5m ago');
    expect(relativeSessionTime(now - 3 * 3_600_000, now)).toBe('3h ago');
    expect(relativeSessionTime(now - 2 * 86_400_000, now)).toBe('2d ago');
    expect(relativeSessionTime(0, now)).toBe('');
    expect(
      sessionMetaText(
        {
          id: 's',
          short_title: 'Session',
          resume_path: '/resume',
          interaction_required: true,
          pending_interaction_count: 2,
          message_count: 1,
        },
        { now },
      ),
    ).toBe('2 decisions waiting · 1 message');
  });

  it('uses the documented resume fallback and active counts', () => {
    expect(
      nodeResumePath(
        node({ count_label: '', recent: [{ id: 's', short_title: '', resume_path: '/recent' }] }),
      ),
    ).toBe('/recent');
    expect(
      activeSessionCount([
        node({ count_label: '', active_count: 3, active: [] }),
        node({ count_label: '', active: [{ id: 's', short_title: '', resume_path: '/' }] }),
      ]),
    ).toBe(4);
  });

  it('shell-quotes apostrophes and builds a reverse registration command', () => {
    expect(shellQuote("https://hub.test/o'connor/")).toBe("'https://hub.test/o'\\''connor/'");
    const command = buildRegistrationCommand("https://hub.test/o'connor/");
    expect(command).toContain("export HUB_URL='https://hub.test/o'\\''connor/'");
    expect(command).toContain('--hub-connect reverse');
    expect(command).toContain('--hub-registration-token "$HUB_REGISTRATION_TOKEN"');
  });
});

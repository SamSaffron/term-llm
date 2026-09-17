import { act, fireEvent, render, screen } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import type { Endpoints } from '../api/endpoints';
import { LiveStore } from '../stores/live-store';
import { LiveStatus } from './LiveStatus';

function setup() {
  const live = new LiveStore({} as Endpoints, () => 'session');
  live.liveId.value = 'call';
  live.sessionId.value = 'session';
  live.phase.value = 'speaking';
  live.partialAssistant.value = 'A long response '.repeat(100);
  live.stop = vi.fn(async () => undefined);
  render(<LiveStatus live={live} />);
  return live;
}

describe('LiveStatus', () => {
  it('shows the complete text, expands in place, and keeps Stop available', () => {
    const live = setup();
    const region = screen.getByRole('region', { name: 'Live transcript' });
    expect(region.textContent).toBe(`Assistant: ${live.partialAssistant.value}`);
    expect(region).toHaveAttribute('tabindex', '0');
    const stop = screen.getByRole('button', { name: 'Stop' });
    fireEvent.click(screen.getByRole('button', { name: 'Expand transcript' }));
    expect(screen.getByRole('button', { name: 'Collapse transcript' })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    expect(region).toHaveClass('live-status-transcript-expanded');
    expect(screen.getByRole('button', { name: 'Stop' })).toBe(stop);
    fireEvent.click(screen.getByRole('button', { name: 'Collapse transcript' }));
    expect(region).not.toHaveClass('live-status-transcript-expanded');
    fireEvent.click(stop);
    expect(live.stop).toHaveBeenCalledOnce();
  });

  it('keeps the completed turn visible while listening and replaces it with new speech', () => {
    const live = setup();
    act(() => {
      live.recentTurns.value = [{ role: 'assistant', text: 'Completed response.' }];
      live.partialAssistant.value = '';
      live.phase.value = 'listening';
    });
    expect(screen.getByRole('status')).toHaveTextContent('Listening…');
    expect(screen.getByRole('region', { name: 'Live transcript' })).toHaveTextContent(
      'Assistant: Completed response.',
    );
    act(() => {
      live.partialUser.value = 'My next question';
    });
    expect(screen.getByRole('region', { name: 'Live transcript' })).toHaveTextContent(
      'You: My next question',
    );
    expect(screen.getByRole('region', { name: 'Live transcript' })).not.toHaveAttribute(
      'aria-live',
    );
  });

  it('follows new text only at the bottom and lets the reader return to Latest', () => {
    const live = setup();
    const region = screen.getByRole('region', { name: 'Live transcript' });
    let height = 500;
    Object.defineProperties(region, {
      scrollHeight: { get: () => height },
      clientHeight: { value: 100 },
    });
    act(() => {
      live.partialAssistant.value += 'more';
    });
    expect(region.scrollTop).toBe(500);
    region.scrollTop = 50;
    fireEvent.scroll(region);
    height = 700;
    act(() => {
      live.partialAssistant.value += 'still more';
    });
    expect(region.scrollTop).toBe(50);
    fireEvent.click(screen.getByRole('button', { name: 'Expand transcript' }));
    expect(region.scrollTop).toBe(50);
    fireEvent.click(screen.getByRole('button', { name: '↓ Latest' }));
    expect(region.scrollTop).toBe(700);
    expect(screen.queryByRole('button', { name: '↓ Latest' })).not.toBeInTheDocument();
    height = 800;
    act(() => {
      live.partialAssistant.value += 'tail';
    });
    expect(region.scrollTop).toBe(800);
    region.scrollTop = 0;
    fireEvent.scroll(region);
    region.scrollTop = 700;
    fireEvent.scroll(region);
    expect(screen.queryByRole('button', { name: '↓ Latest' })).not.toBeInTheDocument();
  });

  it('expands to both sides of recent conversation and includes in-progress speech', () => {
    const live = setup();
    act(() => {
      live.recentTurns.value = [
        { role: 'user', text: 'First question' },
        { role: 'assistant', text: 'First answer' },
        { role: 'user', text: 'Second question' },
        { role: 'assistant', text: 'Second answer' },
      ];
      live.partialUser.value = 'New question';
      live.partialAssistant.value = 'New answer in progress';
    });
    const region = screen.getByRole('region', { name: 'Live transcript' });
    expect(region).not.toHaveTextContent('First question');
    fireEvent.click(screen.getByRole('button', { name: 'Expand transcript' }));
    expect(
      Array.from(region.querySelectorAll('.live-status-turn'), (turn) => turn.textContent),
    ).toEqual([
      'You: First question',
      'Assistant: First answer',
      'You: Second question',
      'Assistant: Second answer',
      'You: New question',
      'Assistant: New answer in progress',
    ]);
    act(() => {
      live.recentTurns.value = [
        ...live.recentTurns.value,
        { role: 'user', text: 'New question' },
        { role: 'assistant', text: 'Final new answer' },
      ];
      live.partialUser.value = '';
      live.partialAssistant.value = '';
      live.phase.value = 'listening';
    });
    expect(region.querySelectorAll('.live-status-turn')).toHaveLength(6);
    expect(region).not.toHaveTextContent('New answer in progress');
    expect(region).toHaveTextContent('Final new answer');
    fireEvent.click(screen.getByRole('button', { name: 'Collapse transcript' }));
    expect(region.textContent).toBe('Assistant: Final new answer');
  });

  it('follows completed history updates without pulling a reader away from older turns', () => {
    const live = setup();
    act(() => {
      live.partialAssistant.value = '';
      live.recentTurns.value = [{ role: 'assistant', text: 'Same last turn' }];
    });
    fireEvent.click(screen.getByRole('button', { name: 'Expand transcript' }));
    const region = screen.getByRole('region', { name: 'Live transcript' });
    let height = 500;
    Object.defineProperties(region, {
      scrollHeight: { get: () => height },
      clientHeight: { value: 100 },
    });
    act(() => {
      live.recentTurns.value = [{ role: 'user', text: 'Earlier turn' }, ...live.recentTurns.value];
    });
    expect(region.scrollTop).toBe(500);
    region.scrollTop = 25;
    fireEvent.scroll(region);
    height = 600;
    act(() => {
      live.recentTurns.value = [...live.recentTurns.value, { role: 'user', text: 'Next question' }];
    });
    expect(region.scrollTop).toBe(25);
    expect(region).toHaveTextContent('Next question');
    fireEvent.click(screen.getByRole('button', { name: '↓ Latest' }));
    expect(region.scrollTop).toBe(600);
  });

  it('keeps an interrupted answer visible and labeled when reading back', () => {
    const live = setup();
    act(() => {
      live.partialAssistant.value = '';
      live.recentTurns.value = [
        { role: 'assistant', text: 'An unfinished explanation', interrupted: true },
      ];
      live.phase.value = 'listening';
    });
    const region = screen.getByRole('region', { name: 'Live transcript' });
    expect(region).toHaveTextContent('Assistant: An unfinished explanation (Interrupted)');
    act(() => {
      live.partialUser.value = 'Wait, go back';
    });
    fireEvent.click(screen.getByRole('button', { name: 'Expand transcript' }));
    expect(region).toHaveTextContent('An unfinished explanation (Interrupted)');
    expect(region).toHaveTextContent('You: Wait, go back');
  });

  it('renders a late-finalized story prompt above the interrupted story', () => {
    const live = setup();
    const receive = (
      live as unknown as {
        applyEvent(event: string, data: object): void;
      }
    ).applyEvent.bind(live);
    act(() => {
      live.partialAssistant.value = '';
      receive('live.transcript', { role: 'user', text: 'Tell me a funny story.' });
      receive('live.transcript', { role: 'assistant', text: 'A man bought a parrot...' });
      receive('live.interrupted', {});
    });
    fireEvent.click(screen.getByRole('button', { name: 'Expand transcript' }));
    const region = screen.getByRole('region', { name: 'Live transcript' });
    const texts = () =>
      Array.from(region.querySelectorAll('.live-status-turn'), (t) => t.textContent);
    expect(texts()).toEqual([
      'You: Tell me a funny story.',
      'Assistant: A man bought a parrot... (Interrupted)',
    ]);
    act(() => {
      receive('live.transcript', { role: 'user', text: 'Tell me a funny story.', final: true });
      receive('live.transcript', { role: 'user', text: 'Stop.' });
    });
    expect(texts()).toEqual([
      'You: Tell me a funny story.',
      'Assistant: A man bought a parrot... (Interrupted)',
      'You: Stop.',
    ]);
  });

  it('uses a single working label and keeps badge styling off the panel', () => {
    const live = setup();
    act(() => {
      live.phase.value = 'working';
      live.delegation.value = { delegationId: 'job', state: 'running' };
    });
    expect(screen.getByRole('status')).toHaveTextContent('Working on your request…');
    expect(screen.queryByText('Working…')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument();
  });

  it('resets panel preferences for a new call and hides after ending', () => {
    const live = setup();
    fireEvent.click(screen.getByRole('button', { name: 'Expand transcript' }));
    act(() => {
      live.liveId.value = 'new-call';
    });
    expect(screen.getByRole('button', { name: 'Expand transcript' })).toHaveAttribute(
      'aria-expanded',
      'false',
    );
    act(() => {
      live.phase.value = 'ended';
    });
    expect(screen.queryByRole('region', { name: 'Live transcript' })).not.toBeInTheDocument();
  });
});

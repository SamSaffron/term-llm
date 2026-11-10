import { render, screen } from '@testing-library/preact';
import { afterEach, expect, it, vi } from 'vitest';

afterEach(() => {
  vi.doUnmock('./NewChatControls');
  vi.resetModules();
});

it('offers recovery when the draft-control chunk fails to load', async () => {
  vi.doMock('./NewChatControls', () => {
    throw new Error('chunk unavailable');
  });
  const { NewChatControls } = await import('./Transcript');
  render(<NewChatControls />);
  expect(await screen.findByRole('button', { name: 'Reload chat controls' })).toBeVisible();
  expect(screen.getByRole('alert')).toHaveTextContent('Reload chat controls');
});

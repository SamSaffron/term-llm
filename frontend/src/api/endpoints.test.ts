import { describe, expect, it, vi } from 'vitest';
import type { APIClient } from './client';
import { endpoints } from './endpoints';

describe('branch tree endpoint', () => {
  it('requests branch points only for the interactive tree browser', async () => {
    const get = vi.fn(async () => ({}));
    const routes = endpoints({ get } as unknown as APIClient);

    await routes.tree('session/one');
    expect(get).toHaveBeenLastCalledWith('/v1/sessions/session%2Fone/tree', undefined);

    await routes.tree('session/one', undefined, true);
    expect(get).toHaveBeenLastCalledWith(
      '/v1/sessions/session%2Fone/tree?include_branch_points=1',
      undefined,
    );
  });
});

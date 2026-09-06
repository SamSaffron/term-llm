import { afterEach, describe, expect, it, vi } from 'vitest';
import { readInjectedConfig } from '../app/config';
import { APIClient } from './client';

const config = readInjectedConfig({ TERM_LLM_UI_PREFIX: '/ui' } as Window);
afterEach(() => vi.unstubAllGlobals());

describe('fetch and upload error contracts', () => {
  it.each([
    { raw: 'plain failure', message: 'plain failure', fetchType: '', uploadType: '' },
    { raw: '{bad json', message: '{bad json', fetchType: '', uploadType: '' },
    {
      raw: '{"error":"failed","type":"outer"}',
      message: 'failed',
      fetchType: 'outer',
      uploadType: 'outer',
    },
    {
      raw: '{"error":{"message":"nested","code":"inner"}}',
      message: 'nested',
      fetchType: 'inner',
      uploadType: 'inner',
    },
    {
      raw: '{"message":"failed","code":"outer"}',
      message: 'failed',
      fetchType: 'outer',
      uploadType: '',
    },
    {
      raw: '{"error":null,"message":"ignored"}',
      message: '{"error":null,"message":"ignored"}',
      fetchType: '',
      uploadType: '',
    },
    { raw: '', message: '', fetchType: '', uploadType: '' },
  ])('preserves error decoding for $raw', async ({ raw, message, fetchType, uploadType }) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(raw, { status: 400, statusText: 'Bad Request' })),
    );
    vi.stubGlobal(
      'XMLHttpRequest',
      class {
        status = 400;
        responseText = raw;
        upload = {};
        open = vi.fn();
        setRequestHeader = vi.fn();
        getAllResponseHeaders = () => '';
        onload = () => {};
        send() {
          this.onload();
        }
      },
    );
    const api = new APIClient(config, { getToken: () => '', onAuthRequired: vi.fn() });
    await expect(api.get('/v1/test')).rejects.toMatchObject({
      name: 'APIError',
      status: 400,
      body: raw,
      message: message || '400 Bad Request',
      type: fetchType,
    });
    await expect(api.upload('/v1/upload', new FormData())).rejects.toMatchObject({
      name: 'APIError',
      status: 400,
      body: raw,
      message: message || 'Upload returned 400',
      type: uploadType,
    });
  });
});

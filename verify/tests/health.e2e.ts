import { describe, expect, test } from './support';

describe('health', () => {
  test('GET /healthz answers ok', async ({ faucet }) => {
    const reply = await faucet.request('/healthz');

    expect(reply.status).toBe(200);
    expect(reply.body).toBe('ok\n');
  });

  test('POST /healthz is not allowed', async ({ faucet }) => {
    const reply = await faucet.request('/healthz', { method: 'POST' });

    expect(reply.status).toBe(405);
  });

  test('an unknown path is not found', async ({ faucet }) => {
    const reply = await faucet.request('/no-such-route');

    expect(reply.status).toBe(404);
  });
});

import { readFileSync } from 'node:fs';
import { describe, expect, test } from './support';

describe('smoke', () => {
  test('the server answers and its database holds the migrated table', async ({ faucet }) => {
    const health = await faucet.request('/healthz');
    const table = await faucet.sql("select to_regclass('public.faucet_events') is not null");

    expect(health.status).toBe(200);
    expect(health.body).toBe('ok\n');
    expect(table).toBe('t');
  });

  test('the app log never holds the webhook secret', async ({ faucet }) => {
    const log = readFileSync('.e2e/logs/app.log', 'utf8');

    expect(log).toContain('listening');
    expect(faucet.leaksSecret(log)).toBe(false);
  });
});

import { readFileSync } from 'node:fs';
import { counts, endpoint, eventRequests, paramsFor, parseReply, storeRun, withDeliveryFaucet } from './delivery';
import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type CronOptions, type Faucet } from './support';
import { subscribed } from './subscriptions';

const run = samples['workflow_run.completed'];
const OLD_DAYS = 40;

const refused: [string, NonNullable<CronOptions['send']>][] = [
  ['no Authorization header', () => ({})],
  ['a wrong secret as long as the right one', (secret) => ({ authorization: `Bearer ${'0'.repeat(secret.length)}` })],
  ['the right secret as a Basic credential', (secret) => ({ authorization: `Basic ${secret}` })],
  ['the right secret without a scheme', (secret) => ({ authorization: secret })],
  ['the right secret one character short', (secret) => ({ authorization: `Bearer ${secret.slice(0, -1)}` })],
  ['the right secret only in the query string', (secret) => ({ query: `secret=${secret}&token=${secret}` })],
];

interface Armed {
  repoId: number;
  old: string;
}

// arm stores one event old enough for a purge to delete.
async function arm(cron: Faucet): Promise<Armed> {
  const repoId = newRepoId();
  const old = newDeliveryId();
  const stored = await cron.deliver({ event: run.event, delivery: old, body: JSON.stringify(run.payload(repoId)) });
  expect(stored.body).toBe('stored\n');
  await cron.backdate(old, OLD_DAYS);
  return { repoId, old };
}

async function deliveriesOf(cron: Faucet, repoId: number): Promise<string[]> {
  return (await cron.rows(repoId)).map((row) => row.delivery_id);
}

describe('cron routes', () => {
  for (const [title, send] of refused) {
    test(`GET /cron/purge with ${title} is 401 and runs nothing`, async ({ cron }) => {
      const armed = await arm(cron);

      const reply = await cron.cronJob('purge', { send });

      expect(reply.status).toBe(401);
      expect(reply.body).toBe('unauthorized\n');
      expect(reply.headers.get('www-authenticate')).toBe('Bearer');
      expect(cron.leaksSecret(reply.body + JSON.stringify([...reply.headers]))).toBe(false);
      expect(await deliveriesOf(cron, armed.repoId), 'the old event must stay').toEqual([armed.old]);
    });
  }

  for (const method of ['POST', 'PUT', 'DELETE']) {
    test(`${method} /cron/purge with the right secret is 405 and runs nothing`, async ({ cron }) => {
      const armed = await arm(cron);

      const reply = await cron.cronJob('purge', { method });

      expect(reply.status).toBe(405);
      expect(await deliveriesOf(cron, armed.repoId), 'the old event must stay').toEqual([armed.old]);
    });
  }

  test('GET /cron/purge with the right secret purges and answers JSON without the secret', async ({ cron }) => {
    const armed = await arm(cron);

    const reply = await cron.cronJob('purge');

    expect(reply.status).toBe(200);
    expect(reply.headers.get('content-type')).toContain('application/json');
    expect(JSON.parse(reply.body)).toEqual({
      deleted: expect.any(Number),
      purged_through: expect.any(Number),
      expired_subscriptions: expect.any(Number),
      ended_deliveries: expect.any(Number),
    });
    expect(await deliveriesOf(cron, armed.repoId)).toEqual([]);
    expect(cron.leaksSecret(reply.body + JSON.stringify([...reply.headers]))).toBe(false);
  });

  test('a faucet without CRON_SECRET answers /cron/purge 503 to every request and runs nothing', async ({ faucet }) => {
    const repoId = newRepoId();
    const old = newDeliveryId();
    const stored = await faucet.deliver({ event: run.event, delivery: old, body: JSON.stringify(run.payload(repoId)) });
    expect(stored.body).toBe('stored\n');
    await faucet.backdate(old, OLD_DAYS);
    const guesses: [string, NonNullable<CronOptions['send']>][] = [
      ['no header', () => ({})],
      ['a bearer token', (secret) => ({ authorization: `Bearer ${secret}` })],
      ['an empty bearer token', () => ({ authorization: 'Bearer' })],
    ];

    try {
      for (const [title, send] of guesses) {
        const reply = await faucet.cronJob('purge', { send });
        expect(reply.status, title).toBe(503);
        expect(reply.body, title).toBe('cron is not configured\n');
        expect(faucet.leaksSecret(reply.body)).toBe(false);
      }
      expect(await deliveriesOf(faucet, repoId), 'a purge must not run').toEqual([old]);
    } finally {
      await faucet.sql(`delete from faucet_events where delivery_id = '${old}'`);
    }
  });

  for (const [title, send] of refused) {
    test(`GET /cron/redeliver with ${title} is 401 and asks GitHub for nothing`, async ({ cron, hooks }) => {
      await cron.withFaucet(cron.hookEnv(), async (started) => {
        await hooks.arm(started, []);

        const reply = await started.cronJob('redeliver', { send });

        expect(reply.status).toBe(401);
        expect(reply.body).toBe('unauthorized\n');
        expect(reply.headers.get('www-authenticate')).toBe('Bearer');
        expect(started.leaksSecret(reply.body + JSON.stringify([...reply.headers]))).toBe(false);
        expect(await hooks.requests(), 'a refused call must not reach GitHub').toEqual([]);
      });
    });
  }

  for (const method of ['POST', 'PUT', 'DELETE']) {
    test(`${method} /cron/redeliver with the right secret is 405 and asks GitHub for nothing`, async ({ cron, hooks }) => {
      await cron.withFaucet(cron.hookEnv(), async (started) => {
        await hooks.arm(started, []);

        const reply = await started.cronJob('redeliver', { method });

        expect(reply.status).toBe(405);
        expect(await hooks.requests()).toEqual([]);
      });
    });
  }

  test('a faucet with a cron secret and no hook settings answers /cron/redeliver 503 after it checks the secret', async ({ cron }) => {
    const wrong = await cron.cronJob('redeliver', { send: () => ({ authorization: `Bearer ${'0'.repeat(64)}` }) });
    const right = await cron.cronJob('redeliver');
    const purge = await cron.cronJob('purge');

    expect(wrong.status, 'a caller without the secret learns nothing about the config').toBe(401);
    expect(right.status).toBe(503);
    expect(right.body).toBe('redelivery is not configured\n');
    expect(purge.status, 'purge does not need the hook settings').toBe(200);
    expect(cron.leaksSecret(right.body)).toBe(false);
  });

  test('GET /cron/deliver with the right secret on a faucet without webhook mode is 503 and sends nothing', async ({ cron }) => {
    const reply = await cron.cronJob('deliver');

    expect(reply.status).toBe(503);
    expect(reply.body).toBe('delivery is not configured\n');
    expect(cron.leaksSecret(reply.body + JSON.stringify([...reply.headers]))).toBe(false);
  });

  test('GET /cron/deliver on a faucet with webhook mode sends nothing for a refused request, and what waited for the right secret', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      await subscribed(started, paramsFor(repoId, at));
      await storeRun(started, repoId, 9001);

      for (const [title, send] of refused) {
        const reply = await started.cronJob('deliver', { send });
        expect(reply.status, title).toBe(401);
        expect(reply.body, title).toBe('unauthorized\n');
        expect(started.leaksSecret(reply.body)).toBe(false);
      }
      expect(await eventRequests(receiver, at), 'no refused request sent a webhook').toEqual([]);
      expect(await started.deliveries(), 'and none queued one').toEqual([]);

      expect(parseReply(await started.cronJob('deliver'))).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, delivered: 1 }));
      expect(await eventRequests(receiver, at)).toHaveLength(1);
    });
  });

  test('a faucet without CRON_SECRET answers /cron/deliver 503 to every request and runs nothing', async ({ faucet }) => {
    for (const send of [() => ({}), (secret: string) => ({ authorization: `Bearer ${secret}` })]) {
      const reply = await faucet.cronJob('deliver', { send });
      expect(reply.status).toBe(503);
      expect(reply.body).toBe('cron is not configured\n');
      expect(faucet.leaksSecret(reply.body)).toBe(false);
    }
  });

  test('the app log records the purge and never holds the cron secret', async ({ cron }) => {
    const log = readFileSync('.e2e/logs/app.log', 'utf8');

    expect(log).toContain('events purged');
    expect(cron.leaksSecret(log)).toBe(false);
  });
});

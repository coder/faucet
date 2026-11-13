import { readFileSync } from 'node:fs';
import { samples, stubDelivery } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type CronOptions, type Faucet, type HookStub } from './support';

const JOBS = ['purge', 'redeliver'] as const;
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
  failed: string;
}

// arm stores one event old enough for a purge to delete and lists one failed delivery that a sweep would ask GitHub for.
async function arm(cron: Faucet, hooks: HookStub): Promise<Armed> {
  const repoId = newRepoId();
  const old = newDeliveryId();
  const failed = newDeliveryId();
  const stored = await cron.deliver({ event: run.event, delivery: old, body: JSON.stringify(run.payload(repoId)) });
  expect(stored.body).toBe('stored\n');
  await cron.backdate(old, OLD_DAYS);
  await hooks.set([stubDelivery(repoId, { id: 1, guid: failed, ageMs: 60_000, status: 500 })]);
  return { repoId, old, failed };
}

async function deliveriesOf(cron: Faucet, repoId: number): Promise<string[]> {
  return (await cron.rows(repoId)).map((row) => row.delivery_id);
}

async function hookRequests(hooks: HookStub): Promise<string[]> {
  const requests = await hooks.requests();
  return requests.filter((r) => r.path.startsWith('/orgs/')).map((r) => `${r.method} ${r.path}`);
}

// expectIdle checks that the job did not purge the old event and did not call the hook API.
async function expectIdle(cron: Faucet, hooks: HookStub, armed: Armed): Promise<void> {
  expect(await deliveriesOf(cron, armed.repoId), 'the old event must stay').toEqual([armed.old]);
  expect(await hookRequests(hooks), 'GitHub must not be called').toEqual([]);
}

describe('cron routes', () => {
  for (const job of JOBS) {
    for (const [title, send] of refused) {
      test(`GET /cron/${job} with ${title} is 401 and runs nothing`, async ({ cron, hooks }) => {
        const armed = await arm(cron, hooks);

        const reply = await cron.cronJob(job, { send });

        expect(reply.status).toBe(401);
        expect(reply.body).toBe('unauthorized\n');
        expect(reply.headers.get('www-authenticate')).toBe('Bearer');
        expect(cron.leaksSecret(reply.body + JSON.stringify([...reply.headers]))).toBe(false);
        await expectIdle(cron, hooks, armed);
      });
    }

    for (const method of ['POST', 'PUT', 'DELETE']) {
      test(`${method} /cron/${job} with the right secret is 405 and runs nothing`, async ({ cron, hooks }) => {
        const armed = await arm(cron, hooks);

        const reply = await cron.cronJob(job, { method });

        expect(reply.status).toBe(405);
        await expectIdle(cron, hooks, armed);
      });
    }
  }

  test('GET /cron/purge with the right secret purges and answers JSON without the secret', async ({ cron, hooks }) => {
    const armed = await arm(cron, hooks);

    const reply = await cron.cronJob('purge');

    expect(reply.status).toBe(200);
    expect(reply.headers.get('content-type')).toContain('application/json');
    expect(JSON.parse(reply.body)).toEqual({ deleted: expect.any(Number), purged_through: expect.any(Number) });
    expect(await deliveriesOf(cron, armed.repoId)).toEqual([]);
    expect(cron.leaksSecret(reply.body + JSON.stringify([...reply.headers]))).toBe(false);
  });

  test('GET /cron/redeliver with the right secret redelivers and answers JSON without the secret', async ({ cron, hooks }) => {
    const armed = await arm(cron, hooks);

    const reply = await cron.cronJob('redeliver');

    expect(reply.status).toBe(200);
    expect(reply.headers.get('content-type')).toContain('application/json');
    expect(JSON.parse(reply.body)).toEqual({ examined: 1, pending: 1, requested: 1, incomplete: false });
    expect(await deliveriesOf(cron, armed.repoId)).toContain(armed.failed);
    expect(cron.leaksSecret(reply.body + JSON.stringify([...reply.headers]))).toBe(false);
  });

  for (const job of JOBS) {
    test(`a faucet without CRON_SECRET answers /cron/${job} 503 to every request and runs nothing`, async ({ faucet }) => {
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
          const reply = await faucet.cronJob(job, { send });
          expect(reply.status, title).toBe(503);
          expect(reply.body, title).toBe('cron is not configured\n');
          expect(faucet.leaksSecret(reply.body)).toBe(false);
        }
        expect(await deliveriesOf(faucet, repoId), 'a purge must not run').toEqual([old]);
      } finally {
        await faucet.sql(`delete from faucet_events where delivery_id = '${old}'`);
      }
    });
  }

  test('the app log never holds the cron secret, the hook token or the stub control token', async ({ cron }) => {
    const log = readFileSync('.e2e/logs/app.log', 'utf8');

    expect(cron.leaksSecret(log)).toBe(false);
  });
});

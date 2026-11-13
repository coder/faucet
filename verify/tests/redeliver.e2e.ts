import { readFileSync } from 'node:fs';
import { samples, stubDelivery } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet, type HookStub, type StubDelivery } from './support';

const run = samples['workflow_run.completed'];
const MINUTE = 60_000;
const HOOK = '/orgs/verify/hooks/7';
const MAX_PAGES = 10;
const MAX_REQUESTS = 50;
const MAX_REDELIVERIES_PER_DELIVERY = 3;

interface Sweep {
  examined: number;
  pending: number;
  gave_up: number;
  requested: number;
  incomplete: boolean;
}

async function sweep(cron: Faucet): Promise<Sweep> {
  const reply = await cron.cronJob('redeliver');
  expect(reply.status, reply.body).toBe(200);
  expect(cron.leaksSecret(reply.body)).toBe(false);
  return JSON.parse(reply.body) as Sweep;
}

// asked returns the attempt IDs that the faucet asked GitHub to redeliver, in request order, and checks that the faucet
// called nothing but the two delivery endpoints, with the hook token and never with another credential.
async function asked(hooks: HookStub): Promise<number[]> {
  const requests = await hooks.requests();
  const ids: number[] = [];
  for (const r of requests) {
    expect(r.auth, `${r.method} ${r.path}`).toBe('hook');
    const attempt = r.method === 'POST' ? new RegExp(`^${HOOK}/deliveries/(\\d+)/attempts$`).exec(r.path) : null;
    if (attempt !== null) ids.push(Number(attempt[1]));
    else expect(`${r.method} ${r.path}`).toBe(`GET ${HOOK}/deliveries`);
  }
  return ids;
}

async function listCalls(hooks: HookStub): Promise<string[]> {
  return (await hooks.requests()).filter((r) => r.method === 'GET').map((r) => r.query);
}

async function storeDirectly(cron: Faucet, repoId: number, delivery: string): Promise<void> {
  const reply = await cron.deliver({ event: run.event, delivery, body: JSON.stringify(run.payload(repoId)) });
  expect(reply.body).toBe('stored\n');
}

describe('redelivery sweep', () => {
  test('asks GitHub again only for failed deliveries that are not stored and have no later success', async ({ cron, hooks }) => {
    const repoId = newRepoId();
    const guid = {
      failed: newDeliveryId(),
      stored: newDeliveryId(),
      retried: newDeliveryId(),
      delivered: newDeliveryId(),
      twice: newDeliveryId(),
      silent: newDeliveryId(),
      late: newDeliveryId(),
      tooOld: newDeliveryId(),
    };
    const attempt = (id: number, guid: string, ageMin: number, status: number): StubDelivery =>
      stubDelivery(repoId, { id, guid, ageMs: ageMin * MINUTE, status });
    await storeDirectly(cron, repoId, guid.stored);
    await hooks.set([
      attempt(101, guid.failed, 10, 500),
      attempt(102, guid.stored, 20, 502),
      attempt(103, guid.retried, 30, 500),
      attempt(104, guid.retried, 25, 200),
      attempt(105, guid.delivered, 15, 200),
      attempt(106, guid.twice, 50, 500),
      attempt(107, guid.twice, 40, 503),
      attempt(109, guid.silent, 5, 0),
      attempt(111, guid.late, 100, 500),
      attempt(110, guid.tooOld, 125, 500),
    ]);

    const first = await sweep(cron);
    const requests = await asked(hooks);
    const rows = await cron.rows(repoId);
    const second = await sweep(cron);

    expect(first).toEqual({ examined: 9, pending: 4, gave_up: 0, requested: 4, incomplete: false });
    expect(requests, 'the newest failed attempt of each delivery, oldest first').toEqual([111, 107, 101, 109]);
    expect(rows.map((r) => r.delivery_id).sort()).toEqual([guid.failed, guid.late, guid.silent, guid.stored, guid.twice].sort());
    expect(second).toEqual({ examined: 13, pending: 0, gave_up: 0, requested: 0, incomplete: false });
    expect(await cron.rows(repoId)).toHaveLength(5);
  });

  test('gives up on a delivery that the faucet keeps rejecting after 3 redeliveries', async ({ cron, hooks }) => {
    const repoId = newRepoId();
    const guid = newDeliveryId();
    await hooks.set([{ id: 201, guid, ageMs: 5 * MINUTE, status: 500, event: run.event }]);

    const sweeps: Sweep[] = [];
    for (let i = 0; i < MAX_REDELIVERIES_PER_DELIVERY + 2; i++) sweeps.push(await sweep(cron));
    const requests = await asked(hooks);

    expect(sweeps.map((r) => [r.pending, r.gave_up, r.requested])).toEqual([[1, 0, 1], [1, 0, 1], [1, 0, 1], [0, 1, 0], [0, 1, 0]]);
    expect(requests, 'the first attempt, then each redelivery').toEqual([201, 202, 203]);
    expect(await cron.rows(repoId)).toEqual([]);
    expect(readFileSync('.e2e/logs/app.log', 'utf8')).toMatch(new RegExp(`redelivery gave up\\s+delivery_id=${guid}\\s+redeliveries=3\\s+reason=limit`));
  });

  test('sends at most 50 redelivery requests per run, oldest first, and the next run goes on', async ({ cron, hooks }) => {
    const repoId = newRepoId();
    const total = MAX_REQUESTS + 10;
    const guids = Array.from({ length: total }, newDeliveryId);
    await hooks.set(
      guids.map((guid, i) => stubDelivery(repoId, { id: 1000 + i, guid, ageMs: (total - i) * 1_000 + MINUTE, status: 500 })),
    );

    const first = await sweep(cron);
    const firstAsked = await asked(hooks);
    const second = await sweep(cron);
    const third = await sweep(cron);

    expect(first).toEqual({ examined: total, pending: total, gave_up: 0, requested: MAX_REQUESTS, incomplete: true });
    expect(firstAsked).toEqual(Array.from({ length: MAX_REQUESTS }, (_, i) => 1000 + i));
    expect(second).toMatchObject({ pending: 10, requested: 10, incomplete: false });
    expect(third).toMatchObject({ pending: 0, requested: 0, incomplete: false });
    expect(await cron.rows(repoId)).toHaveLength(total);
  });

  test('reads at most 10 pages of deliveries per run', async ({ cron, hooks }) => {
    const repoId = newRepoId();
    const pageSize = 5;
    const deliveries = Array.from({ length: 12 * pageSize }, (_, i) =>
      stubDelivery(repoId, { id: 300 + i, guid: newDeliveryId(), ageMs: (i + 1) * 1_000, status: 200 }),
    );
    await hooks.set(deliveries, { pageSize });

    const result = await sweep(cron);
    const lists = await listCalls(hooks);

    expect(result).toEqual({ examined: MAX_PAGES * pageSize, pending: 0, gave_up: 0, requested: 0, incomplete: true });
    expect(lists).toHaveLength(MAX_PAGES);
    expect(new Set(lists).size, 'each call asks for a new page').toBe(MAX_PAGES);
  });

  test('stops at the first redelivery request that GitHub refuses and reports a bad gateway', async ({ cron, hooks }) => {
    const repoId = newRepoId();
    const deliveries = Array.from({ length: 3 }, (_, i) =>
      stubDelivery(repoId, { id: 400 + i, guid: newDeliveryId(), ageMs: (i + 5) * MINUTE, status: 500 }),
    );
    await hooks.set(deliveries, { redeliveryStatus: 403 });

    const reply = await cron.cronJob('redeliver');

    expect(reply.status).toBe(502);
    expect(reply.body).toBe('redelivery failed\n');
    expect(cron.leaksSecret(reply.body)).toBe(false);
    expect(await asked(hooks), 'one request, then the run stops').toHaveLength(1);
    expect(await cron.rows(repoId)).toEqual([]);
  });

  test('a faucet with a cron secret and no hook settings answers 503 after it checks the secret', async ({ cron }) => {
    await cron.withFaucet({}, async (bare, output) => {
      const wrong = await bare.cronJob('redeliver', { send: () => ({ authorization: `Bearer ${'0'.repeat(64)}` }) });
      const right = await bare.cronJob('redeliver');
      const purge = await bare.cronJob('purge');

      expect(wrong.status).toBe(401);
      expect(right.status).toBe(503);
      expect(right.body).toBe('redelivery is not configured\n');
      expect(purge.status).toBe(200);
      expect(bare.leaksSecret(right.body + output())).toBe(false);
    });
  });
});

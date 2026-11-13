import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet, type StubDelivery } from './support';

const run = samples['workflow_run.completed'];
const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const MAX_ASKS = 7;
const PAGES_PER_RUN = 20;
const HOOK = '/orgs/verify/hooks/7';

interface Outcome {
  busy: boolean;
  pages: number;
  attempts: number;
  tracked: number;
  asked: number;
  recovered: number;
  gave_up: { limit: number; expired: number };
  gaps: number;
  pending: number;
  ranges: number;
}

// Each test runs its own faucet on its own database, because the position of a run is state that the next test would inherit.
function withHook<T>(cron: Faucet, fn: (started: Faucet, output: () => string) => Promise<T>): Promise<T> {
  return cron.withFaucet(cron.hookEnv(), fn);
}

async function sweep(faucet: Faucet): Promise<Outcome> {
  const reply = await faucet.cronJob('redeliver');
  expect(reply.status, reply.body).toBe(200);
  expect(faucet.leaksSecret(reply.body)).toBe(false);
  return JSON.parse(reply.body) as Outcome;
}

// failed is an attempt that faucet answered with `status`. A redelivery of it posts a workflow run that faucet stores.
function failed(repoId: number, guid: string, ageMs: number, status = 503): StubDelivery {
  return { guid, ageMs, status, payload: run.payload(repoId) };
}

// rejected is a delivery that faucet rejects every time: the payload has no action.
function rejected(repoId: number, guid: string, ageMs: number): StubDelivery {
  return { guid, ageMs, status: 400, payload: { repository: { id: repoId } } };
}

function lines(output: string, message: string): string[] {
  return output.split('\n').filter((line) => line.includes(message));
}

describe('redelivery', () => {
  test('asks GitHub again for a failed delivery, stores it, and counts the recovery once', async ({ cron, hooks }) => {
    await withHook(cron, async (started, output) => {
      const repoId = newRepoId();
      const guid = newDeliveryId();
      await hooks.arm(started, [failed(repoId, guid, 5 * MINUTE)]);

      const first = await sweep(started);
      const stored = (await started.rows(repoId)).map((row) => row.delivery_id);
      const second = await sweep(started);
      const third = await sweep(started);

      expect(first).toMatchObject({ busy: false, tracked: 1, asked: 1, recovered: 0 });
      expect(stored, 'the redelivery reached faucet, which stored the event').toEqual([guid]);
      expect(second).toMatchObject({ tracked: 0, asked: 0, recovered: 1, gaps: 0 });
      expect(third.recovered, 'a delivery recovers once').toBe(0);
      expect(await started.sql(`select state, asks from faucet_redeliveries where guid = '${guid}'`)).toBe('recovered|1');
      expect(lines(output(), 'redelivery recovered'), 'and is logged once').toHaveLength(1);
      expect(lines(output(), 'redelivery requested')).toHaveLength(1);
      expect(started.leaksSecret(output())).toBe(false);
    });
  });

  test('sends the hook token only to the two delivery endpoints of the configured webhook', async ({ cron, hooks }) => {
    await withHook(cron, async (started) => {
      const repoId = newRepoId();
      await hooks.arm(started, [failed(repoId, newDeliveryId(), 5 * MINUTE), failed(repoId, newDeliveryId(), 6 * MINUTE)]);

      await sweep(started);
      const requests = await hooks.requests();

      expect(requests.length).toBeGreaterThan(1);
      for (const request of requests) {
        expect(request.auth, `${request.method} ${request.path}`).toBe('hook');
        expect(`${request.method} ${request.path}`).toMatch(new RegExp(`^(GET ${HOOK}/deliveries|POST ${HOOK}/deliveries/\\d+/attempts)$`));
      }
    });
  });

  test('asks for a delivery that faucet always rejects at a doubling pace, gives it up once, and logs it once', async ({ cron, hooks }) => {
    await withHook(cron, async (started, output) => {
      const repoId = newRepoId();
      const guid = newDeliveryId();
      await hooks.arm(started, [rejected(repoId, guid, 5 * MINUTE)]);
      const outcomes: Outcome[] = [];
      const fastForward = (): Promise<string> => started.sql(`update faucet_redeliveries set next_ask_at = now() where guid = '${guid}' returning guid`);

      outcomes.push(await sweep(started));
      const waiting = await sweep(started);
      for (let ask = 2; ask <= MAX_ASKS; ask++) {
        await fastForward();
        outcomes.push(await sweep(started));
      }
      await fastForward();
      const limit = await sweep(started);
      const after = await sweep(started);
      const asks = (await hooks.requests()).filter((request) => request.method === 'POST');

      expect(waiting.asked, 'the second ask waits 10 minutes').toBe(0);
      expect(outcomes.map((outcome) => outcome.asked)).toEqual(Array.from({ length: MAX_ASKS }, () => 1));
      expect(asks).toHaveLength(MAX_ASKS);
      expect(limit.gave_up).toEqual({ limit: 1, expired: 0 });
      expect(after).toMatchObject({ asked: 0, gave_up: { limit: 0, expired: 0 } });
      expect(await started.sql(`select state, reason, asks from faucet_redeliveries where guid = '${guid}'`)).toBe(`gave_up|limit|${MAX_ASKS}`);
      expect(await started.rows(repoId)).toEqual([]);
      const logged = lines(output(), 'redelivery gave up');
      expect(logged).toHaveLength(1);
      expect(logged[0]).toContain(guid);
      expect(logged[0]).toContain('reason=limit');
    });
  });

  test('reads a list that does not fit in one run across runs, and keeps its place in the table', async ({ cron, hooks }) => {
    await withHook(cron, async (started) => {
      const repoId = newRepoId();
      const pageSize = 5;
      const total = 160;
      const deep = [140, 150, 157];
      const guids = deep.map(() => newDeliveryId());
      const attempts: StubDelivery[] = Array.from({ length: total }, (_, i) => {
        const slot = deep.indexOf(i + 1);
        const ageMs = (i + 1) * 20_000 + MINUTE;
        return slot >= 0 ? failed(repoId, guids[slot]!, ageMs) : { guid: newDeliveryId(), ageMs, status: 200 };
      });
      await hooks.arm(started, attempts, { pageSize });

      const first = await sweep(started);
      const open = await started.sql("select count(*) from faucet_redelivery_ranges where state = 'open'");
      const second = await sweep(started);
      const openAfter = await started.sql("select count(*) from faucet_redelivery_ranges where state = 'open'");

      expect(first).toMatchObject({ pages: PAGES_PER_RUN, attempts: PAGES_PER_RUN * pageSize, tracked: 0, ranges: 1 });
      expect(open, 'the unread part of the list is a row, so a restart would find it').toBe('1');
      expect(second).toMatchObject({ tracked: 3, asked: 3, ranges: 0 });
      expect(openAfter).toBe('0');
      expect((await started.rows(repoId)).map((row) => row.delivery_id).sort()).toEqual([...guids].sort());
    });
  });

  test('records the time before the first run\'s lookback as one gap, and asks for nothing inside it', async ({ cron, hooks }) => {
    await withHook(cron, async (started, output) => {
      const recent = newRepoId();
      const before = newRepoId();
      await hooks.arm(started, [failed(recent, newDeliveryId(), 20 * HOUR), failed(before, newDeliveryId(), 30 * HOUR)]);

      const first = await sweep(started);
      const second = await sweep(started);

      expect(first).toMatchObject({ tracked: 1, asked: 1, gaps: 1 });
      expect(second.gaps, 'the gap is recorded once').toBe(0);
      expect(await started.rows(recent)).toHaveLength(1);
      expect(await started.rows(before), 'the delivery inside the gap is not asked for').toEqual([]);
      expect(await started.sql("select count(*) from faucet_redelivery_ranges where state = 'gap'")).toBe('1');
      const logged = lines(output(), 'redelivery gap');
      expect(logged).toHaveLength(1);
      expect(logged[0]).toContain('before tracking began');
    });
  });

  test('a run that starts while another holds the lease does nothing, so one request goes out per delivery', async ({ cron, hooks }) => {
    await withHook(cron, async (started) => {
      const repoId = newRepoId();
      await hooks.arm(started, [failed(repoId, newDeliveryId(), 5 * MINUTE)], { listDelayMs: 1500 });

      const [one, two] = await Promise.all([started.cronJob('redeliver'), started.cronJob('redeliver')]);
      const outcomes = [one, two].map((reply) => JSON.parse(reply.body) as Outcome);
      const asks = (await hooks.requests()).filter((request) => request.method === 'POST');

      expect([one.status, two.status]).toEqual([200, 200]);
      expect(outcomes.filter((outcome) => outcome.busy)).toHaveLength(1);
      expect(outcomes.reduce((sum, outcome) => sum + outcome.asked, 0)).toBe(1);
      expect(asks).toHaveLength(1);
    });
  });

  test('stops at a redelivery request that GitHub refuses, reports a generic error, and still spends the ask', async ({ cron, hooks }) => {
    await withHook(cron, async (started, output) => {
      const repoId = newRepoId();
      const guid = newDeliveryId();
      await hooks.arm(started, [failed(repoId, guid, 5 * MINUTE), failed(repoId, newDeliveryId(), 6 * MINUTE)], { redeliveryStatus: 403 });

      const reply = await started.cronJob('redeliver');
      const asks = (await hooks.requests()).filter((request) => request.method === 'POST');

      expect(reply.status).toBe(500);
      expect(reply.body).toBe('redelivery failed\n');
      expect(reply.body).not.toContain('403');
      expect(output(), 'the log keeps the status').toContain('403');
      expect(asks, 'one request, then the run stops').toHaveLength(1);
      expect(started.leaksSecret(reply.body + output())).toBe(false);
      expect(await started.sql('select count(*) from faucet_redeliveries where asks = 1')).toBe('1');
      expect(await started.rows(repoId)).toEqual([]);
    });
  });
});

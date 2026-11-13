import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet } from './support';

const run = samples['workflow_run.completed'];
const RUN = run.expected.type;
const RETENTION_DAYS = 30;

interface PurgeReply {
  deleted: number;
  purged_through: number;
}

interface PollResult {
  events: { eventId: string }[];
  cursor: string;
  truncated: boolean;
}

function repoOf(repoId: number): string {
  return `verify/r${repoId}`;
}

async function store(faucet: Faucet, repoId: number, ageDays = 0): Promise<string> {
  const delivery = newDeliveryId();
  const reply = await faucet.deliver({ event: run.event, delivery, body: JSON.stringify(run.payload(repoId)) });
  expect(reply.body, 'delivery').toBe('stored\n');
  if (ageDays > 0) await faucet.backdate(delivery, ageDays);
  return delivery;
}

async function purge(faucet: Faucet): Promise<PurgeReply> {
  const reply = await faucet.cronJob('purge');
  expect(reply.status, reply.body).toBe(200);
  return JSON.parse(reply.body) as PurgeReply;
}

async function poll(faucet: Faucet, repoId: number, cursor?: string): Promise<PollResult> {
  const reply = await faucet.mcp<PollResult>('events/poll', { name: RUN, arguments: { repo: repoOf(repoId) }, cursor });
  if (reply.status !== 200 || reply.result === null) throw new Error(`poll failed: ${reply.body}`);
  return reply.result;
}

async function deliveriesOf(faucet: Faucet, repoId: number): Promise<string[]> {
  return (await faucet.rows(repoId)).map((row) => row.delivery_id);
}

// retentionOracle asks Postgres, not faucet, what a purge with the default retention must delete and where it leaves the mark.
async function retentionOracle(faucet: Faucet): Promise<PurgeReply> {
  const old = `received_at < now() - interval '${RETENTION_DAYS} days'`;
  const deleted = Number(await faucet.sql(`select count(*) from faucet_events where ${old}`));
  const highest = Number(await faucet.sql(`select coalesce(max(id), 0) from faucet_events where ${old}`));
  return { deleted, purged_through: Math.max(highest, await faucet.purgeMark()) };
}

describe('purge', () => {
  test('deletes the events older than the retention, keeps the rest and reports what it did', async ({ cron }) => {
    const repoId = newRepoId();
    await store(cron, repoId, 40);
    await store(cron, repoId, RETENTION_DAYS + 1);
    const justInside = await store(cron, repoId, RETENTION_DAYS - 1);
    const fresh = await store(cron, repoId);
    const expected = await retentionOracle(cron);

    const reply = await purge(cron);

    expect(expected.deleted).toBeGreaterThanOrEqual(2);
    expect(reply).toEqual(expected);
    expect(await deliveriesOf(cron, repoId)).toEqual([justInside, fresh]);
    expect(await cron.purgeMark()).toBe(reply.purged_through);
  });

  test('a second purge deletes nothing and leaves the mark where it was', async ({ cron }) => {
    await store(cron, newRepoId(), 40);
    const first = await purge(cron);

    const second = await purge(cron);

    expect(second).toEqual({ deleted: 0, purged_through: first.purged_through });
  });

  test('an events/poll from before the purge says events were lost, one from after does not', async ({ cron }) => {
    const repoId = newRepoId();
    const before = (await poll(cron, repoId)).cursor;
    await store(cron, repoId, 40);
    const survivor = await store(cron, repoId);
    await purge(cron);

    const lost = await poll(cron, repoId, before);
    const resumed = await poll(cron, repoId, lost.cursor);
    const later = await store(cron, repoId);
    const next = await poll(cron, repoId, lost.cursor);

    expect(lost.events.map((e) => e.eventId)).toEqual([survivor]);
    expect(lost.truncated).toBe(true);
    expect(resumed).toMatchObject({ events: [], truncated: false });
    expect(next.events.map((e) => e.eventId)).toEqual([later]);
    expect(next.truncated).toBe(false);
  });

  test('a wait from before the purge says events were lost, and a cursor at the mark does not', async ({ cron }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const args = { repo, types: [RUN], timeout_s: 1 };
    const before = await cron.waitForEvents(args);
    await store(cron, repoId, 40);
    const survivor = await store(cron, repoId);
    const { purged_through: mark } = await purge(cron);

    const lost = await cron.waitForEvents({ ...args, cursor: before.cursor });
    const belowMark = await cron.waitForEvents({ ...args, cursor: mark - 1 });
    const atMark = await cron.waitForEvents({ ...args, cursor: mark });
    const rows = await cron.rows(repoId);

    expect(before.truncated).toBe(false);
    for (const outcome of [lost, belowMark, atMark]) {
      expect(outcome.isError).toBe(false);
      expect(outcome.events?.map((e) => e.id)).toEqual([rows[0]?.id]);
    }
    expect(rows.map((r) => r.delivery_id)).toEqual([survivor]);
    expect([lost.truncated, belowMark.truncated, atMark.truncated]).toEqual([true, true, false]);
  });

  test('a wait on a table that a purge emptied is flagged once, then starts at the mark', async ({ cron }) => {
    await cron.withFaucet({}, async (empty) => {
      const repoId = newRepoId();
      const args = { repo: repoOf(repoId), types: [RUN], timeout_s: 1 };
      const before = await empty.waitForEvents(args);
      await store(empty, repoId, 40);
      await store(empty, repoId, 41);
      const { purged_through: mark } = await purge(empty);

      const lost = await empty.waitForEvents({ ...args, cursor: before.cursor });
      const resumed = await empty.waitForEvents({ ...args, cursor: lost.cursor });
      const fresh = await empty.waitForEvents(args);

      expect(await empty.rows(repoId)).toEqual([]);
      expect(before).toMatchObject({ cursor: 0, truncated: false });
      expect(mark).toBeGreaterThan(0);
      expect(lost).toMatchObject({ events: [], cursor: mark, truncated: true });
      expect(resumed).toMatchObject({ events: [], cursor: mark, truncated: false });
      expect(fresh).toMatchObject({ events: [], cursor: mark, truncated: false });
    });
  });

  test('FAUCET_RETENTION_DAYS sets how old an event must be to go', async ({ cron }) => {
    await cron.withFaucet({ FAUCET_RETENTION_DAYS: '2' }, async (short) => {
      const repoId = newRepoId();
      await store(short, repoId, 3);
      const inside = await store(short, repoId, 1);

      const reply = await short.cronJob('purge');

      expect(reply.status).toBe(200);
      expect(JSON.parse(reply.body)).toMatchObject({ deleted: 1 });
      expect(await deliveriesOf(short, repoId)).toEqual([inside]);
    });
  });
});

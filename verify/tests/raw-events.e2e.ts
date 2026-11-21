import { counts, deliverDue, endpoint, eventRequests, expectSigned, paramsFor, withDeliveryFaucet } from './delivery';
import { recorded } from './payloads';
import { repoOf, subscribed } from './subscriptions';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet } from './support';

const PUSH = 'github.push';
const POLL_CURSOR = /^r1\.\d+\.[0-9a-f]{32}$/;

interface PollResult {
  events: { eventId: string; name: string; timestamp: string; data: Record<string, unknown> }[];
  cursor: string;
  truncated: boolean;
  hasMore: boolean;
}

interface PurgeReply {
  raw_events_cleared: number;
  raw_events_deleted: number;
  expired_leases: number;
}

function pushPayload(repoId: number): Record<string, unknown> {
  return {
    ref: 'refs/heads/main',
    after: 'a'.repeat(40),
    pusher: { name: 'octocat' },
    repository: { id: repoId, full_name: repoOf(repoId) },
  };
}

// push posts a signed push of the repository and returns the reply body with the GitHub delivery ID.
async function push(faucet: Faucet, repoId: number, delivery = newDeliveryId()): Promise<{ delivery: string; status: number; body: string }> {
  const reply = await faucet.deliver({ event: 'push', delivery, body: JSON.stringify(pushPayload(repoId)) });
  return { delivery, status: reply.status, body: reply.body };
}

async function poll(faucet: Faucet, repoId: number, cursor?: string, args: Record<string, unknown> = {}): Promise<PollResult> {
  const reply = await faucet.mcp<PollResult>('events/poll', { name: PUSH, arguments: { repo: repoOf(repoId), ...args }, cursor });
  if (reply.status !== 200 || reply.result === null) throw new Error(`poll failed: ${reply.body}`);
  return reply.result;
}

// stored says how many raw events have the delivery ID, and how many of them still hold a payload.
async function stored(faucet: Faucet, delivery: string): Promise<{ rows: number; withBody: number }> {
  const [rows, withBody] = (await faucet.sql(`select count(*), count(body) from faucet_raw_events where delivery_id = '${delivery}'`)).split('|');
  return { rows: Number(rows), withBody: Number(withBody) };
}

describe('raw GitHub events', () => {
  test('a push that no interest wants is acknowledged with 202 and not stored', async ({ faucet }) => {
    const sent = await push(faucet, newRepoId());

    expect(sent.status).toBe(202);
    expect(sent.body).toBe('ignored\n');
    expect(await stored(faucet, sent.delivery)).toEqual({ rows: 0, withBody: 0 });
  });

  test('a subscription to github.push gets a push as a signed webhook with the payload as data', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      const subscription = await subscribed(started, paramsFor(repoId, at, {}, PUSH));
      expect(subscription.cursor).toMatch(/^r1\.\d+$/);
      expect(subscription.truncated).toBe(false);

      const other = await push(started, newRepoId());
      const sent = await push(started, repoId);
      expect(other.body, 'a push of a repository that nobody subscribed to').toBe('ignored\n');
      expect(sent.status).toBe(200);
      expect(sent.body).toBe('stored\n');
      expect((await push(started, repoId, sent.delivery)).body, 'the same delivery again').toBe('duplicate\n');

      expect(await deliverDue(started)).toEqual(counts({ claimed: 1, sent: 1, delivered: 1 }));
      const requests = await eventRequests(receiver, at);
      expect(requests).toHaveLength(1);
      const body = expectSigned(requests[0]!, subscription.id);
      expect(body.name).toBe(PUSH);
      expect(body.data).toEqual(pushPayload(repoId));
      expect(body.cursor).toMatch(/^r1\.\d+$/);
      expect(await deliverDue(started), 'a second run has nothing left to send').toEqual(counts());
    });
  });

  test('events/poll for github.push opens a lease, returns a push that arrives after it, and a purge then drops the payload', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const repoId = newRepoId();
      const before = await push(started, repoId);
      expect(before.body, 'no lease exists yet').toBe('ignored\n');

      const opened = await poll(started, repoId);
      expect(opened.events).toEqual([]);
      expect(opened.truncated).toBe(false);
      expect(opened.cursor).toMatch(POLL_CURSOR);

      const sent = await push(started, repoId);
      expect(sent.body).toBe('stored\n');
      const page = await poll(started, repoId, opened.cursor);
      expect(page.events).toHaveLength(1);
      expect(page.events[0]).toMatchObject({ eventId: sent.delivery, name: PUSH, data: pushPayload(repoId) });
      expect(page.truncated).toBe(false);
      expect(await stored(started, sent.delivery)).toEqual({ rows: 1, withBody: 1 });

      const unread = JSON.parse((await started.cronJob('purge')).body) as PurgeReply;
      expect(unread.raw_events_cleared, 'the poller has not acknowledged the event').toBe(0);
      expect((await poll(started, repoId, opened.cursor)).events, 'the same cursor reads the event again').toHaveLength(1);

      expect((await poll(started, repoId, page.cursor)).events, 'the next cursor acknowledges it').toEqual([]);
      const read = JSON.parse((await started.cronJob('purge')).body) as PurgeReply;
      expect(read).toMatchObject({ raw_events_cleared: 1, raw_events_deleted: 0, expired_leases: 0 });
      expect(await stored(started, sent.delivery), 'the delivery ID stays, the payload is gone').toEqual({ rows: 1, withBody: 0 });
      expect((await push(started, repoId, sent.delivery)).body, 'a redelivery of a cleared event').toBe('duplicate\n');
    });
  });

  test('a lease of github.release never gets a draft release, and gets a published one', async ({ faucet }) => {
    const repoId = newRepoId();
    const params = { name: 'github.release', arguments: { repo: repoOf(repoId) } };
    const opened = await faucet.mcp<PollResult>('events/poll', params);
    expect(opened.result?.cursor).toMatch(POLL_CURSOR);

    const published = recorded('release_published', repoId) as { release: Record<string, unknown> };
    for (const action of ['created', 'edited', 'deleted']) {
      const draft = { ...published, action, release: { ...published.release, draft: true, published_at: null } };
      const sent = await faucet.deliver({ event: 'release', delivery: newDeliveryId(), body: JSON.stringify(draft) });
      expect(sent.status, action).toBe(202);
      expect(sent.body, action).toBe('ignored\n');
    }

    const delivery = newDeliveryId();
    expect((await faucet.deliver({ event: 'release', delivery, body: JSON.stringify(published) })).body).toBe('stored\n');
    const page = await faucet.mcp<PollResult>('events/poll', { ...params, cursor: opened.result?.cursor });
    expect(page.result?.events.map((event) => event.eventId), 'only the published release').toEqual([delivery]);
    expect(page.result?.events[0]?.data).toEqual(published);
  });

  test('a poll with fields gets only those members of the payload', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const repoId = newRepoId();
      const fields = { fields: ['repository.id', 'ref'] };
      const opened = await poll(started, repoId, undefined, fields);

      const sent = await push(started, repoId);
      expect(sent.body).toBe('stored\n');
      const page = await poll(started, repoId, opened.cursor, fields);

      expect(page.events).toHaveLength(1);
      expect(page.events[0]?.data).toEqual({ ref: 'refs/heads/main', repository: { id: repoId } });
      const body = await started.sql(`select convert_from(body, 'UTF8') from faucet_raw_events where delivery_id = '${sent.delivery}'`);
      expect(JSON.parse(body), 'only the fields that an interest asked for are stored').toEqual({ ref: 'refs/heads/main', repository: { id: repoId } });
    });
  });

  test('a poll with a cursor of another lease starts a new lease and says truncated', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const repoId = newRepoId();
      const opened = await poll(started, repoId);

      const other = await poll(started, repoId, opened.cursor, { actions: ['created'] });

      expect(other.truncated, 'other arguments are another lease').toBe(true);
      expect(other.cursor).toMatch(POLL_CURSOR);
      expect(other.cursor).not.toBe(opened.cursor);
      for (const cursor of ['v2.0', 'v1.0']) {
        const typed = await started.mcp('events/poll', { name: PUSH, arguments: { repo: repoOf(repoId) }, cursor });
        expect(typed.error?.code, cursor).toBe(-32602);
      }
    });
  });
});

import { readFileSync } from 'node:fs';
import { setTimeout as sleep } from 'node:timers/promises';
import { samples, type Sample } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet, type McpOptions, type McpReply } from './support';

const run = samples['workflow_run.completed'];
const check = samples['check_run.completed'];
const closed = samples['pull_request.closed'];
const comment = samples['issue_comment.created'];

const RUN = run.expected.type;
const REPO_DENIED = 'repository not found or not accessible';
const ISO_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/;

interface PolledEvent {
  eventId: string;
  name: string;
  timestamp: string;
  data: Record<string, unknown>;
}

interface PollResult {
  resultType: string;
  events: PolledEvent[];
  cursor: string;
  truncated: boolean;
  hasMore: boolean;
  nextPollMs: number;
}

function repoOf(repoId: number): string {
  return `verify/r${repoId}`;
}

function poll(
  faucet: Faucet,
  name: string,
  args: Record<string, unknown>,
  extra: Record<string, unknown> = {},
  opts: McpOptions = {},
): Promise<McpReply<PollResult>> {
  return faucet.mcp<PollResult>('events/poll', { name, arguments: args, ...extra }, opts);
}

async function pollRepo(
  faucet: Faucet,
  name: string,
  repoId: number,
  extra: Record<string, unknown> = {},
  args: Record<string, unknown> = {},
): Promise<PollResult> {
  const reply = await poll(faucet, name, { repo: repoOf(repoId), ...args }, extra);
  if (reply.status !== 200 || reply.result === null) throw new Error(`poll of ${name} failed: ${reply.body}`);
  return reply.result;
}

async function deliverSample(faucet: Faucet, sample: Sample, repoId: number): Promise<string> {
  const delivery = newDeliveryId();
  const reply = await faucet.deliver({ event: sample.event, delivery, body: JSON.stringify(sample.payload(repoId)) });
  expect(reply.body, `delivery of ${sample.event}`).toBe('stored\n');
  return delivery;
}

async function startCursor(faucet: Faucet, repoId: number): Promise<string> {
  return (await pollRepo(faucet, RUN, repoId)).cursor;
}

function ids(result: PollResult): string[] {
  return result.events.map((e) => e.eventId);
}

function dataOf(sample: Sample): Record<string, unknown> {
  const { prs, headSha, data } = sample.expected;
  return headSha === null ? { ...data, prs } : { ...data, prs, head_sha: headSha };
}

describe('events poll', () => {
  test('a poll without a cursor returns no events and a cursor, and does not replay earlier events', async ({ faucet }) => {
    const repoId = newRepoId();
    await deliverSample(faucet, run, repoId);

    const missing = await pollRepo(faucet, RUN, repoId);
    const nulled = await pollRepo(faucet, RUN, repoId, { cursor: null });
    const resumed = await pollRepo(faucet, RUN, repoId, { cursor: missing.cursor });

    for (const result of [missing, nulled, resumed]) {
      expect(result).toMatchObject({ resultType: 'complete', events: [], truncated: false, hasMore: false, nextPollMs: 5000 });
      expect(typeof result.cursor).toBe('string');
      expect(result.cursor).not.toBe('');
    }
  });

  test('a delivered event is returned once, with its delivery id, name, timestamp and data', async ({ faucet }) => {
    const repoId = newRepoId();
    const cursor = await startCursor(faucet, repoId);
    const sentAt = Date.now();
    const delivery = await deliverSample(faucet, run, repoId);
    const result = await pollRepo(faucet, RUN, repoId, { cursor });
    const polledAt = Date.now();
    const rows = await faucet.rows(repoId);
    const [event] = result.events;

    expect(result.events).toHaveLength(1);
    expect(event?.eventId).toBe(delivery);
    expect(event?.name).toBe(RUN);
    expect(event?.timestamp).toMatch(ISO_UTC);
    expect(Date.parse(event?.timestamp ?? '')).toBeGreaterThanOrEqual(sentAt - 1_000);
    expect(Date.parse(event?.timestamp ?? '')).toBeLessThanOrEqual(polledAt + 1_000);
    expect(event?.data).toEqual(dataOf(run));
    expect(rows.map((r) => r.delivery_id)).toEqual([delivery]);
    expect(result.hasMore).toBe(false);
    expect(result.truncated).toBe(false);
  });

  test('each event type is polled under its own name with its own data', async ({ faucet }) => {
    const repoId = newRepoId();
    const cursor = await startCursor(faucet, repoId);
    const deliveries = new Map<string, string>();
    for (const sample of Object.values(samples)) {
      deliveries.set(sample.expected.type, await deliverSample(faucet, sample, repoId));
    }
    const polled = new Map<string, PollResult>();
    for (const sample of Object.values(samples)) {
      polled.set(sample.expected.type, await pollRepo(faucet, sample.expected.type, repoId, { cursor }));
    }

    expect(deliveries.size).toBe(Object.keys(samples).length);
    for (const sample of Object.values(samples)) {
      const type = sample.expected.type;
      const result = polled.get(type);
      expect(result?.events.map((e) => [e.eventId, e.name]), type).toEqual([[deliveries.get(type), type]]);
      expect(result?.events[0]?.data, `${type} data`).toEqual(dataOf(sample));
    }
  });

  test('chained cursors return every event once, with no repeat and no gap', async ({ faucet }) => {
    const repoId = newRepoId();
    const otherId = newRepoId();
    const first = await startCursor(faucet, repoId);
    const a = await deliverSample(faucet, run, repoId);
    await deliverSample(faucet, run, otherId);
    const second = await pollRepo(faucet, RUN, repoId, { cursor: first });
    const idle = await pollRepo(faucet, RUN, repoId, { cursor: second.cursor });
    const b = await deliverSample(faucet, run, repoId);
    const c = await deliverSample(faucet, run, repoId);
    const third = await pollRepo(faucet, RUN, repoId, { cursor: idle.cursor });
    const fourth = await pollRepo(faucet, RUN, repoId, { cursor: third.cursor });
    const rows = await faucet.rows(repoId);

    expect(ids(second)).toEqual([a]);
    expect(ids(idle)).toEqual([]);
    expect(typeof idle.cursor).toBe('string');
    expect(idle.cursor).not.toBe('');
    expect(ids(third)).toEqual([b, c]);
    expect(ids(fourth)).toEqual([]);
    expect(rows.map((r) => r.delivery_id)).toEqual([a, b, c]);
  });

  test('the event name, sha and pr narrow what a poll returns', async ({ faucet }) => {
    const repoId = newRepoId();
    const cursor = await startCursor(faucet, repoId);
    const delivered = {
      run: await deliverSample(faucet, run, repoId),
      check: await deliverSample(faucet, check, repoId),
      closed: await deliverSample(faucet, closed, repoId),
      comment: await deliverSample(faucet, comment, repoId),
    };
    const sha = run.expected.headSha ?? '';
    const cases: [string, Record<string, unknown>, string[]][] = [
      [RUN, {}, [delivered.run]],
      ['ci.check_completed', {}, [delivered.check]],
      ['pr.closed', {}, [delivered.closed]],
      [RUN, { sha }, [delivered.run]],
      [RUN, { sha: sha.toUpperCase() }, [delivered.run]],
      [RUN, { sha: 'f'.repeat(40) }, []],
      ['pr.comment_created', { sha }, []],
      ['pr.comment_created', { pr: 7 }, [delivered.comment]],
      ['ci.check_completed', { pr: 8 }, [delivered.check]],
      [RUN, { pr: 8 }, []],
      [RUN, { pr: 99 }, []],
      ['ci.check_completed', { sha, pr: 7 }, [delivered.check]],
      ['ci.check_completed', { sha, pr: 99 }, []],
    ];
    for (const [name, args, expected] of cases) {
      const result = await pollRepo(faucet, name, repoId, { cursor }, args);
      expect(ids(result), `${name} ${JSON.stringify(args)}`).toEqual(expected);
    }
  });

  test('the events of another repository never appear', async ({ faucet }) => {
    const repoId = newRepoId();
    const otherId = newRepoId();
    const cursor = await startCursor(faucet, repoId);
    const mine = await deliverSample(faucet, run, repoId);
    const theirs = await deliverSample(faucet, run, otherId);

    const forMe = await pollRepo(faucet, RUN, repoId, { cursor });
    const forThem = await pollRepo(faucet, RUN, otherId, { cursor });

    expect(ids(forMe)).toEqual([mine]);
    expect(ids(forThem)).toEqual([theirs]);
  });

  test('a repository the token cannot read gets the same -32024 reply as one that does not exist', async ({ faucet }) => {
    const hidden = await poll(faucet, RUN, { repo: 'other/repo' });
    const missing = await poll(faucet, RUN, { repo: 'verify/never-existed' });

    for (const reply of [hidden, missing]) {
      expect(reply.status).toBe(200);
      expect(reply.result).toBeNull();
      expect(reply.error).toEqual({ code: -32024, message: REPO_DENIED });
      expect(reply.body).not.toContain('eventId');
    }
    expect(hidden.body).toBe(missing.body);
  });

  test('invalid arguments are -32602 errors that name the problem', async ({ faucet }) => {
    const repo = repoOf(newRepoId());
    const cases: [string, Record<string, unknown>, Record<string, unknown>, string][] = [
      ['no repo', {}, {}, 'repo'],
      ['an unknown argument', { repo, extra: 1 }, {}, 'extra'],
      ['a repo that is not owner/name', { repo: 'not a repo' }, {}, 'owner/name'],
      ['a short sha', { repo, sha: 'abc' }, {}, 'sha'],
      ['pr 0', { repo, pr: 0 }, {}, 'pr'],
      ['a cursor the server never returned', { repo }, { cursor: 'abc' }, 'cursor'],
      ['maxEvents 0', { repo }, { cursor: 'v1.0', maxEvents: 0 }, 'maxEvents'],
      ['maxAgeMs 0', { repo }, { cursor: 'v1.0', maxAgeMs: 0 }, 'maxAgeMs'],
    ];
    for (const [label, args, extra, message] of cases) {
      const reply = await poll(faucet, RUN, args, extra);
      expect(reply.status, label).toBe(400);
      expect(reply.result, label).toBeNull();
      expect(reply.error?.code, label).toBe(-32602);
      expect(reply.error?.message, label).toContain(message);
    }
  });

  test('an unknown event name is -32023 with the event types in the message', async ({ faucet }) => {
    const reply = await poll(faucet, 'pr.merged', { repo: repoOf(newRepoId()) });

    expect(reply.status).toBe(200);
    expect(reply.result).toBeNull();
    expect(reply.error?.code).toBe(-32023);
    expect(reply.error?.data).toEqual({ kind: 'event' });
    expect(reply.error?.message).toContain('pr.closed');
  });

  test('a missing or wrong Mcp-Name header is 400 with -32020 and the request id', async ({ faucet }) => {
    const args = { repo: repoOf(newRepoId()) };
    const missing = await poll(faucet, RUN, args, {}, { name: null, id: 4242 });
    const wrong = await poll(faucet, RUN, args, {}, { name: 'pr.closed', id: 'wrong-7' });

    expect(missing.status).toBe(400);
    expect(missing.id).toBe(4242);
    expect(missing.error?.code).toBe(-32020);
    expect(missing.result).toBeNull();
    expect(wrong.status).toBe(400);
    expect(wrong.id).toBe('wrong-7');
    expect(wrong.error?.code).toBe(-32020);
    expect(wrong.result).toBeNull();
  });

  test('a matching Mcp-Name header is accepted, plain or in the base64 form', async ({ faucet }) => {
    const args = { repo: repoOf(newRepoId()) };
    const encoded = `=?base64?${Buffer.from(RUN).toString('base64')}?=`;
    const plain = await poll(faucet, RUN, args);
    const base64 = await poll(faucet, RUN, args, {}, { name: encoded });

    for (const reply of [plain, base64]) {
      expect(reply.status).toBe(200);
      expect(reply.error).toBeNull();
      expect(reply.result?.resultType).toBe('complete');
    }
  });

  test('a request without a valid GitHub token is 401, before the Mcp-Name check', async ({ faucet }) => {
    const args = { repo: repoOf(newRepoId()) };
    const none = await poll(faucet, RUN, args, {}, { token: null, name: null });
    const wrong = await poll(faucet, RUN, args, {}, { token: `ghp_${'0'.repeat(40)}`, name: 'pr.closed' });

    for (const reply of [none, wrong]) {
      expect(reply.status).toBe(401);
      expect(reply.body).toBe('unauthorized\n');
      expect(reply.headers.get('www-authenticate')).toBe('Bearer');
    }
  });

  test('maxEvents caps a page and hasMore says that more events wait', async ({ faucet }) => {
    const repoId = newRepoId();
    const cursor = await startCursor(faucet, repoId);
    const delivered = [
      await deliverSample(faucet, run, repoId),
      await deliverSample(faucet, run, repoId),
      await deliverSample(faucet, run, repoId),
    ];

    const page1 = await pollRepo(faucet, RUN, repoId, { cursor, maxEvents: 2 });
    const page2 = await pollRepo(faucet, RUN, repoId, { cursor: page1.cursor, maxEvents: 2 });

    expect(ids(page1)).toEqual(delivered.slice(0, 2));
    expect(page1.hasMore).toBe(true);
    expect(ids(page2)).toEqual(delivered.slice(2));
    expect(page2.hasMore).toBe(false);
  });

  test('a page holds at most 100 events, whatever maxEvents says', async ({ faucet }) => {
    const repoId = newRepoId();
    const cursor = await startCursor(faucet, repoId);
    const delivered: string[] = [];
    for (let i = 0; i < 101; i++) delivered.push(await deliverSample(faucet, run, repoId));

    const unset = await pollRepo(faucet, RUN, repoId, { cursor });
    const huge = await pollRepo(faucet, RUN, repoId, { cursor, maxEvents: 1000 });
    const rest = await pollRepo(faucet, RUN, repoId, { cursor: huge.cursor, maxEvents: 1000 });

    expect(ids(unset)).toEqual(delivered.slice(0, 100));
    expect(unset.hasMore).toBe(true);
    expect(ids(huge)).toEqual(delivered.slice(0, 100));
    expect(huge.hasMore).toBe(true);
    expect(ids(rest)).toEqual(delivered.slice(100));
    expect(rest.hasMore).toBe(false);
  });

  test('maxAgeMs skips events older than that and sets truncated', async ({ faucet }) => {
    const repoId = newRepoId();
    const cursor = await startCursor(faucet, repoId);
    const old = await deliverSample(faucet, run, repoId);
    // The server compares the age of events with its own clock, so the test has to let real time pass.
    await sleep(1_500);
    const recent = await deliverSample(faucet, run, repoId);

    const unbounded = await pollRepo(faucet, RUN, repoId, { cursor });
    const generous = await pollRepo(faucet, RUN, repoId, { cursor, maxAgeMs: 3_600_000 });
    const bounded = await pollRepo(faucet, RUN, repoId, { cursor, maxAgeMs: 1_000 });

    expect(ids(unbounded)).toEqual([old, recent]);
    expect(unbounded.truncated).toBe(false);
    expect(ids(generous)).toEqual([old, recent]);
    expect(generous.truncated).toBe(false);
    expect(ids(bounded)).toEqual([recent]);
    expect(bounded.truncated).toBe(true);
  });

  test('replies and the app log never hold the GitHub token or the webhook secret', async ({ faucet }) => {
    const repoId = newRepoId();
    const denied = `other/denied-${repoId}`;
    const replies: McpReply<unknown>[] = [
      await faucet.mcp('server/discover'),
      await faucet.mcp('events/list'),
      await poll(faucet, RUN, { repo: repoOf(repoId) }),
      await poll(faucet, RUN, { repo: denied }),
      await poll(faucet, RUN, { repo: 'not a repo' }),
      await poll(faucet, RUN, { repo: repoOf(repoId) }, {}, { name: null }),
      await poll(faucet, RUN, { repo: repoOf(repoId) }, {}, { token: null }),
    ];
    const readLog = (): string => readFileSync('.e2e/logs/app.log', 'utf8');

    await expect.poll(() => new RegExp(`poll denied\\s+repo=${denied}\\s`).test(readLog())).toBe(true);
    for (const reply of replies) {
      expect(faucet.leaksSecret(reply.body), reply.body).toBe(false);
      expect(faucet.leaksSecret(JSON.stringify([...reply.headers])), reply.body).toBe(false);
    }
    expect(faucet.leaksSecret(readLog())).toBe(false);
  });
});

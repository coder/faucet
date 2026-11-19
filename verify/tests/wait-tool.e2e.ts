import { randomBytes } from 'node:crypto';
import { closedPullRequest, recorded, samples } from './payloads';
import { appLog, describe, expect, newDeliveryId, newRepoId, test, type Faucet } from './support';

const run = samples['workflow_run.completed'];
const allTypes = ['ci.run_completed', 'ci.check_completed', 'pr.review_submitted', 'pr.closed', 'pr.comment_created'];

const PR = 7;

function repoOf(repoId: number): string {
  return `verify/r${repoId}`;
}

function runPayload(repoId: number): object {
  const payload = run.payload(repoId) as { workflow_run: { pull_requests: object[] } };
  payload.workflow_run.pull_requests = [{ number: PR, base: { repo: { id: repoId } } }];
  return payload;
}

async function deliverRun(faucet: Faucet, repoId: number): Promise<void> {
  const reply = await faucet.deliver({ event: run.event, delivery: newDeliveryId(), body: JSON.stringify(runPayload(repoId)) });
  expect(reply.body).toBe('stored\n');
}

describe('wait tool', () => {
  test('a wait returns the event of a webhook delivered while it waits', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const idle = await faucet.waitForEvents({ repo, types: ['ci.run_completed'], timeout_s: 1 });
    expect(idle.events).toEqual([]);

    const started = Date.now();
    const waiting = faucet.waitForEvents({ repo, types: ['ci.run_completed'], cursor: idle.cursor, timeout_s: 20 });
    await deliverRun(faucet, repoId);
    const outcome = await waiting;
    const [row] = await faucet.rows(repoId);

    expect(outcome.status).toBe(200);
    expect(outcome.isError).toBe(false);
    expect(Date.now() - started).toBeLessThan(15_000);
    expect(outcome.events).toHaveLength(1);
    expect(outcome.events?.[0]).toMatchObject({
      id: row?.id,
      type: 'ci.run_completed',
      repo_id: repoId,
      prs: [PR],
      head_sha: run.expected.headSha,
      data: run.expected.data,
    });
    expect(outcome.cursor).toBe(row?.id);
  });

  test('a wait for a family returns its events with their exact type', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const idle = await faucet.waitForEvents({ repo, types: ['pr.activity'], timeout_s: 1 });
    await deliverRun(faucet, repoId);
    const stored = await faucet.deliver({ event: 'pull_request', delivery: newDeliveryId(), body: JSON.stringify(recorded('pull_request_closed', repoId)) });
    expect(stored.body).toBe('stored\n');

    const outcome = await faucet.waitForEvents({ repo, types: ['pr.activity'], cursor: idle.cursor, timeout_s: 5 });

    expect(outcome.isError).toBe(false);
    expect(outcome.events?.map((e) => e.type)).toEqual(['pr.closed']);
    expect(outcome.events?.[0]).toMatchObject({ repo_id: repoId, prs: [2], data: { merged: false } });
  });

  test('actions narrow only the families in types that have them, and an action of none of them is a tool error', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const idle = await faucet.waitForEvents({ repo, types: ['pr.activity'], timeout_s: 1 });
    await deliverRun(faucet, repoId);
    const stored = await faucet.deliver({ event: 'pull_request', delivery: newDeliveryId(), body: JSON.stringify(recorded('pull_request_closed', repoId)) });
    expect(stored.body).toBe('stored\n');

    const narrowed = await faucet.waitForEvents({ repo, types: ['pr.activity', 'ci.runs'], actions: ['closed'], cursor: idle.cursor, timeout_s: 5 });
    const refused = await faucet.waitForEvents({ repo, types: ['ci.runs'], actions: ['closed'], cursor: idle.cursor, timeout_s: 1 });

    expect(narrowed.isError).toBe(false);
    expect(narrowed.events?.map((e) => e.type), 'ci.runs has no closed action, so it stays whole').toEqual(['ci.run_completed', 'pr.closed']);
    expect(refused.isError).toBe(true);
    expect(refused.text).toContain('action "closed"');
  });

  test('an empty wait returns no events and a cursor at its deadline', async ({ faucet }) => {
    const started = Date.now();
    const outcome = await faucet.waitForEvents({ repo: repoOf(newRepoId()), types: allTypes, timeout_s: 2 });
    const elapsed = Date.now() - started;

    expect(outcome.status).toBe(200);
    expect(outcome.events).toEqual([]);
    expect(typeof outcome.cursor).toBe('number');
    expect(elapsed).toBeGreaterThanOrEqual(1_800);
    expect(elapsed).toBeLessThan(10_000);
  });

  test('the cursor of one result resumes the next wait without a repeat', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    await deliverRun(faucet, repoId);
    const first = await faucet.waitForEvents({ repo, types: allTypes, cursor: 0, timeout_s: 1 });
    await deliverRun(faucet, repoId);
    const second = await faucet.waitForEvents({ repo, types: allTypes, cursor: first.cursor, timeout_s: 1 });
    const rows = await faucet.rows(repoId);

    expect(first.events?.map((e) => e.id)).toEqual([rows[0]?.id]);
    expect(second.events?.map((e) => e.id)).toEqual([rows[1]?.id]);
  });

  test('types, sha and pr narrow what a wait returns', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    await deliverRun(faucet, repoId);
    const closed = await faucet.deliver({ event: 'pull_request', delivery: newDeliveryId(), body: JSON.stringify(closedPullRequest(repoId)) });
    expect(closed.body).toBe('stored\n');
    const [runRow, closedRow] = await faucet.rows(repoId);

    const ids = async (extra: Record<string, unknown>): Promise<number[]> => {
      const outcome = await faucet.waitForEvents({ repo, cursor: 0, timeout_s: 1, ...extra });
      return (outcome.events ?? []).map((e) => e.id);
    };
    expect(await ids({ types: allTypes })).toEqual([runRow?.id, closedRow?.id]);
    expect(await ids({ types: ['pr.closed'] })).toEqual([closedRow?.id]);
    expect(await ids({ types: allTypes, sha: run.expected.headSha })).toEqual([runRow?.id, closedRow?.id]);
    expect(await ids({ types: allTypes, sha: 'f'.repeat(40) })).toEqual([]);
    expect(await ids({ types: allTypes, pr: 99 })).toEqual([]);
    expect(await ids({ types: allTypes, pr: PR })).toEqual([runRow?.id, closedRow?.id]);
  });

  // GitHub decides what the person reads, and answers 404 for a repository the person cannot see, as for one that does not exist.
  test('a repository the person cannot see is denied the same way as one that does not exist', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const person = started.as('person');
      const repoId = newRepoId();
      await deliverRun(started, repoId);

      const hidden = await person.waitForEvents({ repo: 'verify/hidden', types: allTypes, cursor: 0 });
      const missing = await person.waitForEvents({ repo: 'verify/never-existed', types: allTypes, cursor: 0 });

      for (const outcome of [hidden, missing]) {
        expect(outcome.status).toBe(200);
        expect(outcome.isError).toBe(true);
        expect(outcome.events).toBeNull();
        expect(outcome.text).toBe('repository not found or not accessible');
      }
    }, { browserLogin: true });
  });

  test('a request without a faucet token is 401 with the challenge', async ({ faucet }) => {
    const args = { repo: repoOf(newRepoId()), types: allTypes };
    const none = await faucet.waitForEvents(args, null);
    const wrong = await faucet.waitForEvents(args, `ghp_${'0'.repeat(40)}`);

    for (const reply of [none, wrong]) {
      expect(reply.status).toBe(401);
      expect(reply.body).toBe('unauthorized\n');
      expect(reply.headers.get('www-authenticate')).toBe(faucet.challenge());
    }
  });

  test('a bearer with a faucet prefix that faucet did not mint, or a GitHub token, is 401 and never reaches GitHub', async ({ faucet, hooks }) => {
    const args = { repo: repoOf(newRepoId()), types: allTypes, timeout_s: 1 };
    await hooks.arm(faucet, []);
    for (const token of [...['fct_', 'fcr_', 'fcs_'].map((prefix) => prefix + randomBytes(32).toString('base64url')), `ghp_${randomBytes(20).toString('hex')}`]) {
      const reply = await faucet.waitForEvents(args, token);
      expect(reply.status, token.slice(0, 4)).toBe(401);
      expect(reply.body).toBe('unauthorized\n');
      expect(reply.headers.get('www-authenticate')).toBe(faucet.challenge());
    }
    expect(await hooks.requests(), 'faucet judges every bearer itself and sends none to GitHub').toEqual([]);
  });

  test('invalid input is a tool error that names the problem', async ({ faucet }) => {
    const repo = repoOf(newRepoId());
    const cases: [Record<string, unknown>, string][] = [
      [{ repo: 'not a repo', types: allTypes }, 'owner/name'],
      [{ repo, types: ['nope'] }, 'ci.run_completed'],
      [{ repo, types: [] }, 'at least one'],
      [{ repo, types: allTypes, timeout_s: 51 }, '50'],
      [{ repo, types: allTypes, sha: 'abc' }, '40'],
    ];
    for (const [args, message] of cases) {
      const outcome = await faucet.waitForEvents(args);
      expect(outcome.isError, JSON.stringify(args)).toBe(true);
      expect(outcome.text).toContain(message);
    }
  });

  test('the app log never holds a token or the webhook secret', async ({ faucet }) => {
    const denied = `other/denied-${newRepoId()}`;
    expect((await faucet.waitForEvents({ repo: denied, types: allTypes, cursor: 0 })).isError).toBe(true);

    await expect.poll(() => new RegExp(`wait denied\\s+repo=${denied}\\s`).test(appLog())).toBe(true);
    expect(faucet.leaksSecret(appLog())).toBe(false);
  });
});

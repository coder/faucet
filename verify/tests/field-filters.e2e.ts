import { counts, deliverDue, endpoint, eventRequests, expectSigned, paramsFor, withDeliveryFaucet } from './delivery';
import { recorded } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet, type McpReply } from './support';
import { newMarker, repoOf, subscribe, subscribed, subscribeParams } from './subscriptions';

// The repository that a head edited to come from a fork is in. newRepoId never gives it.
const FORK = { id: 7, name: 'repo', full_name: 'forker/repo', fork: true };

interface Polled {
  events: { eventId: string; name: string; data: Record<string, unknown> }[];
  cursor: string;
  hasMore: boolean;
}

interface Payload {
  workflow_run: Record<string, unknown>;
  pull_request: { base: Record<string, unknown>; head: Record<string, unknown> };
}

// The recorded samples run CI on master and open the pull request from changes into master. run and pullRequest edit a
// sample to the branch and the conclusion that a case needs, and leave the rest as GitHub sent it.
function run(repoId: number, branch: string, conclusion: string): Payload {
  const payload = recorded('workflow_run_completed', repoId) as Payload;
  Object.assign(payload.workflow_run, { head_branch: branch, conclusion });
  return payload;
}

function pullRequest(name: string, repoId: number, base: string, head = 'feature'): Payload {
  const payload = recorded(name, repoId) as Payload;
  payload.pull_request.base.ref = base;
  payload.pull_request.head.ref = head;
  return payload;
}

// octokit/webhooks records no payload from a fork, so these edit a recorded one: the head repository becomes another one, or
// null, which GitHub sends for a pull request whose fork was deleted.
function runFromFork(payload: Payload): Payload {
  payload.workflow_run.head_repository = FORK;
  return payload;
}

function pullRequestFrom(payload: Payload, repo: object | null): Payload {
  payload.pull_request.head.repo = repo;
  return payload;
}

async function store(faucet: Faucet, event: string, payload: object): Promise<string> {
  const delivery = newDeliveryId();
  const reply = await faucet.deliver({ event, delivery, body: JSON.stringify(payload) });
  expect(reply.body, event).toBe('stored\n');
  return delivery;
}

interface UseCase {
  family: string;
  args: Record<string, string[]>;
  // store stores six events and returns the delivery IDs of the two that match, in order.
  store: (faucet: Faucet, repoId: number) => Promise<string[]>;
  // seen describes an event by the fields that the filters read, and matched says what the two that match read.
  seen: (data: Record<string, unknown>) => string;
  matched: string[];
}

const USE_CASES: Record<string, UseCase> = {
  'CI failed on main': {
    family: 'ci.runs',
    args: { actions: ['completed'], head_branch: ['main'], conclusion: ['failure', 'timed_out'] },
    store: async (faucet, repoId) => {
      const failed = await store(faucet, 'workflow_run', run(repoId, 'main', 'failure'));
      await store(faucet, 'workflow_run', run(repoId, 'main', 'success'));
      await store(faucet, 'workflow_run', run(repoId, 'dev', 'failure'));
      const timedOut = await store(faucet, 'workflow_run', run(repoId, 'main', 'timed_out'));
      await store(faucet, 'workflow_run', runFromFork(run(repoId, 'main', 'failure')));
      await store(faucet, 'pull_request', pullRequest('pull_request_opened', repoId, 'main'));
      return [failed, timedOut];
    },
    seen: (data) => `${String(data.conclusion)} on ${String(data.head_branch)}, from a fork: ${String(data.from_fork)}`,
    matched: ['failure on main, from a fork: false', 'timed_out on main, from a fork: false'],
  },
  'pull requests into main to review': {
    family: 'pr.activity',
    args: { actions: ['opened', 'reopened', 'ready_for_review', 'synchronize'], base_branch: ['main'] },
    store: async (faucet, repoId) => {
      const opened = await store(faucet, 'pull_request', pullRequest('pull_request_opened', repoId, 'main'));
      await store(faucet, 'pull_request', recorded('pull_request_opened', repoId));
      await store(faucet, 'pull_request', pullRequest('pull_request_labeled', repoId, 'main'));
      const pushed = await store(faucet, 'pull_request', pullRequest('pull_request_synchronize', repoId, 'main'));
      await store(faucet, 'pull_request', pullRequest('pull_request_closed', repoId, 'main'));
      await store(faucet, 'workflow_run', run(repoId, 'main', 'failure'));
      return [opened, pushed];
    },
    seen: (data) => `${String(data.action)} into ${String(data.base_branch)}`,
    matched: ['opened into main', 'synchronize into main'],
  },
};

async function poll(faucet: Faucet, name: string, args: object, extra: object = {}): Promise<McpReply<Polled>> {
  return faucet.mcp<Polled>('events/poll', { name, arguments: args, ...extra });
}

async function polled(faucet: Faucet, name: string, args: object, extra: object = {}): Promise<Polled> {
  const reply = await poll(faucet, name, args, extra);
  if (reply.status !== 200 || reply.result === null) throw new Error(`poll of ${name} failed: ${reply.body}`);
  return reply.result;
}

describe('field filters', () => {
  for (const [name, useCase] of Object.entries(USE_CASES)) {
    test(`a poll for ${name} returns only those events, page by page`, async ({ faucet }) => {
      const repoId = newRepoId();
      const args = { repo: repoOf(repoId), ...useCase.args };
      const { cursor } = await polled(faucet, useCase.family, args);
      const want = await useCase.store(faucet, repoId);

      const all = await polled(faucet, useCase.family, args, { cursor });
      expect(all.events.map((e) => e.eventId)).toEqual(want);
      expect(all.events.map((e) => useCase.seen(e.data))).toEqual(useCase.matched);
      expect(all.hasMore).toBe(false);

      const first = await polled(faucet, useCase.family, args, { cursor, maxEvents: 1 });
      expect(first.events.map((e) => e.eventId)).toEqual(want.slice(0, 1));
      expect(first.hasMore, 'a page of one says that a match is left').toBe(true);
      const second = await polled(faucet, useCase.family, args, { cursor: first.cursor, maxEvents: 1 });
      expect(second.events.map((e) => e.eventId)).toEqual(want.slice(1));
      expect(second.hasMore, 'and none after the second, though events that do not match follow it').toBe(false);
    });

    test(`a subscription for ${name} is delivered only those events`, async ({ faucet, receiver }) => {
      await withDeliveryFaucet(faucet, async (started) => {
        const repoId = newRepoId();
        const at = await endpoint(receiver, 'ok');
        const { id } = await subscribed(started, paramsFor(repoId, at, useCase.args, useCase.family));
        await useCase.store(started, repoId);

        expect(await deliverDue(started)).toEqual(counts({ enqueued: 2, claimed: 2, sent: 2, delivered: 2 }));
        const bodies = (await eventRequests(receiver, at)).map((r) => expectSigned(r, id));
        expect(bodies.map((b) => b.name)).toEqual([useCase.family, useCase.family]);
        expect(bodies.map((b) => useCase.seen(b.data)).sort()).toEqual(useCase.matched);
      });
    });
  }

  test('a head_branch filter never matches a branch of a fork', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const { cursor } = await polled(faucet, 'ci.runs', { repo });
    const ours = await store(faucet, 'workflow_run', run(repoId, 'main', 'failure'));
    await store(faucet, 'workflow_run', runFromFork(run(repoId, 'main', 'failure')));
    const ourPR = await store(faucet, 'pull_request', pullRequest('pull_request_opened', repoId, 'master', 'main'));
    await store(faucet, 'pull_request', pullRequestFrom(pullRequest('pull_request_opened', repoId, 'master', 'main'), FORK));
    await store(faucet, 'pull_request', pullRequestFrom(pullRequest('pull_request_opened', repoId, 'master', 'main'), null));

    const runs = await polled(faucet, 'ci.runs', { repo, head_branch: ['main'] }, { cursor });
    const prs = await polled(faucet, 'pr.activity', { repo, head_branch: ['main'] }, { cursor });
    expect(runs.events.map((e) => e.eventId)).toEqual([ours]);
    expect(prs.events.map((e) => e.eventId), 'nor a branch of a fork that was deleted').toEqual([ourPR]);
    const all = await polled(faucet, 'pr.activity', { repo }, { cursor });
    expect(all.events.map((e) => [e.data.head_branch, e.data.from_fork])).toEqual([['main', false], ['main', true], ['main', true]]);
  });

  test('a conclusion filter matches no job or check that has not completed', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const { cursor } = await polled(faucet, 'ci.jobs', { repo });
    await store(faucet, 'workflow_job', recorded('workflow_job_queued', repoId));
    const failedJob = await store(faucet, 'workflow_job', recorded('workflow_job_completed', repoId));
    await store(faucet, 'check_run', recorded('check_run_created', repoId));
    const passedCheck = await store(faucet, 'check_run', recorded('check_run_completed', repoId));

    const jobs = await polled(faucet, 'ci.jobs', { repo, conclusion: ['failure', 'success'] }, { cursor });
    const checks = await polled(faucet, 'ci.checks', { repo, conclusion: ['failure', 'success'] }, { cursor });
    expect(jobs.events.map((e) => [e.eventId, e.data.conclusion])).toEqual([[failedJob, 'failure']]);
    expect(checks.events.map((e) => [e.eventId, e.data.conclusion])).toEqual([[passedCheck, 'success']]);
  });

  test('reordered or repeated filter values address the same subscription', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const params = subscribeParams(repoId, marker, { name: 'ci.runs' });
    const sorted = await subscribed(faucet, { ...params, arguments: { repo: repoOf(repoId), head_branch: ['main', 'release'], conclusion: ['failure', 'timed_out'] } });
    const shuffled = await subscribed(faucet, {
      ...params,
      arguments: { conclusion: ['timed_out', 'failure', 'failure'], head_branch: ['release', 'main'], repo: repoOf(repoId) },
    });

    expect(shuffled.id).toBe(sorted.id);
    expect(await faucet.subscriptions(marker)).toHaveLength(1);
  });

  test('a field filter that the name does not offer is -32602 for a poll and a subscribe, which stores nothing', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const marker = newMarker();
    for (const [name, args] of [
      ['ci.runs', { branch: ['main'] }],
      ['ci.runs', { base_branch: ['main'] }],
      ['ci.checks', { head_branch: ['main'] }],
      ['ci.jobs', { head_branch: ['main'] }],
      ['ci.suites', { head_branch: ['main'] }],
      ['ci.statuses', { conclusion: ['failure'] }],
      ['pr.activity', { conclusion: ['failure'] }],
      ['ci.run_completed', { head_branch: ['main'] }],
      ['ci.runs', { head_branch: [] }],
      ['ci.runs', { head_branch: 'main' }],
    ] as const) {
      const label = `${name} ${JSON.stringify(args)}`;
      const polledReply = await poll(faucet, name, { repo, ...args }, { cursor: 'v2.0' });
      expect(polledReply.error?.code, label).toBe(-32602);
      const subscribedReply = await subscribe(faucet, subscribeParams(repoId, marker, { name, arguments: { repo, ...args } }));
      expect(subscribedReply.error?.code, label).toBe(-32602);
    }
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('the wait tool takes the field filters and narrows only the families in types that offer them', async ({ faucet }) => {
    const repoId = newRepoId();
    const repo = repoOf(repoId);
    const idle = await faucet.waitForEvents({ repo, types: ['ci.runs'], timeout_s: 1 });
    const want = await USE_CASES['CI failed on main']!.store(faucet, repoId);
    const [first, second] = (await faucet.rows(repoId)).filter((r) => want.includes(r.delivery_id));

    const failures = await faucet.waitForEvents({ repo, types: ['ci.runs'], ...USE_CASES['CI failed on main']!.args, cursor: idle.cursor, timeout_s: 5 });
    expect(failures.isError).toBe(false);
    expect(failures.events?.map((e) => e.id)).toEqual([first!.repo_seq, second!.repo_seq]);

    const mixed = await faucet.waitForEvents({ repo, types: ['ci.runs', 'pr.activity'], conclusion: ['failure'], cursor: idle.cursor, timeout_s: 5 });
    expect(mixed.events?.map((e) => `${e.type} ${String(e.data.conclusion)}`), 'pr.activity has no conclusion, so it stays whole').toEqual([
      'ci.run_completed failure',
      'ci.run_completed failure',
      'ci.run_completed failure',
      'pr.opened undefined',
    ]);

    const refused = await faucet.waitForEvents({ repo, types: ['ci.runs'], base_branch: ['main'], cursor: idle.cursor, timeout_s: 1 });
    expect(refused.isError).toBe(true);
    expect(refused.text).toContain('base_branch is a filter of none of the names in types, only of pr.activity');
  });
});

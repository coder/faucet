import { ENDPOINT_SAYS } from '../runstate.mjs';
import {
  counts,
  deliverDue,
  endpoint,
  eventRequests,
  expectSigned,
  paramsFor,
  parseReply,
  pollFrom,
  pollStart,
  storeRun,
  withDeliveryFaucet,
  type DeliverReply,
} from './delivery';
import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';
import { occurrences, subscribed } from './subscriptions';

const check = samples['check_run.completed'];

describe('webhook delivery', () => {
  test('a subscription gets its matching events as signed webhooks, once each, and no other event', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const otherRepoId = newRepoId();
      const watching = await endpoint(receiver, 'ok');
      const filtered = await endpoint(receiver, 'ok');
      const elsewhere = await endpoint(receiver, 'ok');

      const before = await storeRun(started, repoId, 9000);
      const subscription = await subscribed(started, paramsFor(repoId, watching));
      await subscribed(started, paramsFor(repoId, filtered, { pr: 8 }));
      const otherSubscription = await subscribed(started, paramsFor(otherRepoId, elsewhere));
      const cursor = await pollStart(started, repoId);

      const first = await storeRun(started, repoId, 9101);
      const second = await storeRun(started, repoId, 9102);
      const checkRun = await started.deliver({ event: check.event, delivery: newDeliveryId(), body: JSON.stringify(check.payload(repoId)) });
      expect(checkRun.body).toBe('stored\n');
      const other = await storeRun(started, otherRepoId, 9201);

      const reply = await started.cronJob('deliver');
      expect(reply.status, reply.body).toBe(200);
      expect(reply.headers.get('content-type')).toContain('application/json');
      expect(JSON.parse(reply.body), 'the cron reply has these members and no others').toEqual(counts({ enqueued: 3, claimed: 3, sent: 3, delivered: 3 }));
      expect(started.leaksSecret(reply.body)).toBe(false);

      const requests = await eventRequests(receiver, watching);
      expect(requests, 'the two runs of the repository and nothing else').toHaveLength(2);
      const bodies = requests.map((request) => expectSigned(request, subscription.id));
      expect(bodies.map((b) => b.data.run_id).sort()).toEqual([9101, 9102]);
      expect(new Set(requests.map((r) => r.headers['webhook-id'])).size, 'one webhook-id per event').toBe(2);

      const polled = await pollFrom(started, repoId, cursor);
      expect(polled.map((e) => e.eventId).sort(), 'events/poll names the events by their GitHub delivery IDs').toEqual([first, second].sort());
      for (const body of bodies) {
        const same = polled.find((e) => e.data.run_id === body.data.run_id);
        expect(same, `events/poll returns run ${String(body.data.run_id)}`).toBeDefined();
        expect(body.name).toBe(same!.name);
        expect(body.timestamp).toBe(same!.timestamp);
        expect(body.data).toEqual(same!.data);
        expect([before, first, second, other], 'eventId is not a GitHub delivery ID').not.toContain(body.eventId);
        expect(body.cursor).toMatch(/^v1\.\d+$/);
      }

      expect(await eventRequests(receiver, filtered), 'a pr filter that no run matches gets nothing').toEqual([]);

      const [alone] = await eventRequests(receiver, elsewhere);
      expect(await eventRequests(receiver, elsewhere)).toHaveLength(1);
      const aloneBody = expectSigned(alone!, otherSubscription.id);
      const [row] = await started.rows(otherRepoId);
      expect(aloneBody.cursor, 'the cursor is the position before the only event that is unsent').toBe(`v1.${row!.id - 1}`);
      const replayed = await pollFrom(started, otherRepoId, aloneBody.cursor);
      expect(replayed.map((e) => e.eventId), 'events/poll from that cursor returns the event').toEqual([other]);
      expect(replayed[0]!.data).toEqual(aloneBody.data);

      expect(await started.deliveries(), 'every delivery ended').toEqual([]);
      expect(await deliverDue(started), 'a second run has nothing to do').toEqual(counts());
      expect(await eventRequests(receiver, watching)).toHaveLength(2);
      expect(output().match(/\[info\]\s+webhook delivered\s.*subscription=/g)).toHaveLength(3);
    });
  });

  test('six runs at once send each of 30 events exactly once', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      const subscription = await subscribed(started, paramsFor(repoId, at));
      const runIds = Array.from({ length: 30 }, (_, n) => 9900 + n);
      for (const runId of runIds) await storeRun(started, repoId, runId);

      const runs = (await Promise.all(Array.from({ length: 6 }, () => started.cronJob('deliver')))).map(parseReply);

      const total = (member: 'enqueued' | 'claimed' | 'sent' | 'delivered' | 'retried' | 'superseded'): number =>
        runs.reduce((sum, run: DeliverReply) => sum + run[member], 0);
      expect(total('delivered'), JSON.stringify(runs)).toBe(30);
      expect([total('enqueued'), total('claimed'), total('sent')], 'each event is queued, claimed and sent once').toEqual([30, 30, 30]);
      expect([total('retried'), total('superseded')]).toEqual([0, 0]);
      expect(runs.filter((run) => run.claimed > 0).length, 'the runs overlapped and shared the work').toBeGreaterThanOrEqual(2);
      for (const run of runs) expect([run.gave_up, run.incomplete]).toEqual([{}, false]);

      const requests = await eventRequests(receiver, at);
      expect(requests).toHaveLength(30);
      for (const request of requests) expectSigned(request, subscription.id);
      expect(new Set(requests.map((r) => r.headers['webhook-id'])).size, 'each webhook-id once').toBe(30);
      expect(requests.map((r) => (JSON.parse(r.body) as { data: { run_id: number } }).data.run_id).sort()).toEqual(runIds);
      expect(await started.deliveries()).toEqual([]);
      expect(await deliverDue(started)).toEqual(counts());
    });
  });

  test('the log, the cron replies and the tables hold no secret, no callback path or token, and nothing that an endpoint said', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const endpoints = [await endpoint(receiver, 'ok'), await endpoint(receiver, 'fail500'), await endpoint(receiver, 'gone')];
      const ids: string[] = [];
      for (const at of endpoints) ids.push((await subscribed(started, paramsFor(repoId, at))).id);
      await storeRun(started, repoId, 9951);

      const replies: string[] = [];
      const queued: string[] = [];
      for (let run = 0; run < 5; run++) {
        if (run > 0) await started.makeDeliveriesDue();
        const reply = await started.cronJob('deliver');
        replies.push(reply.body);
        parseReply(reply);
        queued.push(JSON.stringify(await started.deliveries()));
      }
      expect(queued[0], 'the first run left a delivery that waits for its retry').not.toBe('[]');
      expect(queued[4]).toBe('[]');

      const log = output();
      const [ok, failing, gone] = ids as [string, string, string];
      expect(log).toMatch(new RegExp(`\\[info\\]\\s+webhook delivered\\s.*subscription=${ok}\\s.*attempt=1\\s.*host=127\\.0\\.0\\.1\\b`));
      expect(log).toMatch(new RegExp(`\\[info\\]\\s+webhook failed, will retry\\s.*subscription=${failing}\\s.*category=http_5xx`));
      expect(log).toMatch(new RegExp(`\\[warn\\]\\s+webhook given up\\s.*subscription=${failing}\\s.*reason=attempts_exhausted`));
      expect(log).toMatch(new RegExp(`\\[warn\\]\\s+webhook given up\\s.*subscription=${gone}\\s.*reason=endpoint_gone\\s.*category=http_4xx`));

      const answers = replies.join('\n');
      const tables = JSON.stringify([queued, await Promise.all(endpoints.map((at) => started.subscriptions(at.id)))]);
      for (const at of endpoints) {
        const base64 = at.secret.slice('whsec_'.length);
        for (const needle of [at.secret, base64]) {
          expect(occurrences(tables, needle), `the tables hold ${needle.slice(0, 6)}...`).toBe(0);
        }
        expect(await started.secretCopies(at.secret), 'no column of the subscription holds the secret').toBe(0);
        for (const needle of [at.secret, base64, at.id, at.token, at.url]) {
          for (const [name, text] of [['the log', log], ['a cron reply', answers]] as const) {
            expect(occurrences(text, needle), `${name} holds ${needle.slice(0, 6)}...`).toBe(0);
          }
        }
      }
      expect(occurrences(log, '/hook/'), 'the log names the host and never the path').toBe(0);
      for (const [name, text] of [['the log', log], ['a cron reply', answers], ['the tables', tables]] as const) {
        expect(occurrences(text, ENDPOINT_SAYS), `${name} hold what an endpoint answered`).toBe(0);
      }
      expect(started.leaksSecret(log + answers + tables)).toBe(false);
    });
  });
});

import { setTimeout as sleep } from 'node:timers/promises';
import { CATEGORY, counts, deliverDue, endpoint, eventRequests, expectSigned, paramsFor, refreshed, storeRun, withDeliveryFaucet } from './delivery';
import { describe, expect, newRepoId, test } from './support';
import { newSecret, subscribed, unsubscribe } from './subscriptions';

const ISO_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;
// The wait after attempt 1, 2, 3 and 4. The fifth attempt has no wait, because nothing follows it.
const BACKOFF_SECONDS = [15, 60, 180, 360];

const STOPS = [
  { status: 410, behavior: 'gone', reason: 'endpoint_gone' },
  { status: 413, behavior: 'toolarge', reason: 'endpoint_payload_too_large' },
];

describe('webhook delivery, failures and retries', () => {
  test('a failed attempt is retried after its backoff with the same webhook-id, a new timestamp and a valid signature', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'fail-once');
      const params = paramsFor(repoId, at);
      const subscription = await subscribed(started, params);
      await storeRun(started, repoId, 9301);

      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));
      const [queued, ...rest] = await started.deliveries();
      expect(rest, 'one delivery waits for its retry').toEqual([]);
      expect(queued!.attempts).toBe(1);
      expect(queued!.lease_until, 'the failed attempt gave up its claim').toBeNull();
      const wait = queued!.next_attempt_at - queued!.enqueued_at;
      expect(wait, 'the first retry is 15 seconds after the attempt').toBeGreaterThanOrEqual(14_000);
      expect(wait).toBeLessThan(17_000);
      const [failed] = await eventRequests(receiver, at);
      expect(failed!.headers['webhook-id'], 'the row keeps the webhook-id of every attempt').toBe(queued!.message_id);

      const failing = await refreshed(started, params);
      expect(failing).toEqual({ active: true, lastError: 'http_5xx', failedSince: expect.stringMatching(ISO_UTC) });

      expect(await deliverDue(started), 'a run before the retry is due sends nothing').toEqual(counts());
      expect(await eventRequests(receiver, at)).toHaveLength(1);

      // webhook-timestamp counts whole seconds, so the retry must come in another second than the first attempt.
      await sleep(1100);
      expect(await started.makeDeliveriesDue()).toBe(1);
      expect(await deliverDue(started)).toEqual(counts({ claimed: 1, sent: 1, delivered: 1 }));

      const requests = await eventRequests(receiver, at);
      expect(requests).toHaveLength(2);
      const [first, retry] = requests as [(typeof requests)[number], (typeof requests)[number]];
      expectSigned(first, subscription.id);
      expectSigned(retry, subscription.id);
      expect(retry.headers['webhook-id']).toBe(first.headers['webhook-id']);
      expect(retry.headers['webhook-timestamp']).not.toBe(first.headers['webhook-timestamp']);
      expect(retry.headers['webhook-signature']).not.toBe(first.headers['webhook-signature']);
      expect(retry.body, 'the retry carries the same event').toBe(first.body);
      expect(await started.deliveries()).toEqual([]);

      const healthy = await refreshed(started, params);
      expect(healthy.lastError).toBeNull();
      expect(healthy.lastDeliveryAt).toMatch(ISO_UTC);
      expect(healthy.failedSince).toBeUndefined();
    });
  });

  test('a delivery gets five attempts on the 15 s, 1 min, 3 min and 6 min schedule and then ends as attempts_exhausted', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'fail500');
      const params = paramsFor(repoId, at);
      const subscription = await subscribed(started, params);
      await storeRun(started, repoId, 9302);

      let enqueuedAt: number | undefined;
      for (const [index, backoff] of BACKOFF_SECONDS.entries()) {
        const attempt = index + 1;
        if (attempt > 1) expect(await started.makeDeliveriesDue()).toBe(1);
        expect(await deliverDue(started), `attempt ${attempt}`).toEqual(counts({ enqueued: attempt === 1 ? 1 : 0, claimed: 1, sent: 1, retried: 1 }));
        const [row] = await started.deliveries();
        expect(row!.attempts).toBe(attempt);
        enqueuedAt ??= row!.enqueued_at;
        expect(row!.enqueued_at, 'the window of 15 minutes runs from the enqueue').toBe(enqueuedAt);
        const wait = row!.next_attempt_at - Date.now();
        expect(wait, `after attempt ${attempt} the wait is ${backoff} s`).toBeGreaterThan((backoff - 5) * 1000);
        expect(wait).toBeLessThan((backoff + 1) * 1000);
      }

      expect(await started.makeDeliveriesDue()).toBe(1);
      expect(await deliverDue(started), 'attempt 5').toEqual(counts({ claimed: 1, sent: 1, gave_up: { attempts_exhausted: 1 } }));
      expect(await started.deliveries()).toEqual([]);
      const requests = await eventRequests(receiver, at);
      expect(requests, 'exactly five requests').toHaveLength(5);
      for (const request of requests) expectSigned(request, subscription.id);
      expect(new Set(requests.map((r) => r.headers['webhook-id'])).size, 'one webhook-id for all five').toBe(1);
      expect(await started.subscriptions(at.id), 'the subscription stays').toHaveLength(1);

      expect(await started.makeDeliveriesDue()).toBe(0);
      expect(await deliverDue(started), 'a sixth run sends nothing').toEqual(counts());
      expect(await eventRequests(receiver, at)).toHaveLength(5);

      expect(output().match(/\[info\]\s+webhook failed, will retry\s.*category=http_5xx/g)).toHaveLength(4);
      expect(output()).toMatch(/\[warn\]\s+webhook given up\s.*reason=attempts_exhausted\s.*category=http_5xx/);
      const status = await refreshed(started, params);
      expect(status).toEqual({ active: true, lastError: 'http_5xx', failedSince: expect.stringMatching(ISO_UTC) });
    });
  });

  for (const { status, behavior, reason } of STOPS) {
    test(`an answer of ${status} ends that delivery as ${reason}, keeps the subscription, and the next event is delivered`, async ({ faucet, receiver }) => {
      await withDeliveryFaucet(faucet, async (started) => {
        const repoId = newRepoId();
        const at = await endpoint(receiver, `${behavior}-once`);
        const params = paramsFor(repoId, at);
        const subscription = await subscribed(started, params);
        const [before] = await started.subscriptions(at.id);
        await storeRun(started, repoId, 9401);

        expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, gave_up: { [reason]: 1 } }));
        expect(await eventRequests(receiver, at), 'one request, and no retry').toHaveLength(1);
        expect(await started.deliveries()).toEqual([]);
        expect(await started.subscriptions(at.id), 'the endpoint ended the delivery, not the subscription').toEqual([before]);
        expect((await refreshed(started, params)).lastError, 'a 410 or a 413 leaves the delivery status alone, as SEP-3415 says').toBeNull();

        await storeRun(started, repoId, 9402);
        expect(await deliverDue(started), 'the delivery that ended is not queued again').toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, delivered: 1 }));
        const requests = await eventRequests(receiver, at);
        expect(requests).toHaveLength(2);
        for (const request of requests) expectSigned(request, subscription.id);
        expect(new Set(requests.map((r) => r.headers['webhook-id'])).size, 'the two events have two webhook-ids').toBe(2);
        expect((await refreshed(started, params)).lastError).toBeNull();
      });
    });

    test(`an endpoint that always answers ${status} ends every delivery as ${reason} after one request and keeps the subscription`, async ({ faucet, receiver }) => {
      await withDeliveryFaucet(faucet, async (started) => {
        const repoId = newRepoId();
        const at = await endpoint(receiver, behavior);
        await subscribed(started, paramsFor(repoId, at));
        await storeRun(started, repoId, 9403);
        await storeRun(started, repoId, 9404);

        expect(await deliverDue(started)).toEqual(counts({ enqueued: 2, claimed: 2, sent: 2, gave_up: { [reason]: 2 } }));
        expect(await eventRequests(receiver, at)).toHaveLength(2);
        expect(await started.deliveries()).toEqual([]);
        expect(await started.subscriptions(at.id)).toHaveLength(1);
        expect(await deliverDue(started)).toEqual(counts());
        expect(await eventRequests(receiver, at)).toHaveLength(2);
      });
    });
  }

  test('the cursor of a body stays behind an event that is still unsent, and moves on when it ends', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'fail-once');
      const subscription = await subscribed(started, paramsFor(repoId, at));
      await storeRun(started, repoId, 9901);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));
      await storeRun(started, repoId, 9902);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, delivered: 1 }));
      expect(await started.makeDeliveriesDue()).toBe(1);
      expect(await deliverDue(started)).toEqual(counts({ claimed: 1, sent: 1, delivered: 1 }));
      await storeRun(started, repoId, 9903);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, delivered: 1 }));

      const [first, , third] = (await started.rows(repoId)).map((row) => row.id) as [number, number, number];
      const cursors = (await eventRequests(receiver, at)).map((request) => {
        const body = expectSigned(request, subscription.id);
        return [body.data.run_id, body.cursor];
      });
      expect(cursors, 'event 1 failed and was retried, so the cursor of event 2 and of its own retry is behind event 1').toEqual([
        [9901, `v1.${first - 1}`],
        [9902, `v1.${first - 1}`],
        [9901, `v1.${first - 1}`],
        [9903, `v1.${third - 1}`],
      ]);
    });
  });

  test('an endpoint that cannot verify the signature answers 401, and that is a failed attempt', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      const params = paramsFor(repoId, at);
      await subscribed(started, params);
      await receiver.register(at.id, newSecret());
      await storeRun(started, repoId, 9502);

      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));
      const requests = await eventRequests(receiver, at);
      expect(requests.map((r) => r.signature), 'the receiver tells a signature of another secret from a valid one').toEqual(['invalid']);
      expect((await refreshed(started, params)).lastError).toBe('http_4xx');
    });
  });

  test('a redirect is a failed attempt and is never followed', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'redirect-events');
      const params = paramsFor(repoId, at);
      await subscribed(started, params);
      await storeRun(started, repoId, 9501);

      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));
      expect((await receiver.requests(at.id)).map((r) => r.behavior), 'the target of the redirect was never requested').toEqual(['redirect-events', 'redirect-events']);
      const [row] = await started.deliveries();
      expect(row!.attempts).toBe(1);
      expect((await refreshed(started, params)).lastError).toMatch(CATEGORY);
    });
  });

  test('a subscription that expired gets no request, whether its event came before or after the expiry', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const early = await endpoint(receiver, 'ok');
      await subscribed(started, paramsFor(repoId, early));
      expect(await started.expireSubscriptions(early.id)).toBe(1);
      await storeRun(started, repoId, 9601);

      expect(await deliverDue(started), 'an expired subscription is not read').toEqual(counts());
      expect(await eventRequests(receiver, early)).toEqual([]);
      expect(await started.deliveries()).toEqual([]);

      const queued = await endpoint(receiver, 'fail500');
      await subscribed(started, paramsFor(repoId, queued));
      await storeRun(started, repoId, 9602);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));
      expect(await started.deliveries()).toHaveLength(1);

      expect(await started.expireSubscriptions(queued.id)).toBe(1);
      expect(await started.makeDeliveriesDue()).toBe(1);
      expect(await deliverDue(started), 'the delivery that waited for a retry ends with its subscription').toEqual(counts({ gave_up: { subscription_expired: 1 } }));
      expect(await eventRequests(receiver, queued), 'and the retry is never sent').toHaveLength(1);
      expect(await started.deliveries()).toEqual([]);
    });
  });

  test('a delivery that a crashed run claimed waits for its lease, then goes out with the webhook-id it was queued with', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'fail-once');
      const subscription = await subscribed(started, paramsFor(repoId, at));
      await storeRun(started, repoId, 9701);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));
      const [queued] = await started.deliveries();

      expect(await started.makeDeliveriesDue()).toBe(1);
      expect(await started.leaseDeliveries(30, 2)).toBe(1);
      const [leased] = await started.deliveries();
      expect(leased!.lease_until! - Date.now(), 'the claim of a run that never finished').toBeGreaterThan(20_000);
      expect(await deliverDue(started), 'a delivery that is leased is due and is not sent').toEqual(counts());
      expect(await eventRequests(receiver, at)).toHaveLength(1);
      expect(await started.deliveries()).toEqual([leased]);

      expect(await started.leaseDeliveries(-1, 2)).toBe(1);
      expect(await deliverDue(started)).toEqual(counts({ claimed: 1, sent: 1, delivered: 1 }));
      const requests = await eventRequests(receiver, at);
      expect(requests).toHaveLength(2);
      for (const request of requests) {
        expectSigned(request, subscription.id);
        expect(request.headers['webhook-id']).toBe(queued!.message_id);
      }
      expect(await started.deliveries()).toEqual([]);
    });
  });

  test('a crash costs the attempt: a delivery whose claim lapsed on its fifth attempt ends without a request', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'fail500');
      await subscribed(started, paramsFor(repoId, at));
      await storeRun(started, repoId, 9702);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));

      expect(await started.makeDeliveriesDue()).toBe(1);
      expect(await started.leaseDeliveries(-1, 5)).toBe(1);
      expect(await deliverDue(started)).toEqual(counts({ claimed: 1, gave_up: { attempts_exhausted: 1 } }));
      expect(await eventRequests(receiver, at), 'the sixth claim sent nothing').toHaveLength(1);
      expect(await started.deliveries()).toEqual([]);
    });
  });

  test('unsubscribing ends the deliveries that wait for a retry, and nothing is sent after it', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'fail500');
      const params = paramsFor(repoId, at);
      await subscribed(started, params);
      await storeRun(started, repoId, 9801);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, retried: 1 }));
      expect(await started.deliveries()).toHaveLength(1);

      const reply = await unsubscribe(started, params);
      expect(reply.error, reply.body).toBeNull();
      expect(await started.deliveries()).toEqual([]);
      expect(await started.makeDeliveriesDue()).toBe(0);
      expect(await deliverDue(started), 'the run does not count what the unsubscribe ended').toEqual(counts());
      expect(await eventRequests(receiver, at)).toHaveLength(1);
    });
  });
});

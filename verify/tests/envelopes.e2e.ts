import {
  counts,
  deliverDue,
  endpoint,
  envelopeRequests,
  eventRequests,
  expectSigned,
  expectSignedEnvelope,
  paramsFor,
  pollFrom,
  runPayload,
  storeRun,
  withDeliveryFaucet,
} from './delivery';
import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet } from './support';
import { subscribe, subscribed, occurrences } from './subscriptions';

const run = samples['workflow_run.completed'];
const TERMINATED = { type: 'terminated', error: { code: -32024, message: 'Forbidden', data: { reason: 'Access revoked' } } };

interface PurgeReply {
  deleted: number;
  purged_through: number;
}

// storeOld stores a run that a purge with the default retention of 30 days deletes.
async function storeOld(started: Faucet, repoId: number, runId: number): Promise<void> {
  const delivery = newDeliveryId();
  const reply = await started.deliver({ event: run.event, delivery, body: JSON.stringify(runPayload(repoId, runId)) });
  expect(reply.body, `workflow_run ${runId}`).toBe('stored\n');
  await started.backdate(delivery, 40);
}

async function purge(started: Faucet): Promise<PurgeReply> {
  const reply = await started.cronJob('purge');
  expect(reply.status, reply.body).toBe(200);
  return JSON.parse(reply.body) as PurgeReply;
}

describe('control envelopes', () => {
  test('a purge that outran a subscription sends one signed gap envelope whose cursor resumes at the first surviving event', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      const subscription = await subscribed(started, paramsFor(repoId, at));
      await storeOld(started, repoId, 9001);
      await storeOld(started, repoId, 9002);
      const purged = await purge(started);
      expect(purged.deleted, 'the purge deleted both runs').toBe(2);
      const survivor = await storeRun(started, repoId, 9003);

      const reply = await started.cronJob('deliver');
      expect(JSON.parse(reply.body), 'one gap, sent, and the survivor delivered').toEqual(
        counts({ enqueued: 1, gaps: 1, gaps_sent: 1, claimed: 1, sent: 1, delivered: 1 }),
      );
      expect(started.leaksSecret(reply.body)).toBe(false);

      const gaps = await envelopeRequests(receiver, at, 'gap');
      expect(gaps).toHaveLength(1);
      const body = expectSignedEnvelope(gaps[0]!, 'gap', subscription.id);
      expect(Object.keys(body).sort(), 'a gap body has a type and a cursor and nothing else').toEqual(['cursor', 'type']);
      expect(body.cursor, 'the cursor is the purge mark that the purge reported').toBe(`v1.${purged.purged_through}`);
      const resumed = await pollFrom(started, repoId, body.cursor as string);
      expect(resumed.map((e) => e.eventId), 'resuming from the gap cursor replays the survivor').toEqual([survivor]);

      const events = await eventRequests(receiver, at);
      expect(events).toHaveLength(1);
      expectSigned(events[0]!, subscription.id);

      expect(await deliverDue(started), 'a gap is sent once').toEqual(counts());
      expect(await envelopeRequests(receiver, at, 'gap')).toHaveLength(1);
      expect(output().match(/gap envelope was sent/g)).toHaveLength(1);
      expect(started.leaksSecret(output())).toBe(false);
      expect(output()).not.toContain(at.token);
    });
  });

  test('a gap envelope that the endpoint refuses is counted once and not retried, and the events still flow', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'fail500');
      await subscribed(started, paramsFor(repoId, at));
      await storeOld(started, repoId, 9101);
      await purge(started);
      await storeRun(started, repoId, 9102);

      expect(await deliverDue(started), 'the failed gap is counted and the event is queued for its retry').toEqual(
        counts({ enqueued: 1, gaps: 1, gaps_failed: 1, claimed: 1, sent: 1, retried: 1 }),
      );
      await started.makeDeliveriesDue();
      expect(await deliverDue(started)).toEqual(counts({ claimed: 1, sent: 1, retried: 1 }));

      expect(await envelopeRequests(receiver, at, 'gap'), 'one attempt, however many runs follow').toHaveLength(1);
      expect(output().match(/gap envelope was not delivered.*reason=http_5xx/g)).toHaveLength(1);
      expect(started.leaksSecret(output())).toBe(false);
    });
  });

  test('a renewal that lost access sends one signed terminated envelope and ends the subscription, and one that kept access sends none', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      const params = paramsFor(repoId, at);
      const subscription = await subscribed(started, params);

      await subscribed(started, params);
      expect(await envelopeRequests(receiver, at, 'terminated'), 'a renewal with access sends nothing').toEqual([]);
      expect(await started.subscriptions(at.id)).toHaveLength(1);

      await receiver.revoke(repoId);
      const refused = await subscribe(started, params);
      expect(refused.error?.code, 'the renewal gets the Forbidden of a denied subscribe').toBe(-32024);

      const envelopes = await envelopeRequests(receiver, at, 'terminated');
      expect(envelopes).toHaveLength(1);
      expectSignedEnvelope(envelopes[0]!, 'terminated', subscription.id);
      expect(JSON.parse(envelopes[0]!.body)).toEqual(TERMINATED);
      expect(await started.subscriptions(at.id), 'the subscription is gone').toEqual([]);

      expect((await subscribe(started, params)).error?.code).toBe(-32024);
      expect(await envelopeRequests(receiver, at, 'terminated'), 'a second renewal finds nothing to end').toHaveLength(1);
      expect(await deliverDue(started), 'nothing is left to deliver').toEqual(counts());
      expect(occurrences(output(), 'the terminated envelope was sent')).toBe(1);
      expect(started.leaksSecret(output())).toBe(false);
      expect(output()).not.toContain(at.token);
    });
  });

  test('a first subscribe to a repository the token cannot read sends nothing, and a terminated envelope that is refused is not retried', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      await receiver.revoke(repoId);
      expect((await subscribe(started, paramsFor(repoId, at))).error?.code).toBe(-32024);
      expect(await envelopeRequests(receiver, at, 'terminated'), 'there was no subscription to end').toEqual([]);

      const failing = await endpoint(receiver, 'fail500');
      const otherRepoId = newRepoId();
      const params = paramsFor(otherRepoId, failing);
      await subscribed(started, params);
      await receiver.revoke(otherRepoId);
      expect((await subscribe(started, params)).error?.code).toBe(-32024);
      expect((await subscribe(started, params)).error?.code).toBe(-32024);

      expect(await envelopeRequests(receiver, failing, 'terminated'), 'one attempt').toHaveLength(1);
      expect(await started.subscriptions(failing.id), 'the subscription is gone although the envelope was refused').toEqual([]);
      expect(output().match(/terminated envelope was not delivered.*reason=http_5xx/g)).toHaveLength(1);
      expect(started.leaksSecret(output())).toBe(false);
    });
  });
});

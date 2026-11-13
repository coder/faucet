import { createHmac } from 'node:crypto';
import { counts, deliverDue, endpoint, envelopeRequests, eventRequests, expectSigned, expectSignedEnvelope, paramsFor, storeRun, withDeliveryFaucet } from './delivery';
import { describe, expect, newRepoId, test, type ReceivedRequest } from './support';
import { newSecret, subscribe, subscribed } from './subscriptions';

const WINDOW_MS = 10 * 60_000;

// signature is the entry that webhook-signature must hold for secret: an independent reading of Standard Webhooks.
function signature(secret: string, request: ReceivedRequest): string {
  const key = Buffer.from(secret.slice('whsec_'.length), 'base64');
  const mac = createHmac('sha256', key).update(`${request.headers['webhook-id']}.${request.headers['webhook-timestamp']}.`).update(request.body);
  return `v1,${mac.digest('base64')}`;
}

function entries(request: ReceivedRequest): string[] {
  return (request.headers['webhook-signature'] ?? '').split(' ').sort();
}

describe('secret rotation', () => {
  test('a refresh with a new secret signs deliveries with both secrets for the window and with the new one after it, and the previous secret is sealed and then removed', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      const oldSecret = at.secret;
      const nextSecret = newSecret();
      const params = paramsFor(repoId, at);
      const subscription = await subscribed(started, params);

      await subscribed(started, params);
      const [same] = await started.subscriptions(at.id);
      expect(same!.previous_secret_enc, 'a refresh with the same secret starts no window').toBeNull();
      expect(same!.previous_secret_until).toBeNull();

      const before = Date.now();
      await subscribed(started, { ...params, delivery: { url: at.url, secret: nextSecret } });
      const [row] = await started.subscriptions(at.id);
      expect(started.openSecret(row!), 'the current secret is the new one').toBe(nextSecret);
      expect(started.openPreviousSecret(row!), 'the previous secret is the old one, sealed with the row key').toBe(oldSecret);
      expect(row!.previous_secret_until!, 'the window ends within 10 minutes of the refresh').toBeGreaterThan(before + WINDOW_MS - 1000);
      expect(row!.previous_secret_until!).toBeLessThanOrEqual(Date.now() + WINDOW_MS);
      expect([await started.secretCopies(oldSecret), await started.secretCopies(nextSecret)], 'no row holds either secret in the clear').toEqual([0, 0]);

      await storeRun(started, repoId, 9201);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, delivered: 1 }));
      const [heldOld] = await eventRequests(receiver, at);
      expectSigned(heldOld!, subscription.id);
      expect(entries(heldOld!), 'a receiver that holds only the old secret verifies, and the new secret signs too').toEqual([signature(oldSecret, heldOld!), signature(nextSecret, heldOld!)].sort());

      await receiver.register(at.id, nextSecret);
      await storeRun(started, repoId, 9202);
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 1, claimed: 1, sent: 1, delivered: 1 }));
      const heldNew = (await eventRequests(receiver, at))[1]!;
      expectSigned(heldNew, subscription.id);
      expect(entries(heldNew), 'a receiver that holds only the new secret verifies').toEqual([signature(oldSecret, heldNew), signature(nextSecret, heldNew)].sort());

      expect(await started.endRotationWindows(at.id)).toBe(1);
      await storeRun(started, repoId, 9203);
      expect(await deliverDue(started), 'the run removes the ended window before it sends').toEqual(
        counts({ enqueued: 1, claimed: 1, sent: 1, delivered: 1, previous_secrets_cleared: 1 }),
      );
      const after = (await eventRequests(receiver, at))[2]!;
      expectSigned(after, subscription.id);
      expect(entries(after), 'after the window only the new secret signs').toEqual([signature(nextSecret, after)]);
      expect(entries(after)).not.toContain(signature(oldSecret, after));

      const [cleared] = await started.subscriptions(at.id);
      expect([cleared!.previous_secret_enc, cleared!.previous_secret_until], 'the sealed previous secret is gone from the row').toEqual([null, null]);
      expect(started.openSecret(cleared!), 'the subscription and its current secret are untouched').toBe(nextSecret);
      expect(await deliverDue(started), 'nothing is left to remove').toEqual(counts());

      for (const secret of [oldSecret, nextSecret]) {
        expect(output()).not.toContain(secret.slice('whsec_'.length));
      }
      expect(started.leaksSecret(output())).toBe(false);
    });
  });

  test('a terminated envelope is signed with both stored secrets during the window and never with the secret of the renewal', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const at = await endpoint(receiver, 'ok');
      const oldSecret = at.secret;
      const nextSecret = newSecret();
      const renewalSecret = newSecret();
      const params = paramsFor(repoId, at);
      const subscription = await subscribed(started, params);
      await subscribed(started, { ...params, delivery: { url: at.url, secret: nextSecret } });

      await receiver.revoke(repoId);
      const refused = await subscribe(started, { ...params, delivery: { url: at.url, secret: renewalSecret } });
      expect(refused.error?.code, 'the renewal gets the Forbidden of a denied subscribe').toBe(-32024);

      const [terminated] = await envelopeRequests(receiver, at, 'terminated');
      expectSignedEnvelope(terminated!, 'terminated', subscription.id);
      expect(entries(terminated!)).toEqual([signature(oldSecret, terminated!), signature(nextSecret, terminated!)].sort());
      expect(entries(terminated!)).not.toContain(signature(renewalSecret, terminated!));
      for (const secret of [oldSecret, nextSecret, renewalSecret]) {
        expect(output()).not.toContain(secret.slice('whsec_'.length));
      }
    });
  });
});

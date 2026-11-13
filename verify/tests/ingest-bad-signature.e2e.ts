import { closedPullRequest } from './payloads';
import { describe, expect, type Faucet, newDeliveryId, newRepoId, test } from './support';

describe('ingest bad signature', () => {
  const rejected: [string, (faucet: Faucet, body: string) => string | null][] = [
    ['no signature header', () => null],
    ['a signature made with another secret', (faucet, body) => faucet.sign(body, 'not-the-webhook-secret')],
    ['a signature over a different body', (faucet) => faucet.sign('{"tampered":true}')],
    ['a sha1 signature', () => `sha1=${'0'.repeat(40)}`],
    ['a digest that is not hex', () => `sha256=${'z'.repeat(64)}`],
    ['a digest that is too short', () => 'sha256=abcd'],
  ];

  for (const [title, makeSignature] of rejected) {
    test(`${title} is rejected with 401 and stores nothing`, async ({ faucet }) => {
      const repoId = newRepoId();
      const body = JSON.stringify(closedPullRequest(repoId));

      const reply = await faucet.deliver({
        event: 'pull_request',
        delivery: newDeliveryId(),
        body,
        signature: makeSignature(faucet, body),
      });

      expect(reply.status).toBe(401);
      expect(reply.body).toBe('invalid signature\n');
      expect(await faucet.rows(repoId)).toHaveLength(0);
    });
  }

  test('a missing signature is rejected before the missing headers are noticed', async ({ faucet }) => {
    const reply = await faucet.deliver({ body: '{}', signature: null });

    expect(reply.status).toBe(401);
  });
});

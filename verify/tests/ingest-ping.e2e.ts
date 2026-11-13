import { describe, expect, newDeliveryId, newRepoId, test } from './support';

describe('ingest ping', () => {
  test('a signed ping is answered with pong and stores nothing', async ({ faucet }) => {
    const repoId = newRepoId();

    const reply = await faucet.deliver({
      event: 'ping',
      delivery: newDeliveryId(),
      body: JSON.stringify({ zen: 'Keep it logically awesome.', hook_id: 1, repository: { id: repoId } }),
    });

    expect(reply.status).toBe(200);
    expect(reply.body).toBe('pong\n');
    expect(await faucet.rows(repoId)).toHaveLength(0);
  });

  test('a ping with a wrong signature is rejected', async ({ faucet }) => {
    const body = JSON.stringify({ zen: 'Keep it logically awesome.', hook_id: 1 });

    const reply = await faucet.deliver({ event: 'ping', delivery: newDeliveryId(), body, signature: faucet.sign(body, 'not-the-webhook-secret') });

    expect(reply.status).toBe(401);
  });
});

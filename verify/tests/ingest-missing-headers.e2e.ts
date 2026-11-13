import { closedPullRequest } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';

describe('ingest missing headers', () => {
  test('a signed delivery without X-GitHub-Event is rejected with 400', async ({ faucet }) => {
    const repoId = newRepoId();

    const reply = await faucet.deliver({ delivery: newDeliveryId(), body: JSON.stringify(closedPullRequest(repoId)) });

    expect(reply.status).toBe(400);
    expect(reply.body).toBe('missing event or delivery header\n');
    expect(await faucet.rows(repoId)).toHaveLength(0);
  });

  test('a signed delivery without X-GitHub-Delivery is rejected with 400', async ({ faucet }) => {
    const repoId = newRepoId();

    const reply = await faucet.deliver({ event: 'pull_request', body: JSON.stringify(closedPullRequest(repoId)) });

    expect(reply.status).toBe(400);
    expect(reply.body).toBe('missing event or delivery header\n');
    expect(await faucet.rows(repoId)).toHaveLength(0);
  });
});

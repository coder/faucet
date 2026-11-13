import { closedPullRequest } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';

describe('ingest wrong method', () => {
  test('GET /webhook/github is not allowed', async ({ faucet }) => {
    const reply = await faucet.request('/webhook/github');

    expect(reply.status).toBe(405);
  });

  for (const method of ['PUT', 'PATCH', 'DELETE']) {
    test(`a signed ${method} is not allowed and stores nothing`, async ({ faucet }) => {
      const repoId = newRepoId();
      const body = JSON.stringify(closedPullRequest(repoId));

      const reply = await faucet.request('/webhook/github', {
        method,
        body,
        headers: { 'X-GitHub-Event': 'pull_request', 'X-GitHub-Delivery': newDeliveryId(), 'X-Hub-Signature-256': faucet.sign(body) },
      });

      expect(reply.status).toBe(405);
      expect(await faucet.rows(repoId)).toHaveLength(0);
    });
  }
});

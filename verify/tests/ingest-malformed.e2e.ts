import { describe, expect, newDeliveryId, newRepoId, test } from './support';

describe('ingest malformed', () => {
  const malformed: [string, (repoId: number) => string][] = [
    ['a body that is not JSON', () => 'not json'],
    ['a payload with no action', (repoId) => JSON.stringify({ repository: { id: repoId } })],
    ['a payload with no pull request number', (repoId) => JSON.stringify({ action: 'closed', repository: { id: repoId }, pull_request: {} })],
    ['a payload with no repository id', () => JSON.stringify({ action: 'closed', number: 7, pull_request: {} })],
  ];

  for (const [title, makeBody] of malformed) {
    test(`${title} is rejected with 400 and stores nothing`, async ({ faucet }) => {
      const repoId = newRepoId();

      const reply = await faucet.deliver({ event: 'pull_request', delivery: newDeliveryId(), body: makeBody(repoId) });

      expect(reply.status).toBe(400);
      expect(reply.body).toContain('malformed payload');
      expect(await faucet.rows(repoId)).toHaveLength(0);
    });
  }
});

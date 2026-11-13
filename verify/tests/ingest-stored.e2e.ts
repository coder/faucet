import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';

describe('ingest stored', () => {
  for (const [key, sample] of Object.entries(samples)) {
    test(`${key} is stored as ${sample.expected.type}`, async ({ faucet }) => {
      const repoId = newRepoId();
      const delivery = newDeliveryId();

      const reply = await faucet.deliver({
        event: sample.event,
        delivery,
        body: JSON.stringify(sample.payload(repoId)),
      });
      const rows = await faucet.rows(repoId);

      expect(reply.status).toBe(200);
      expect(reply.body).toBe('stored\n');
      expect(rows).toHaveLength(1);
      expect(rows[0]).toMatchObject({
        source: 'github',
        delivery_id: delivery,
        type: sample.expected.type,
        repo_id: repoId,
        prs: sample.expected.prs,
        head_sha: sample.expected.headSha,
      });
      expect(rows[0]?.data).toEqual(sample.expected.data);
    });
  }
});

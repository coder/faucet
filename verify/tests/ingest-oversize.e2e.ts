import { closedPullRequest } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';

const LIMIT_BYTES = 25 * 1024 * 1024;

function paddedClosedPullRequest(repoId: number, totalBytes: number): string {
  const base = JSON.stringify({ ...closedPullRequest(repoId), padding: '' });
  const padding = totalBytes - Buffer.byteLength(base);
  const body = base.replace('"padding":""', `"padding":"${'a'.repeat(padding)}"`);
  if (Buffer.byteLength(body) !== totalBytes) throw new Error(`built a ${Buffer.byteLength(body)} byte body, wanted ${totalBytes}`);
  return body;
}

describe('ingest oversize', () => {
  test('a signed body of exactly 25 MiB is accepted', async ({ faucet }) => {
    const repoId = newRepoId();

    const reply = await faucet.deliver({ event: 'pull_request', delivery: newDeliveryId(), body: paddedClosedPullRequest(repoId, LIMIT_BYTES) });

    expect(reply.status).toBe(200);
    expect(reply.body).toBe('stored\n');
    expect(await faucet.rows(repoId)).toHaveLength(1);
  });

  test('a signed body one byte over 25 MiB is rejected with 413 and stores nothing', async ({ faucet }) => {
    const repoId = newRepoId();

    const reply = await faucet.deliver({ event: 'pull_request', delivery: newDeliveryId(), body: paddedClosedPullRequest(repoId, LIMIT_BYTES + 1) });

    expect(reply.status).toBe(413);
    expect(reply.body).toBe('body too large\n');
    expect(await faucet.rows(repoId)).toHaveLength(0);
  });
});

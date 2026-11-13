import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';

const sample = samples['workflow_run.completed'];

describe('ingest duplicate', () => {
  test('a redelivered event is acknowledged and stored once', async ({ faucet }) => {
    const repoId = newRepoId();
    const delivery = { event: sample.event, delivery: newDeliveryId(), body: JSON.stringify(sample.payload(repoId)) };

    const first = await faucet.deliver(delivery);
    const [stored] = await faucet.rows(repoId);
    const second = await faucet.deliver(delivery);
    const rows = await faucet.rows(repoId);

    expect(first.body).toBe('stored\n');
    expect(second.status).toBe(200);
    expect(second.body).toBe('duplicate\n');
    expect(rows).toEqual([stored]);
  });

  test('a replay with the same delivery id and a different body keeps the first row', async ({ faucet }) => {
    const repoId = newRepoId();
    const deliveryId = newDeliveryId();
    const original = sample.payload(repoId) as { workflow_run: { conclusion: string } };
    const changed = structuredClone(original);
    changed.workflow_run.conclusion = 'failure';

    await faucet.deliver({ event: sample.event, delivery: deliveryId, body: JSON.stringify(original) });
    const replay = await faucet.deliver({ event: sample.event, delivery: deliveryId, body: JSON.stringify(changed) });
    const rows = await faucet.rows(repoId);

    expect(replay.body).toBe('duplicate\n');
    expect(rows).toHaveLength(1);
    expect(rows[0]?.data).toMatchObject({ conclusion: 'success' });
  });
});

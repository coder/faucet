import { describe, expect, newRepoId, test } from './support';
import { delivery, newMarker, newSecret, occurrences, subscribe, subscribeParams, type Params } from './subscriptions';

describe('events subscribe with a wrongly typed parameter', () => {
  test('is refused with -32602 that does not quote the secret, and writes nothing', async ({ faucet }) => {
    const marker = newMarker();
    const secret = newSecret();
    const cases: [string, Params, string | undefined][] = [
      ['a cursor that is a number', { cursor: 5 }, undefined],
      ['maxAgeMs as text', { cursor: 'v1.0', maxAgeMs: 'a minute' }, undefined],
      ['maxAgeMs as a fraction', { cursor: 'v1.0', maxAgeMs: 1.5 }, undefined],
      ['a name that is a number', { name: 5 }, '5'],
    ];

    for (const [label, over, name] of cases) {
      const reply = await subscribe(faucet, subscribeParams(newRepoId(), marker, { delivery: delivery(marker, { secret }), ...over }), name === undefined ? {} : { name });

      expect(reply.status, label).toBe(400);
      expect(reply.error?.code, label).toBe(-32602);
      expect(occurrences(reply.body, secret), `${label} quotes the secret`).toBe(0);
    }
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });
});

import { appLog, describe, expect, newRepoId, test } from './support';
import {
  callbackUrl,
  delivery,
  newMarker,
  newSecret,
  occurrences,
  repoOf,
  subscribeParams,
  subscribed,
  unsubscribe,
  unsubscribedLine,
  unsubscribeParams,
} from './subscriptions';

const NO_SUCH_SUBSCRIPTION = { code: -32023, message: 'no such subscription', data: { kind: 'subscription' } };

describe('events unsubscribe', () => {
  test('removes the caller\'s own subscription, answers complete and logs it without the callback URL', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const params = subscribeParams(repoId, marker);
    const { id } = await subscribed(faucet, params);

    const reply = await unsubscribe(faucet, params);

    expect(reply.status).toBe(200);
    expect(reply.error).toBeNull();
    expect(reply.result).toMatchObject({ resultType: 'complete' });
    expect(await faucet.subscriptions(marker)).toEqual([]);
    await expect.poll(() => unsubscribedLine(repoId, id).test(appLog())).toBe(true);
    expect(appLog()).toMatch(new RegExp(`\\[info\\]\\s+unsubscribed\\s.*login=verify-user\\s+subscription=${id}\\b`));
    expect(occurrences(appLog(), marker)).toBe(0);
  });

  test('removes only that subscription and leaves the same principal\'s others', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const gone = subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker, '/gone') }) });
    const kept = subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker, '/kept') }) });
    await subscribed(faucet, gone);
    await subscribed(faucet, kept);

    await unsubscribe(faucet, gone);
    const rows = await faucet.subscriptions(marker);

    expect(rows.map((r) => r.callback_url)).toEqual([callbackUrl(marker, '/kept')]);
  });

  test('a second call for the same subscription is -32023, no such subscription', async ({ faucet }) => {
    const marker = newMarker();
    const params = subscribeParams(newRepoId(), marker);
    await subscribed(faucet, params);
    await unsubscribe(faucet, params);

    const again = await unsubscribe(faucet, params);

    expect(again.status).toBe(200);
    expect(again.result).toBeNull();
    expect(again.error).toEqual(NO_SUCH_SUBSCRIPTION);
  });

  test('another principal gets -32023, the same reply as for a key nobody subscribed, and the row stays', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const secret = newSecret();
    const params = subscribeParams(repoId, marker, { delivery: delivery(marker, { secret }) });
    await subscribed(faucet, params);
    const before = await faucet.subscriptions(marker);
    const neverSubscribed = subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker, '/never') }) });

    const theirs = await unsubscribe(faucet, params, { as: 'other' });
    const nobodys = await unsubscribe(faucet, neverSubscribed, { as: 'other' });
    const mineNever = await unsubscribe(faucet, neverSubscribed);
    const after = await faucet.subscriptions(marker);
    const mine = await unsubscribe(faucet, params);

    for (const reply of [theirs, nobodys, mineNever]) {
      expect(reply.status).toBe(200);
      expect(reply.result).toBeNull();
      expect(reply.error).toEqual(NO_SUCH_SUBSCRIPTION);
    }
    expect(theirs.body).toBe(nobodys.body);
    expect(theirs.body).toBe(mineNever.body);
    expect(after).toEqual(before);
    expect(faucet.openSecret(after[0]!)).toBe(secret);
    expect(mine.error).toBeNull();
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('the second principal removes its own subscription and leaves the first one\'s', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const params = subscribeParams(repoId, marker);
    await subscribed(faucet, params);
    await subscribed(faucet, params, { as: 'other' });

    const reply = await unsubscribe(faucet, params, { as: 'other' });
    const rows = await faucet.subscriptions(marker);

    expect(reply.error).toBeNull();
    expect(rows.map((r) => r.principal)).toEqual(['verify-user']);
  });

  test('the event, arguments and URL are the key: a near miss removes nothing and the exact key removes the row', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const repo = repoOf(repoId);
    const url = callbackUrl(marker);
    const params = subscribeParams(repoId, marker, { arguments: { repo, pr: 1 }, delivery: delivery(marker, { url }) });
    const misses: [string, object][] = [
      ['another pr', { arguments: { repo, pr: 2 } }],
      ['no pr', { arguments: { repo } }],
      ['another readable repository', { arguments: { repo: repoOf(newRepoId()), pr: 1 } }],
      ['another event', { name: 'pr.closed' }],
      ['another path', { delivery: { url: callbackUrl(marker, '/other') } }],
      ['another query', { delivery: { url: `${url}&x=1` } }],
    ];
    await subscribed(faucet, params);
    const before = await faucet.subscriptions(marker);

    for (const [label, over] of misses) {
      const reply = await faucet.mcp('events/unsubscribe', { ...unsubscribeParams(params), ...over });

      expect(reply.error, label).toEqual(NO_SUCH_SUBSCRIPTION);
    }
    const afterMisses = await faucet.subscriptions(marker);
    const exact = await unsubscribe(faucet, params);

    expect(afterMisses).toEqual(before);
    expect(exact.error).toBeNull();
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('arguments in another key order and a callback host in upper case with the default port name the same subscription', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const repo = repoOf(repoId);
    await subscribed(faucet, subscribeParams(repoId, marker, { arguments: { repo, pr: 3 } }));

    const reply = await faucet.mcp('events/unsubscribe', {
      name: 'ci.run_completed',
      arguments: { pr: 3, repo },
      delivery: { url: callbackUrl(marker).replace('hooks.verify.test', 'HOOKS.Verify.Test:443') },
    });

    expect(reply.error).toBeNull();
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('needs no repository access and no allowlist: a repository the token cannot read and a URL off the allowlist are -32023, not -32024 or -32027', async ({ faucet }) => {
    const marker = newMarker();
    const params = subscribeParams(newRepoId(), marker);
    await subscribed(faucet, params);

    const hidden = await faucet.mcp('events/unsubscribe', { ...unsubscribeParams(params), arguments: { repo: 'other/repo' } });
    const missing = await faucet.mcp('events/unsubscribe', { ...unsubscribeParams(params), arguments: { repo: 'verify/never-existed' } });
    const offList = await faucet.mcp('events/unsubscribe', { ...unsubscribeParams(params), delivery: { url: `https://evil.verify.test/hook/${marker}` } });

    for (const reply of [hidden, missing, offList]) {
      expect(reply.status).toBe(200);
      expect(reply.error).toEqual(NO_SUCH_SUBSCRIPTION);
    }
    expect(await faucet.subscriptions(marker)).toHaveLength(1);
  });

  test('a request without a valid token is 401, and a parameter that is not valid is refused with its own error, and nothing is removed', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const params = subscribeParams(repoId, marker);
    const keyed = unsubscribeParams(params);
    await subscribed(faucet, params);
    const cases: [string, object, number, string][] = [
      ['an unknown event name', { ...keyed, name: 'ci.nope' }, -32023, 'unknown event type'],
      ['an http callback URL', { ...keyed, delivery: { url: `http://hooks.verify.test/hook/${marker}` } }, -32602, 'scheme must be https'],
      ['a private callback address', { ...keyed, delivery: { url: `https://10.1.2.3/hook/${marker}` } }, -32602, 'host must be a public address'],
      ['no delivery', { ...keyed, delivery: undefined }, -32602, 'delivery'],
      ['no delivery url', { ...keyed, delivery: {} }, -32602, 'delivery.url is required'],
      ['a delivery mode other than webhook', { ...keyed, delivery: { mode: 'push', url: callbackUrl(marker) } }, -32026, 'webhook delivery mode'],
      ['no repo in the arguments', { ...keyed, arguments: {} }, -32602, 'repo'],
    ];

    const none = await faucet.mcp('events/unsubscribe', keyed, { token: null });
    const wrong = await faucet.mcp('events/unsubscribe', keyed, { token: `ghp_${'0'.repeat(40)}` });
    const unnamed = await faucet.mcp('events/unsubscribe', keyed, { name: null });

    for (const reply of [none, wrong]) {
      expect(reply.status).toBe(401);
      expect(reply.body).toBe('unauthorized\n');
    }
    expect(unnamed.error?.code).toBe(-32020);
    for (const [label, body, code, message] of cases) {
      const reply = await faucet.mcp('events/unsubscribe', body as Record<string, unknown>);

      expect(reply.error?.code, label).toBe(code);
      expect(reply.error?.message, label).toContain(message);
    }
    expect(await faucet.subscriptions(marker)).toHaveLength(1);
  });
});

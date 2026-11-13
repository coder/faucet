import { randomBytes } from 'node:crypto';
import { samples } from './payloads';
import { appLog, describe, expect, newDeliveryId, newRepoId, test, type Faucet } from './support';
import {
  CALLBACK_ORIGIN,
  EVENT,
  MAX_TTL_MS,
  MIN_TTL_MS,
  REPO_DENIED,
  SUBSCRIPTION_LIMIT,
  callbackUrl,
  delivery,
  newMarker,
  newSecret,
  occurrences,
  repoOf,
  subscribe,
  subscribeParams,
  subscribed,
  subscribedLine,
  unsubscribe,
  unsubscribeParams,
  type Params,
} from './subscriptions';

const run = samples['workflow_run.completed'];
const UTC_SECONDS = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;

interface PollResult {
  cursor: string;
}

function fails(fn: () => unknown): boolean {
  try {
    fn();
    return false;
  } catch {
    return true;
  }
}

function floorSecond(ms: number): number {
  return Math.floor(ms / 1000) * 1000;
}

async function deliverRun(faucet: Faucet, repoId: number): Promise<void> {
  const reply = await faucet.deliver({ event: run.event, delivery: newDeliveryId(), body: JSON.stringify(run.payload(repoId)) });
  expect(reply.body, 'delivery').toBe('stored\n');
}

async function newestEventId(faucet: Faucet): Promise<number> {
  return Number(await faucet.sql('select coalesce(max(id), 0) from faucet_events'));
}

describe('events subscribe', () => {
  test('a readable repository gets one stored row and the id, expiry and cursor that the reply names', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const sha = randomBytes(20).toString('hex');
    const params = subscribeParams(repoId, marker, { arguments: { repo: repoOf(repoId), sha, pr: 7 } });
    await deliverRun(faucet, repoId);
    const newestBefore = await newestEventId(faucet);

    const sentAt = Date.now();
    const reply = await subscribe(faucet, params);
    const repliedAt = Date.now();
    const newestAfter = await newestEventId(faucet);
    const rows = await faucet.subscriptions(marker);
    const [row] = rows;

    expect(reply.status).toBe(200);
    expect(reply.error).toBeNull();
    expect(reply.result).toMatchObject({ resultType: 'complete', truncated: false });
    expect(reply.result?.id).toMatch(/^sub_[0-9a-f]{16}$/);
    expect(reply.result?.refreshBefore).toMatch(UTC_SECONDS);
    expect(reply.result?.cursor).toMatch(/^v1\.\d+$/);
    expect(rows).toHaveLength(1);
    expect(row).toMatchObject({
      principal: 'verify-user',
      callback_url: (params.delivery as { url: string }).url,
      event_type: EVENT,
      arguments: { repo: repoOf(repoId), sha, pr: 7 },
      repo_id: repoId,
    });
    expect(row?.key_hash.startsWith((reply.result?.id ?? '').slice('sub_'.length)), 'id is the start of key_hash').toBe(true);
    expect(row?.expires_at).toBe(Date.parse(reply.result?.refreshBefore ?? ''));
    expect(row?.created_at).toBe(row?.updated_at);
    expect(row?.created_at).toBeGreaterThanOrEqual(sentAt);
    expect(row?.created_at).toBeLessThanOrEqual(repliedAt);
    expect(reply.result?.cursor).toBe(`v1.${row?.cursor}`);
    expect(row?.cursor).toBeGreaterThanOrEqual(newestBefore);
    expect(row?.cursor).toBeLessThanOrEqual(newestAfter);
  });

  test('a subscription without a cursor starts at the newest event and one with an earlier poll cursor starts there', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const polled = await faucet.mcp<PollResult>('events/poll', { name: EVENT, arguments: { repo: repoOf(repoId) } });
    const earlier = polled.result?.cursor ?? '';
    await deliverRun(faucet, repoId);
    const newest = await newestEventId(faucet);

    const atHead = await subscribed(faucet, subscribeParams(repoId, marker));
    const replayed = await subscribed(faucet, subscribeParams(repoId, marker, { cursor: earlier, delivery: delivery(marker, { url: callbackUrl(marker, '/replay') }) }));
    const rows = await faucet.subscriptions(marker);

    expect(earlier).toMatch(/^v1\.\d+$/);
    expect(Number(earlier.slice('v1.'.length))).toBeLessThan(newest);
    expect(Number(atHead.cursor.slice('v1.'.length))).toBeGreaterThanOrEqual(newest);
    expect(replayed).toMatchObject({ cursor: earlier, truncated: false });
    expect(rows.map((r) => `v1.${r.cursor}`)).toEqual([atHead.cursor, earlier]);
  });

  test('the delivery mode may be left out or be webhook, and both name the same subscription', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const url = callbackUrl(marker);

    const omitted = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url }) }));
    const named = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url, mode: 'webhook' }) }));
    const nulled = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url, mode: null }) }));

    expect(named.id).toBe(omitted.id);
    expect(nulled.id).toBe(omitted.id);
    expect(await faucet.subscriptions(marker)).toHaveLength(1);
  });

  test('a repository the token cannot read gets the same -32024 reply as one that does not exist, and nothing is written', async ({ faucet }) => {
    const marker = newMarker();
    const hidden = await subscribe(faucet, subscribeParams(newRepoId(), marker, { arguments: { repo: 'other/repo' } }));
    const missing = await subscribe(faucet, subscribeParams(newRepoId(), marker, { arguments: { repo: 'verify/never-existed' } }));

    for (const reply of [hidden, missing]) {
      expect(reply.status).toBe(200);
      expect(reply.result).toBeNull();
      expect(reply.error).toEqual({ code: -32024, message: REPO_DENIED });
    }
    expect(hidden.body).toBe(missing.body);
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('a bad parameter is refused before repository access, and repository access before the callback origin', async ({ faucet }) => {
    const marker = newMarker();
    const repoId = newRepoId();
    const hidden = { arguments: { repo: 'other/repo' } };
    const offList = { delivery: delivery(marker, { url: `https://evil.verify.test/hook/${marker}` }) };
    const notHttps = { delivery: delivery(marker, { url: `http://hooks.verify.test/hook/${marker}` }) };

    const badParamHidden = await subscribe(faucet, subscribeParams(repoId, marker, { ...hidden, ...notHttps }));
    const hiddenOffList = await subscribe(faucet, subscribeParams(repoId, marker, { ...hidden, ...offList }));
    const readableOffList = await subscribe(faucet, subscribeParams(repoId, marker, offList));

    expect(badParamHidden.error?.code).toBe(-32602);
    expect(hiddenOffList.error?.code).toBe(-32024);
    expect(readableOffList.error?.code).toBe(-32027);
    await expect
      .poll(() => new RegExp(`callback not verified\\s+repo=${repoOf(repoId)}\\s.*reason=challenge_failed`).test(appLog()))
      .toBe(true);
    expect(appLog()).toMatch(/\[info\]\s+subscribe denied\s+repo=other\/repo\s/);
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('a callback URL that is not a public https URL on the allowlist is refused, never quoted, and writes nothing', async ({ faucet }) => {
    const marker = newMarker();
    const repoId = newRepoId();
    const userinfo = `u${randomBytes(4).toString('hex')}:p${randomBytes(4).toString('hex')}`;
    const path = `/hook/${marker}`;
    const cases: [string, string, number, string][] = [
      ['an http URL', `http://hooks.verify.test${path}`, -32602, 'scheme must be https'],
      ['a private IPv4 address', `https://10.1.2.3${path}`, -32602, 'host must be a public address'],
      ['a link-local address', `https://169.254.169.254${path}`, -32602, 'host must be a public address'],
      ['a unique-local IPv6 address', `https://[fd00::1]${path}`, -32602, 'host must be a public address'],
      ['localhost', `https://localhost${path}`, -32602, 'loopback'],
      ['a loopback address', `https://127.0.0.1${path}`, -32602, 'loopback'],
      ['loopback written as one decimal number', `https://2130706433${path}`, -32602, 'host must be a public name or address'],
      ['a single-label host', `https://intranet${path}`, -32602, 'host must be a public name or address'],
      ['user info', `https://${userinfo}@hooks.verify.test${path}`, -32602, 'user info'],
      ['a fragment', `https://hooks.verify.test${path}#section`, -32602, 'fragment'],
      ['a host that is not on the allowlist', `https://evil.verify.test${path}`, -32027, "origin is not on this server's allowlist"],
      ['the allowed host on another port', `https://hooks.verify.test:8443${path}`, -32027, "origin is not on this server's allowlist"],
      ['a public IP address that is not on the allowlist', `https://8.8.8.8${path}`, -32027, "origin is not on this server's allowlist"],
    ];

    for (const [label, url, code, message] of cases) {
      const reply = await subscribe(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url }) }));

      expect(reply.result, label).toBeNull();
      expect(reply.error?.code, label).toBe(code);
      expect(reply.error?.message, label).toContain(message);
      if (code === -32027) expect(reply.error?.data, label).toEqual({ reason: 'challenge_failed' });
      expect(occurrences(reply.body, marker), `${label} is not quoted`).toBe(0);
      expect(occurrences(reply.body, userinfo), `${label} user info is not quoted`).toBe(0);
    }
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('a callback URL with an upper-case host and the default port is stored in canonical form and names the same subscription', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const plain = callbackUrl(marker);

    const mixed = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url: plain.replace('hooks.verify.test', 'HOOKS.Verify.Test:443') }) }));
    const again = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url: plain }) }));
    const rows = await faucet.subscriptions(marker);

    expect(again.id).toBe(mixed.id);
    expect(rows.map((r) => r.callback_url)).toEqual([plain]);
  });

  test('a secret that is not whsec_ and the padded base64 of 24 to 64 bytes is refused, never echoed, and writes nothing', async ({ faucet }) => {
    const marker = newMarker();
    const repoId = newRepoId();
    const std = randomBytes(32).toString('base64');
    const cases: [string, string][] = [
      ['no prefix', std],
      ['a whsec- prefix', `whsec-${std}`],
      ['an empty value', ''],
      ['only the prefix', 'whsec_'],
      ['23 bytes', newSecret(23)],
      ['65 bytes', newSecret(65)],
      ['characters outside base64', `whsec_${'!'.repeat(44)}`],
      ['the URL-safe alphabet', `whsec_${Buffer.alloc(33, 0xfb).toString('base64url')}`],
      ['no padding', newSecret(32).replace(/=+$/, '')],
      ['a space inside', `whsec_${std.slice(0, 10)} ${std.slice(10)}`],
    ];

    for (const [label, secret] of cases) {
      const reply = await subscribe(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { secret }) }));

      expect(reply.result, label).toBeNull();
      expect(reply.status, label).toBe(400);
      expect(reply.error?.code, label).toBe(-32602);
      expect(reply.error?.message, label).toContain('delivery.secret');
      if (secret.length > 8) expect(occurrences(reply.body, secret), `${label} is not echoed`).toBe(0);
    }
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('secrets of 24 and 64 bytes are accepted and open to the secret that was sent', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const shortest = newSecret(24);
    const longest = newSecret(64);

    await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker, '/short'), secret: shortest }) }));
    await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker, '/long'), secret: longest }) }));
    const rows = await faucet.subscriptions(marker);

    expect(rows.map((r) => faucet.openSecret(r))).toEqual([shortest, longest]);
  });

  test('the secret is stored sealed: no column holds it, and an independent AES-256-GCM read with the key and the key_hash returns it', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const secret = newSecret();
    const first = subscribeParams(repoId, marker, { delivery: delivery(marker, { secret }) });
    const second = subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker, '/second'), secret }) });

    const reply = await subscribe(faucet, first);
    await subscribed(faucet, second);
    const [row, other] = await faucet.subscriptions(marker);
    const sealed = Buffer.from(row?.secret_enc ?? '', 'hex');
    const otherSealed = Buffer.from(other?.secret_enc ?? '', 'hex');

    expect(occurrences(reply.body, secret)).toBe(0);
    expect(await faucet.secretCopies(secret)).toBe(0);
    expect(sealed[0], 'seal version').toBe(1);
    expect(sealed.length, 'version, nonce, ciphertext of the secret, tag').toBe(1 + 12 + Buffer.byteLength(secret) + 16);
    expect(faucet.openSecret(row!)).toBe(secret);
    expect(fails(() => faucet.openSecret(row!, randomBytes(32))), 'a wrong key fails').toBe(true);
    expect(fails(() => faucet.openSecret({ ...row!, key_hash: other!.key_hash })), 'the key_hash of another row fails').toBe(true);
    expect(sealed.subarray(1, 13).equals(otherSealed.subarray(1, 13)), 'two rows that hold the same secret have different nonces').toBe(false);
    expect(faucet.openSecret(other!)).toBe(secret);
  });

  test('subscribing again with the same key refreshes the row: same id, new secret and expiry, same created_at and cursor', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const url = callbackUrl(marker);
    const oldSecret = newSecret();
    const newerSecret = newSecret();

    const first = await subscribed(faucet, subscribeParams(repoId, marker, { ttlMs: 60_000, delivery: delivery(marker, { url, secret: oldSecret }) }));
    const [before] = await faucet.subscriptions(marker);
    await deliverRun(faucet, repoId);
    const again = await subscribed(faucet, subscribeParams(repoId, marker, { ttlMs: 300_000, delivery: delivery(marker, { url, secret: newerSecret }) }));
    const after = await faucet.subscriptions(marker);
    const [row] = after;
    const third = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url, secret: newerSecret }) }));
    const [latest] = await faucet.subscriptions(marker);

    expect(Number(await faucet.sql(`select max(id) from faucet_events where repo_id = ${repoId}`))).toBeGreaterThan(before!.cursor);
    expect(after).toHaveLength(1);
    expect(again.id).toBe(first.id);
    expect(third.id).toBe(first.id);
    expect(again.cursor).toBe(first.cursor);
    expect(row?.cursor).toBe(before?.cursor);
    expect(row?.created_at).toBe(before?.created_at);
    expect(row?.updated_at).toBeGreaterThan(before!.updated_at);
    expect(row?.expires_at).toBeGreaterThan(before!.expires_at);
    expect(faucet.openSecret(row!)).toBe(newerSecret);
    expect(await faucet.secretCopies(oldSecret)).toBe(0);
    expect(latest?.secret_enc, 'the same secret sealed again in the same row is the same bytes, so the renewal is no rotation').toBe(row?.secret_enc);
    expect(faucet.openSecret(latest!)).toBe(newerSecret);
  });

  test('arguments written in another key order name the same subscription', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const url = callbackUrl(marker);

    const one = await subscribed(faucet, subscribeParams(repoId, marker, { arguments: { repo: repoOf(repoId), pr: 3 }, delivery: delivery(marker, { url }) }));
    const two = await subscribed(faucet, subscribeParams(repoId, marker, { arguments: { pr: 3, repo: repoOf(repoId) }, delivery: delivery(marker, { url }) }));

    expect(two.id).toBe(one.id);
    expect(await faucet.subscriptions(marker)).toHaveLength(1);
  });

  test('a different URL, query, event or arguments is another subscription with its own id and row', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const sha = randomBytes(20).toString('hex');
    const repo = repoOf(repoId);
    const url = callbackUrl(marker);
    const variants: [string, Params][] = [
      ['the base', {}],
      ['another path', { delivery: delivery(marker, { url: callbackUrl(marker, '/other') }) }],
      ['another query', { delivery: delivery(marker, { url: `${url}&x=1` }) }],
      ['another event', { name: 'pr.closed' }],
      ['another pr', { arguments: { repo, pr: 5 } }],
      ['another sha', { arguments: { repo, sha } }],
    ];

    const ids: string[] = [];
    for (const [label, over] of variants) {
      ids.push((await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url }), ...over }))).id);
      expect(new Set(ids).size, `${label} adds an id`).toBe(ids.length);
    }
    const rows = await faucet.subscriptions(marker);

    expect(rows).toHaveLength(variants.length);
    expect(new Set(rows.map((r) => r.key_hash)).size).toBe(variants.length);
  });

  test('a second principal that subscribes the same key gets its own row, id and secret', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const url = callbackUrl(marker);
    const mine = newSecret();
    const theirs = newSecret();

    const first = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url, secret: mine }) }));
    const second = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url, secret: theirs }) }), { as: 'other' });
    const afterSecond = await faucet.subscriptions(marker);
    await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url, secret: newSecret() }) }));
    const afterRefresh = await faucet.subscriptions(marker);

    expect(second.id).not.toBe(first.id);
    expect(afterSecond.map((r) => r.principal).sort()).toEqual(['verify-other', 'verify-user']);
    expect(afterSecond.map((r) => r.repo_id)).toEqual([repoId, repoId]);
    const byPrincipal = (rows: typeof afterSecond, login: string) => rows.find((r) => r.principal === login)!;
    expect(faucet.openSecret(byPrincipal(afterSecond, 'verify-user'))).toBe(mine);
    expect(faucet.openSecret(byPrincipal(afterSecond, 'verify-other'))).toBe(theirs);
    expect(byPrincipal(afterRefresh, 'verify-other')).toEqual(byPrincipal(afterSecond, 'verify-other'));
    await expect.poll(() => new RegExp(`\\[info\\]\\s+subscribed\\s.*login=verify-other\\s+subscription=${second.id}\\b`).test(appLog())).toBe(true);
  });

  test('the expiry is the requested ttlMs, capped at 10 minutes and raised to 30 seconds, in the reply and in the row', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const cases: [string, Params, number][] = [
      ['no ttlMs', {}, MAX_TTL_MS],
      ['ttlMs null', { ttlMs: null }, MAX_TTL_MS],
      ['one day', { ttlMs: 86_400_000 }, MAX_TTL_MS],
      ['the largest safe integer', { ttlMs: Number.MAX_SAFE_INTEGER }, MAX_TTL_MS],
      ['one millisecond over the cap', { ttlMs: MAX_TTL_MS + 1 }, MAX_TTL_MS],
      ['exactly the cap', { ttlMs: MAX_TTL_MS }, MAX_TTL_MS],
      ['a value between', { ttlMs: 90_000 }, 90_000],
      ['one millisecond over the floor', { ttlMs: MIN_TTL_MS + 1 }, MIN_TTL_MS + 1],
      ['exactly the floor', { ttlMs: MIN_TTL_MS }, MIN_TTL_MS],
      ['one millisecond under the floor', { ttlMs: MIN_TTL_MS - 1 }, MIN_TTL_MS],
      ['one millisecond', { ttlMs: 1 }, MIN_TTL_MS],
      ['zero', { ttlMs: 0 }, MIN_TTL_MS],
      ['a negative value', { ttlMs: -5_000 }, MIN_TTL_MS],
    ];

    for (const [index, [label, over, grantMs]] of cases.entries()) {
      const url = callbackUrl(marker, `/ttl-${index}`);
      const sentAt = Date.now();
      const result = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url }), ...over }));
      const repliedAt = Date.now();
      const refreshBefore = Date.parse(result.refreshBefore);
      const [row] = (await faucet.subscriptions(marker)).filter((r) => r.callback_url === url);

      expect(result.refreshBefore, label).toMatch(UTC_SECONDS);
      expect(refreshBefore, `${label} is not before the grant`).toBeGreaterThanOrEqual(floorSecond(sentAt + grantMs));
      expect(refreshBefore, `${label} is not after the grant`).toBeLessThanOrEqual(floorSecond(repliedAt + grantMs));
      expect(row?.expires_at, `${label} is stored as replied`).toBe(refreshBefore);
    }
  });

  test('a ttlMs so negative that its nanoseconds overflow gets the floor, never a longer grant', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const url = callbackUrl(marker, '/ttl-overflow');

    for (const ttlMs of [-9_223_372_036_854, -9_223_372_036_855, -9_300_000_000_000, -18_000_000_000_000, -Number.MAX_SAFE_INTEGER]) {
      const sentAt = Date.now();
      const result = await subscribed(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url }), ttlMs }));
      const repliedAt = Date.now();
      const refreshBefore = Date.parse(result.refreshBefore);

      expect(refreshBefore, `${ttlMs} is not before the floor`).toBeGreaterThanOrEqual(floorSecond(sentAt + MIN_TTL_MS));
      expect(refreshBefore, `${ttlMs} is not after the floor`).toBeLessThanOrEqual(floorSecond(repliedAt + MIN_TTL_MS));
    }

    const rows = await faucet.subscriptions(marker);
    expect(rows).toHaveLength(1);
    expect(rows[0]?.expires_at, 'the stored expiry is the floor too').toBeLessThanOrEqual(Date.now() + MIN_TTL_MS);
  });

  test('a parameter that is not valid is refused with the error that names it and writes nothing', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const repo = repoOf(repoId);
    const url = callbackUrl(marker);
    const cases: [string, Params, number, string, unknown?][] = [
      ['an unknown event name', { name: 'ci.nope' }, -32023, 'unknown event type', { kind: 'event' }],
      ['a delivery mode other than webhook', { delivery: delivery(marker, { mode: 'push' }) }, -32026, 'webhook delivery mode', { feature: 'deliveryMode', value: 'push' }],
      ['no delivery', { delivery: undefined }, -32602, 'delivery'],
      ['a null delivery', { delivery: null }, -32602, 'delivery'],
      ['a delivery that is a string', { delivery: 'webhook' }, -32602, 'delivery'],
      ['no delivery url', { delivery: { secret: newSecret() } }, -32602, 'delivery.url is required'],
      ['no delivery secret', { delivery: { url } }, -32602, 'delivery.secret is required'],
      ['a delivery url that is a number', { delivery: { url: 5, secret: newSecret() } }, -32602, 'delivery'],
      ['a delivery secret that is a number', { delivery: { url, secret: 5 } }, -32602, 'delivery'],
      ['no arguments', { arguments: undefined }, -32602, 'repo'],
      ['no repo', { arguments: {} }, -32602, 'repo'],
      ['an unknown argument', { arguments: { repo, extra: 1 } }, -32602, 'extra'],
      ['a repo that is not owner/name', { arguments: { repo: 'not a repo' } }, -32602, 'owner/name'],
      ['a short sha', { arguments: { repo, sha: 'abc' } }, -32602, 'sha'],
      ['pr 0', { arguments: { repo, pr: 0 } }, -32602, 'pr'],
      ['a cursor the server never returned', { cursor: 'abc' }, -32602, 'cursor'],
      ['maxAgeMs 0', { cursor: 'v1.0', maxAgeMs: 0 }, -32602, 'maxAgeMs'],
      ['ttlMs as text', { ttlMs: 'ten minutes' }, -32602, 'ttlMs'],
      ['ttlMs as a fraction', { ttlMs: 1.5 }, -32602, 'ttlMs'],
    ];

    for (const [label, over, code, message, data] of cases) {
      const reply = await subscribe(faucet, subscribeParams(repoId, marker, { delivery: delivery(marker, { url }), ...over }));

      expect(reply.result, label).toBeNull();
      expect(reply.error?.code, label).toBe(code);
      expect(reply.error?.message, label).toContain(message);
      if (data !== undefined) expect(reply.error?.data, label).toEqual(data);
    }
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('a request without a valid token is 401, and a missing or different Mcp-Name header is -32020, and none writes a row', async ({ faucet }) => {
    const marker = newMarker();
    const params = subscribeParams(newRepoId(), marker);

    const none = await subscribe(faucet, params, { token: null });
    const wrong = await subscribe(faucet, params, { token: `ghp_${'0'.repeat(40)}` });
    const unnamed = await subscribe(faucet, params, { name: null });
    const mismatched = await subscribe(faucet, params, { name: 'pr.closed' });

    for (const reply of [none, wrong]) {
      expect(reply.status).toBe(401);
      expect(reply.body).toBe('unauthorized\n');
      expect(reply.headers.get('www-authenticate')).toBe('Bearer');
    }
    for (const reply of [unnamed, mismatched]) {
      expect(reply.status).toBe(400);
      expect(reply.error?.code).toBe(-32020);
    }
    expect(await faucet.subscriptions(marker)).toEqual([]);
  });

  test('a name in another letter case never changes the event that the Mcp-Name header names, for subscribe and unsubscribe', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const params = subscribeParams(repoId, marker);
    const { name: _name, ...withoutName } = params;
    const eventsOf = async () => (await faucet.subscriptions(marker)).map((r) => r.event_type);

    const shadowed = await subscribe(faucet, { ...params, NAME: 'pr.closed', Name: 'pr.closed' }, { name: EVENT });
    const upperCaseOnly = await subscribe(faucet, { ...withoutName, NAME: 'pr.closed' }, { name: null });
    const titleCaseOnly = await subscribe(faucet, { ...withoutName, Name: 'pr.closed' }, { name: 'pr.closed' });

    expect(shadowed.error, shadowed.body).toBeNull();
    expect(await eventsOf(), 'only the event of the exact name member is stored').toEqual([EVENT]);
    for (const reply of [upperCaseOnly, titleCaseOnly]) {
      expect(reply.status).toBe(400);
      expect(reply.error?.code).toBe(-32602);
      expect(reply.error?.message).toContain('name is required');
    }

    await subscribed(faucet, subscribeParams(repoId, marker, { name: 'pr.closed' }));
    expect((await eventsOf()).sort()).toEqual(['ci.run_completed', 'pr.closed']);
    const { name: _unsubscribed, ...unsubscribeWithoutName } = unsubscribeParams(params);
    const noExactName = await faucet.mcp('events/unsubscribe', { ...unsubscribeWithoutName, Name: 'pr.closed' }, { name: 'pr.closed' });
    const shadowedUnsubscribe = await faucet.mcp('events/unsubscribe', { ...unsubscribeParams(params), NAME: 'pr.closed' }, { name: EVENT });

    expect(noExactName.error?.code).toBe(-32602);
    expect(shadowedUnsubscribe.error, shadowedUnsubscribe.body).toBeNull();
    expect(await eventsOf(), 'only the subscription of the exact name member is removed').toEqual(['pr.closed']);
  });

  test('the app log holds no secret, no key and no callback URL path or query, for accepted and refused calls', async ({ faucet }) => {
    const repoId = newRepoId();
    const marker = newMarker();
    const secret = newSecret();
    const badSecret = newSecret(23);
    const refused = [
      subscribeParams(repoId, marker, { delivery: delivery(marker, { url: `https://evil.verify.test/hook/${marker}?token=q${marker}`, secret }) }),
      subscribeParams(repoId, marker, { arguments: { repo: 'other/repo' }, delivery: delivery(marker, { secret }) }),
      subscribeParams(repoId, marker, { delivery: delivery(marker, { url: `http://hooks.verify.test/hook/${marker}`, secret }) }),
      subscribeParams(repoId, marker, { delivery: delivery(marker, { secret: badSecret }) }),
    ];
    const accepted = subscribeParams(repoId, marker, { delivery: delivery(marker, { secret }) });

    const replies = [];
    for (const params of refused) replies.push(await subscribe(faucet, params));
    const result = await subscribed(faucet, accepted);
    replies.push(await subscribe(faucet, accepted));
    const gone = await unsubscribe(faucet, accepted);
    await expect.poll(() => new RegExp(`\\[info\\]\\s+unsubscribed\\s.*subscription=${result.id}\\b`).test(appLog())).toBe(true);
    const log = appLog();

    expect(gone.error).toBeNull();
    expect(log).toMatch(subscribedLine(repoId, result.id));
    for (const reply of replies) {
      expect(faucet.leaksSecret(reply.body), reply.body).toBe(false);
      expect(occurrences(reply.body, secret), reply.body).toBe(0);
    }
    expect(faucet.leaksSecret(log)).toBe(false);
    const kept: [string, string][] = [['the secret', secret], ['the rejected secret', badSecret], ['the path and query', marker]];
    for (const [label, value] of kept) expect(occurrences(log, value), `${label} in the log`).toBe(0);
  });

  test('a principal holds at most 100 live subscriptions: the next is -32025, a refresh, another principal and a freed slot still work', async ({ faucet }) => {
    const key = randomBytes(32).toString('base64');
    await faucet.withFaucet({ FAUCET_WEBHOOK_KEY: key, FAUCET_WEBHOOK_ALLOWED_ORIGINS: CALLBACK_ORIGIN }, async (started) => {
      const repoId = newRepoId();
      const marker = newMarker();
      const at = (suffix: string): Params => subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker, suffix) }) });
      const held = Array.from({ length: SUBSCRIPTION_LIMIT }, (_, i) => at(`/${i}`));
      for (const params of held) await subscribed(started, params);
      const over = at('/over');

      const refused = await subscribe(started, over);
      const full = await started.subscriptions(marker);
      const refreshed = await subscribe(started, held[0]!);
      const otherPrincipal = await subscribe(started, over, { as: 'other' });
      await unsubscribe(started, held[1]!);
      const afterFree = await subscribe(started, over);
      const final = await started.subscriptions(marker);

      expect(refused.result).toBeNull();
      expect(refused.error).toMatchObject({ code: -32025, data: { limit: 'subscriptions', max: SUBSCRIPTION_LIMIT } });
      expect(full).toHaveLength(SUBSCRIPTION_LIMIT);
      expect(full.some((r) => r.callback_url.includes('/over'))).toBe(false);
      expect(refreshed.error, 'a refresh at the limit').toBeNull();
      expect(otherPrincipal.error, 'another principal at the limit').toBeNull();
      expect(afterFree.error, 'after a slot was freed').toBeNull();
      expect(final).toHaveLength(SUBSCRIPTION_LIMIT + 2 - 1);
    });
  });
});

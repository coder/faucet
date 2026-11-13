import { randomBytes } from 'node:crypto';
import { describe, expect, freePort, newRepoId, test, type Faucet, type Receiver, type ReceivedRequest } from './support';
import { EVENT, newMarker, newSecret, occurrences, repoOf, subscribe, subscribed, type Params } from './subscriptions';

const KEY = randomBytes(32).toString('base64');
const HANDSHAKE_ENV = { FAUCET_WEBHOOK_KEY: KEY, FAUCET_WEBHOOK_CALLBACK_VERIFICATION: 'handshake' };
const NONCE = /^[A-Za-z0-9_-]{43}$/;

interface Endpoint {
  id: string;
  secret: string;
  url: string;
}

async function endpoint(receiver: Receiver, behavior: string, opts: { port?: number; registered?: string } = {}): Promise<Endpoint> {
  const id = newMarker();
  const secret = newSecret();
  await receiver.register(id, opts.registered ?? secret);
  return { id, secret, url: receiver.callback(behavior, id, opts) };
}

function paramsFor(repoId: number, at: Endpoint): Params {
  return { name: EVENT, arguments: { repo: repoOf(repoId) }, delivery: { url: at.url, secret: at.secret } };
}

function withHandshakeFaucet<T>(faucet: Faucet, fn: (started: Faucet, output: () => string) => Promise<T>, e2eBuild = true): Promise<T> {
  return faucet.withFaucet(HANDSHAKE_ENV, fn, { e2eBuild });
}

function challengeOf(request: ReceivedRequest): string {
  return (JSON.parse(request.body) as { challenge: string }).challenge;
}

// requireRefusal checks what a refused handshake owes the caller: the SEP category, no row, and no secret, URL path or nonce in
// the reply or in the log of the server.
async function requireRefusal(
  started: Faucet,
  output: () => string,
  receiver: Receiver,
  at: Endpoint,
  reply: Awaited<ReturnType<typeof subscribe>>,
  reason: string,
): Promise<void> {
  expect(reply.error?.code, reply.body).toBe(-32027);
  expect(reply.error?.data, reply.body).toEqual({ reason });
  expect(await started.subscriptions(at.id), `${reason}: a refused handshake writes no row`).toEqual([]);
  expect(output()).toMatch(new RegExp(`\\[info\\]\\s+callback not verified\\s.*reason=${reason}\\b`));

  const needles = [at.secret, at.secret.slice('whsec_'.length), at.id, ...(await receiver.requests(at.id)).map(challengeOf).filter((n) => n !== '')];
  for (const needle of needles) {
    expect(occurrences(reply.body, needle), `${reason}: the reply holds ${needle.slice(0, 6)}...`).toBe(0);
    expect(occurrences(output(), needle), `${reason}: the log holds ${needle.slice(0, 6)}...`).toBe(0);
  }
  expect(started.leaksSecret(reply.body + output())).toBe(false);
}

describe('callback handshake', () => {
  test('a subscriber that checks the signature and echoes the nonce is subscribed after one signed request', async ({ faucet, receiver }) => {
    await withHandshakeFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const first = await endpoint(receiver, 'echo');
      const result = await subscribed(started, paramsFor(repoId, first));

      const requests = await receiver.requests(first.id);
      expect(requests).toHaveLength(1);
      const [request] = requests as [ReceivedRequest];
      expect(request.method).toBe('POST');
      expect(request.signature, 'the receiver verified the HMAC with Node crypto, with the subscriber secret').toBe('valid');
      expect(request.headers['content-type']).toBe('application/json');
      expect(request.headers['webhook-id']).toMatch(/^msg_verification_[A-Za-z0-9_-]{22}$/);
      expect(Math.abs(Number(request.headers['webhook-timestamp']) - Date.now() / 1000)).toBeLessThan(60);
      expect(request.headers['webhook-signature']).toMatch(/^v1,[A-Za-z0-9+/]{43}=$/);
      expect(request.headers['x-mcp-subscription-id'], 'the receiver can route by the id that subscribe returns').toBe(result.id);
      expect(request.headers.authorization).toBeUndefined();
      expect(request.headers.cookie).toBeUndefined();
      const body = JSON.parse(request.body) as Record<string, string>;
      expect(Object.keys(body).sort()).toEqual(['challenge', 'type']);
      expect(body.type).toBe('verification');
      expect(body.challenge).toMatch(NONCE);
      expect(started.leaksSecret(JSON.stringify(request))).toBe(false);

      const rows = await started.subscriptions(first.id);
      expect(rows).toHaveLength(1);
      expect(started.openSecret(rows[0]!)).toBe(first.secret);

      await subscribed(started, paramsFor(repoId, first));
      expect(await receiver.requests(first.id), 'a refresh of a verified URL sends nothing').toHaveLength(1);

      const second = await endpoint(receiver, 'echo');
      await subscribed(started, paramsFor(repoId, second));
      const [again] = (await receiver.requests(second.id)) as [ReceivedRequest];
      expect(challengeOf(again), 'a nonce is used once').not.toBe(body.challenge);
      expect(again.headers['webhook-id']).not.toBe(request.headers['webhook-id']);

      for (const needle of [first.secret, first.secret.slice('whsec_'.length), first.id, body.challenge!]) {
        expect(occurrences(output(), needle), `the log holds ${needle.slice(0, 6)}...`).toBe(0);
      }
      expect(started.leaksSecret(output())).toBe(false);
    });
  });

  test('an answer that is wrong, reflected, an error, or signed under another secret is refused with its category and writes no row', async ({ faucet, receiver }) => {
    await withHandshakeFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const cases: [string, string, Endpoint][] = [
        ['challenge_failed', 'a wrong nonce', await endpoint(receiver, 'wrong')],
        ['challenge_failed', 'the request body reflected', await endpoint(receiver, 'reflect')],
        ['http_5xx', 'a 500', await endpoint(receiver, 'error500')],
        ['http_4xx', 'a 404', await endpoint(receiver, 'notfound')],
        ['http_4xx', 'a receiver that holds another secret and answers 401', await endpoint(receiver, 'echo', { registered: newSecret() })],
      ];
      for (const [reason, name, at] of cases) {
        const reply = await subscribe(started, paramsFor(repoId, at));
        expect(reply.error, name).not.toBeNull();
        await requireRefusal(started, output, receiver, at, reply, reason);
        expect(await receiver.requests(at.id), `${name}: one request`).toHaveLength(1);
      }
    });
  });

  test('a redirect, an oversize answer, a closed port and a silent endpoint are refused, and nothing follows the redirect', async ({ faucet, receiver }) => {
    await withHandshakeFaucet(faucet, async (started, output) => {
      const repoId = newRepoId();
      const closedPort = await freePort();
      const cases: [string, string, Endpoint][] = [
        ['challenge_failed', 'a redirect', await endpoint(receiver, 'redirect')],
        ['challenge_failed', 'the right answer padded past 1 KiB', await endpoint(receiver, 'large')],
        ['connection_refused', 'a port where nothing listens', await endpoint(receiver, 'echo', { port: closedPort })],
        ['timeout', 'an endpoint that never answers', await endpoint(receiver, 'slow')],
      ];
      for (const [reason, name, at] of cases) {
        const reply = await subscribe(started, paramsFor(repoId, at));
        expect(reply.error, name).not.toBeNull();
        await requireRefusal(started, output, receiver, at, reply, reason);
      }
      const [redirected] = cases;
      const seen = await receiver.requests(redirected![2].id);
      expect(seen.map((r) => r.behavior), 'the redirect target was never requested').toEqual(['redirect']);
    });
  });

  test('the sixth failing handshake to one host answers ResourceExhausted and sends nothing', async ({ faucet, receiver }) => {
    await withHandshakeFaucet(faucet, async (started) => {
      const repoId = newRepoId();
      for (let attempt = 1; attempt <= 5; attempt++) {
        const at = await endpoint(receiver, 'notfound');
        const reply = await subscribe(started, paramsFor(repoId, at));
        expect(reply.error?.code, `attempt ${attempt}: ${reply.body}`).toBe(-32027);
      }
      const limited = await endpoint(receiver, 'echo');
      const reply = await subscribe(started, paramsFor(repoId, limited));
      expect(reply.error?.code, reply.body).toBe(-32025);
      expect(reply.error?.data).toEqual({ limit: 'callbackVerifications' });
      expect(await receiver.requests(limited.id), 'a limited handshake sends nothing').toEqual([]);
      expect(await started.subscriptions(limited.id), 'and writes no row').toEqual([]);
    });
  });

  test('a production build refuses http and loopback callback URLs, and the receiver never hears of them', async ({ faucet, receiver }) => {
    await withHandshakeFaucet(
      faucet,
      async (started) => {
        const repoId = newRepoId();
        const at = await endpoint(receiver, 'echo');
        const secure = at.url.replace('http://', 'https://');
        for (const [name, url] of [['http', at.url], ['https to a loopback address', secure]] as const) {
          const reply = await subscribe(started, paramsFor(repoId, { ...at, url }));
          expect(reply.error?.code, `${name}: ${reply.body}`).toBe(-32602);
          expect(reply.error?.message, name).toMatch(/^delivery\.url /);
        }
        expect(await receiver.requests(at.id)).toEqual([]);
        expect(await started.subscriptions(at.id)).toEqual([]);
      },
      false,
    );
  });
});

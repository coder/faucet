import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { describe, expect, newRepoId, test, type Faucet } from './support';
import { callbackUrl, delivery, newMarker, newSecret, subscribe, subscribeParams, subscribed } from './subscriptions';

const execFileAsync = promisify(execFile);

const START_TIMEOUT_MS = 20_000;
const FAKE_DB_PASSWORD = 'fake-db-password-7c1f4e';
const FAKE_SECRET = 'fake-webhook-secret-0123456789abcdef';
const SHORT_SECRET = 'tiny-7c1f4e';
const FAKE_CRON_SECRET = 'fake-cron-secret-0123456789abcdef';
const ORIGIN_TOKEN = 'h7c1f4e';
const USERINFO_TOKEN = 'ppass7c1f4e';
const KEY_NOT_BASE64 = 'not-base64-key-7c1f4e!!';
const KEY_32 = Buffer.alloc(32, 0x5a).toString('base64');
const KEY_URL_SAFE = Buffer.alloc(32, 0xfb).toString('base64url');
const KEY_31_BYTES = Buffer.alloc(31, 0x5a).toString('base64');
const KEY_33_BYTES = Buffer.alloc(33, 0x5a).toString('base64');
const KEY_ZERO = Buffer.alloc(32).toString('base64');
const VERIFY_UNKNOWN = 'dns-7c1f4e';
const ORIGIN = `https://${ORIGIN_TOKEN}.verify.test`;
const WELL_FORMED_URL = `postgres://app:${FAKE_DB_PASSWORD}@127.0.0.1:1/faucet?sslmode=disable`;
const MALFORMED_URLS: Record<string, string> = {
  'a port that is not a number': `postgres://app:${FAKE_DB_PASSWORD}@db.example.com:notaport/faucet`,
  'a bad escape after the password': `postgres://app:${FAKE_DB_PASSWORD}%zz@db.example.com/faucet`,
  'a space in the host': `postgres://app:${FAKE_DB_PASSWORD}@db exa.example.com/faucet`,
  'a control character': `postgresql://app:${FAKE_DB_PASSWORD}@db.example.com/faucet\x7f`,
  'an unclosed IPv6 bracket': `postgresql://app:${FAKE_DB_PASSWORD}@[::1/faucet`,
  'a password where a keyword belongs': `${FAKE_DB_PASSWORD} host=db.example.com`,
};

interface Exit {
  code: number | null;
  output: string;
}

async function startFaucet(faucet: Faucet, env: Record<string, string>): Promise<Exit> {
  const port = new URL(faucet.baseUrl).port;
  // A server that wrongly started would find the running faucet's port taken.
  try {
    const { stdout, stderr } = await execFileAsync(faucet.binary, [], { env: { PORT: port, ...env }, timeout: START_TIMEOUT_MS });
    return { code: 0, output: stdout + stderr };
  } catch (err) {
    const failure = err as { code?: unknown; stdout?: string; stderr?: string };
    return {
      code: typeof failure.code === 'number' ? failure.code : null,
      output: `${failure.stdout ?? ''}${failure.stderr ?? ''}`,
    };
  }
}

function occurrences(text: string, needle: string): number {
  return text.split(needle).length - 1;
}

describe('startup rejects a bad DATABASE_URL without printing its password', () => {
  for (const [name, url] of Object.entries(MALFORMED_URLS)) {
    test(`${name} exits 1 and names only the variable`, async ({ faucet }) => {
      const result = await startFaucet(faucet, { DATABASE_URL: url, GITHUB_WEBHOOK_SECRET: FAKE_SECRET });

      expect(result.code).toBe(1);
      expect(result.output).toContain('DATABASE_URL: not a valid Postgres connection string');
      expect(occurrences(result.output, FAKE_DB_PASSWORD)).toBe(0);
      expect(occurrences(result.output, FAKE_SECRET)).toBe(0);
    });
  }
});

describe('startup configuration', () => {
  const valid = { DATABASE_URL: WELL_FORMED_URL, GITHUB_WEBHOOK_SECRET: FAKE_SECRET };
  const webhook = { FAUCET_WEBHOOK_KEY: KEY_32, FAUCET_WEBHOOK_ALLOWED_ORIGINS: ORIGIN };
  const KEY_RULE = 'FAUCET_WEBHOOK_KEY must be the standard base64 of exactly 32 bytes';
  const ORIGIN_RULE = 'FAUCET_WEBHOOK_ALLOWED_ORIGINS must be comma-separated https origins written scheme://host[:port]: ';
  const webhookCases: [string, Record<string, string>, string][] = [
    ['FAUCET_WEBHOOK_KEY without FAUCET_WEBHOOK_ALLOWED_ORIGINS', { FAUCET_WEBHOOK_KEY: KEY_32 }, 'webhook delivery needs FAUCET_WEBHOOK_ALLOWED_ORIGINS\n'],
    ['FAUCET_WEBHOOK_ALLOWED_ORIGINS without FAUCET_WEBHOOK_KEY', { FAUCET_WEBHOOK_ALLOWED_ORIGINS: ORIGIN }, 'webhook delivery needs FAUCET_WEBHOOK_KEY\n'],
    ['a FAUCET_WEBHOOK_KEY that is not base64', { ...webhook, FAUCET_WEBHOOK_KEY: KEY_NOT_BASE64 }, `${KEY_RULE}\n`],
    ['a FAUCET_WEBHOOK_KEY in the URL-safe alphabet', { ...webhook, FAUCET_WEBHOOK_KEY: KEY_URL_SAFE }, `${KEY_RULE}\n`],
    ['a FAUCET_WEBHOOK_KEY of 31 bytes', { ...webhook, FAUCET_WEBHOOK_KEY: KEY_31_BYTES }, `${KEY_RULE} that are not all zero\n`],
    ['a FAUCET_WEBHOOK_KEY of 33 bytes', { ...webhook, FAUCET_WEBHOOK_KEY: KEY_33_BYTES }, `${KEY_RULE} that are not all zero\n`],
    ['an all-zero FAUCET_WEBHOOK_KEY', { ...webhook, FAUCET_WEBHOOK_KEY: KEY_ZERO }, `${KEY_RULE} that are not all zero\n`],
    ['an http origin', { ...webhook, FAUCET_WEBHOOK_ALLOWED_ORIGINS: `http://${ORIGIN_TOKEN}.verify.test` }, `${ORIGIN_RULE}allowed origin 1: scheme must be https\n`],
    ['an origin with a path', { ...webhook, FAUCET_WEBHOOK_ALLOWED_ORIGINS: `${ORIGIN}/path-${ORIGIN_TOKEN}` }, `${ORIGIN_RULE}allowed origin 1 must be scheme://host[:port] only\n`],
    ['an origin with user info', { ...webhook, FAUCET_WEBHOOK_ALLOWED_ORIGINS: `https://user:${USERINFO_TOKEN}@${ORIGIN_TOKEN}.verify.test` }, `${ORIGIN_RULE}allowed origin 1: must not contain user info\n`],
    ['a loopback origin', { ...webhook, FAUCET_WEBHOOK_ALLOWED_ORIGINS: 'https://127.0.0.1' }, `${ORIGIN_RULE}allowed origin 1: host must not be a loopback address\n`],
    ['an empty entry in the origin list', { ...webhook, FAUCET_WEBHOOK_ALLOWED_ORIGINS: `${ORIGIN},` }, `${ORIGIN_RULE}allowed origin 2: must be 1 to 2048 bytes\n`],
    ['the handshake without a FAUCET_WEBHOOK_KEY', { FAUCET_WEBHOOK_CALLBACK_VERIFICATION: 'handshake' }, 'webhook delivery needs FAUCET_WEBHOOK_KEY\n'],
    ['the handshake with FAUCET_WEBHOOK_ALLOWED_ORIGINS', { ...webhook, FAUCET_WEBHOOK_CALLBACK_VERIFICATION: 'handshake' }, 'FAUCET_WEBHOOK_ALLOWED_ORIGINS must not be set when FAUCET_WEBHOOK_CALLBACK_VERIFICATION is handshake\n'],
    ['the allowlist named without origins', { FAUCET_WEBHOOK_KEY: KEY_32, FAUCET_WEBHOOK_CALLBACK_VERIFICATION: 'allowlist' }, 'webhook delivery needs FAUCET_WEBHOOK_ALLOWED_ORIGINS\n'],
    ['a callback verification that does not exist', { ...webhook, FAUCET_WEBHOOK_CALLBACK_VERIFICATION: VERIFY_UNKNOWN }, 'FAUCET_WEBHOOK_CALLBACK_VERIFICATION must be allowlist or handshake\n'],
  ];
  const cases: { name: string; env: Record<string, string>; message: string }[] = [
    {
      name: 'a missing DATABASE_URL',
      env: { GITHUB_WEBHOOK_SECRET: FAKE_SECRET },
      message: 'missing required environment variables: DATABASE_URL\n',
    },
    {
      name: 'a missing GITHUB_WEBHOOK_SECRET',
      env: { DATABASE_URL: WELL_FORMED_URL },
      message: 'missing required environment variables: GITHUB_WEBHOOK_SECRET\n',
    },
    {
      name: 'both variables missing',
      env: {},
      message: 'missing required environment variables: DATABASE_URL, GITHUB_WEBHOOK_SECRET\n',
    },
    {
      name: 'a GITHUB_WEBHOOK_SECRET shorter than 16 bytes',
      env: { DATABASE_URL: WELL_FORMED_URL, GITHUB_WEBHOOK_SECRET: SHORT_SECRET },
      message: 'secret must be at least 16 bytes\n',
    },
    ...[
      ['http://api.github.com', 'use https'],
      ['ftp://api.github.com', 'use https'],
      ['api.github.com', 'be an absolute URL'],
      ['https://api.github.com?x=1', 'not have user info'],
      [`https://user:${FAKE_DB_PASSWORD}@api.github.com`, 'not have user info'],
    ].map(([url, reason]) => ({
      name: `FAUCET_GITHUB_API_URL=${url?.replace(FAKE_DB_PASSWORD, '***')}`,
      env: { DATABASE_URL: WELL_FORMED_URL, GITHUB_WEBHOOK_SECRET: FAKE_SECRET, FAUCET_GITHUB_API_URL: url ?? '' },
      message: `FAUCET_GITHUB_API_URL: GitHub API URL must ${reason}`,
    })),
    ...['abc', '0', '65536', '-1', '80.5'].map((port) => ({
      name: `PORT=${port}`,
      env: { DATABASE_URL: WELL_FORMED_URL, GITHUB_WEBHOOK_SECRET: FAKE_SECRET, PORT: port },
      message: 'PORT must be a number from 1 to 65535\n',
    })),
    {
      name: 'a CRON_SECRET shorter than 16 bytes',
      env: { ...valid, CRON_SECRET: SHORT_SECRET },
      message: 'CRON_SECRET must be at least 16 bytes\n',
    },
    ...['abc', '0', '-1', '3651', '1.5', '30 days'].map((days) => ({
      name: `FAUCET_RETENTION_DAYS=${days}`,
      env: { ...valid, FAUCET_RETENTION_DAYS: days },
      message: 'FAUCET_RETENTION_DAYS must be a whole number of days from 1 to 3650\n',
    })),
    ...webhookCases.map(([name, settings, message]) => ({ name, env: { ...valid, ...settings }, message })),
  ];
  const printedNever = [
    FAKE_DB_PASSWORD, FAKE_SECRET, SHORT_SECRET, FAKE_CRON_SECRET,
    KEY_32, KEY_NOT_BASE64, KEY_URL_SAFE, KEY_31_BYTES, KEY_33_BYTES, KEY_ZERO, ORIGIN_TOKEN, USERINFO_TOKEN, VERIFY_UNKNOWN,
  ];
  for (const { name, env, message } of cases) {
    test(`${name} exits 1 with a clear message and prints no secret`, async ({ faucet }) => {
      const result = await startFaucet(faucet, env);

      expect(result.code).toBe(1);
      expect(result.output).toContain(message);
      for (const secret of printedNever) {
        expect(occurrences(result.output, secret), `${secret.slice(0, 6)}... in the output`).toBe(0);
      }
    });
  }

  test('both webhook variables set start the server, an origin list is trimmed, and the key in use is the one given', async ({ faucet }) => {
    const env = { FAUCET_WEBHOOK_KEY: KEY_32, FAUCET_WEBHOOK_ALLOWED_ORIGINS: 'https://a.verify.test, https://b.verify.test:8443 ' };
    await faucet.withFaucet(env, async (started) => {
      const repoId = newRepoId();
      const marker = newMarker();
      const secret = newSecret();
      const onA = subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker).replace('hooks.verify.test', 'a.verify.test'), secret }) });
      const onB = subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker).replace('hooks.verify.test', 'b.verify.test:8443') }) });
      const onC = subscribeParams(repoId, marker, { delivery: delivery(marker, { url: callbackUrl(marker).replace('hooks.verify.test', 'c.verify.test') }) });

      await subscribed(started, onA);
      await subscribed(started, onB);
      const refused = await subscribe(started, onC);
      const rows = await started.subscriptions(marker);

      expect(refused.error?.code).toBe(-32027);
      expect(rows).toHaveLength(2);
      expect(started.openSecret(rows[0]!)).toBe(secret);
    });
  });
});

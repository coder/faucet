import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { describe, expect, test, type Faucet } from './support';

const execFileAsync = promisify(execFile);

const START_TIMEOUT_MS = 20_000;
const FAKE_DB_PASSWORD = 'fake-db-password-7c1f4e';
const FAKE_SECRET = 'fake-webhook-secret-0123456789abcdef';
const SHORT_SECRET = 'tiny-7c1f4e';
const FAKE_CRON_SECRET = 'fake-cron-secret-0123456789abcdef';
const FAKE_HOOK_TOKEN = 'ghp_fake-hook-token-7c1f4e';
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
  const hook = { FAUCET_HOOK_TOKEN: FAKE_HOOK_TOKEN, FAUCET_HOOK_ORG: 'verify', FAUCET_HOOK_ID: '7' };
  const hookCases: [string, Record<string, string>, string][] = [
    ['only FAUCET_HOOK_TOKEN', { FAUCET_HOOK_TOKEN: FAKE_HOOK_TOKEN }, 'redelivery needs FAUCET_HOOK_ID, FAUCET_HOOK_ORG or FAUCET_HOOK_REPO\n'],
    ['FAUCET_HOOK_ORG and FAUCET_HOOK_ID without a token', { FAUCET_HOOK_ORG: 'verify', FAUCET_HOOK_ID: '7' }, 'redelivery needs FAUCET_HOOK_TOKEN\n'],
    ['FAUCET_HOOK_ORG and FAUCET_HOOK_REPO together', { ...hook, FAUCET_HOOK_REPO: 'verify/repo' }, 'set only one of FAUCET_HOOK_ORG and FAUCET_HOOK_REPO\n'],
    ['FAUCET_HOOK_ID=0', { ...hook, FAUCET_HOOK_ID: '0' }, 'FAUCET_HOOK_ID must be a positive integer\n'],
    ['FAUCET_HOOK_ID=abc', { ...hook, FAUCET_HOOK_ID: 'abc' }, 'FAUCET_HOOK_ID must be a positive integer\n'],
    [
      'a FAUCET_HOOK_TOKEN with a space',
      { ...hook, FAUCET_HOOK_TOKEN: `${FAKE_HOOK_TOKEN} x` },
      'FAUCET_HOOK_*: hook token must be 1 to 1024 printable ASCII characters without spaces',
    ],
    [
      'a FAUCET_HOOK_ORG that is not an organization name',
      { ...hook, FAUCET_HOOK_ORG: 'not/an-org' },
      'FAUCET_HOOK_*: hook org is not a valid organization name',
    ],
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
    ...hookCases.map(([name, settings, message]) => ({ name, env: { ...valid, ...settings }, message })),
  ];
  for (const { name, env, message } of cases) {
    test(`${name} exits 1 with a clear message and prints no secret`, async ({ faucet }) => {
      const result = await startFaucet(faucet, env);

      expect(result.code).toBe(1);
      expect(result.output).toContain(message);
      for (const secret of [FAKE_DB_PASSWORD, FAKE_SECRET, SHORT_SECRET, FAKE_CRON_SECRET, FAKE_HOOK_TOKEN]) {
        expect(occurrences(result.output, secret)).toBe(0);
      }
    });
  }
});

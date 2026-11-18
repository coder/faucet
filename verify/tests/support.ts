import { execFile, spawn } from 'node:child_process';
import { createDecipheriv, createHmac, generateKeyPairSync, randomBytes, randomInt, randomUUID, sign, type KeyObject } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { createServer as createHttpServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createServer as createHttpsServer } from 'node:https';
import { createServer } from 'node:net';
import { setTimeout as sleep } from 'node:timers/promises';
import { promisify } from 'node:util';
import { test as base } from '@e2e-dev/web';
import { RAW_EVENTS } from '../env.mjs';
import { stateFile } from '../runstate.mjs';

export { describe } from '@e2e-dev/web';
export { expect } from 'e2e';

const execFileAsync = promisify(execFile);

interface CronState {
  port: number;
  db: string;
  webhookSecret: string;
  cronSecret: string;
  hookToken: string;
  stubControl: string;
  stubUrl: string;
}

interface ReceiverState {
  url: string;
  control: string;
}

// TlsState names the certificate for 127.0.0.1 and localhost that app.mjs wrote, and its key.
interface TlsState {
  cert: string;
  key: string;
}

// GitHubAppState is faucet's GitHub App, as the stub knows it.
interface GitHubAppState {
  clientId: string;
  clientSecret: string;
}

interface RunState {
  dbHost: string;
  dbPort: number;
  psql: string;
  secret: string;
  githubToken: string;
  otherToken: string;
  webhookKey: string;
  binary: string;
  e2eBinary: string;
  agentBinary: string;
  loginAgentBinary: string;
  tls: TlsState;
  githubApp: GitHubAppState;
  cron: CronState;
  receiver: ReceiverState;
}

// A faucet process that the tests can reach: the main one, the one that has CRON_SECRET, or one a test starts.
interface Target {
  baseUrl: string;
  webhookSecret: string;
  database: string;
  // The FAUCET_WEBHOOK_KEY this process runs with, or null when it has none.
  webhookKey: string | null;
  // The FAUCET_TOKEN_KEY this process runs with, or null when its browser login is off.
  tokenKey: string | null;
}

// StubDelivery is one attempt of GitHub to deliver a webhook, as the stub in app.mjs lists it. ageMs is how long ago it
// happened. payload is what a redelivery posts to faucet.
export interface StubDelivery {
  guid: string;
  ageMs: number;
  status: number;
  event?: string;
  action?: string | null;
  payload?: object;
}

export interface StubRequest {
  method: string;
  path: string;
  query: string;
  // person is an access token that the stub issued to the person of the browser login.
  auth: 'hook' | 'user' | 'person' | 'other' | 'none';
}

// StubExchange is a request to the token endpoint of the stub's GitHub App: the grant, and issued or GitHub's error code.
export interface StubExchange {
  grant: 'authorization_code' | 'refresh_token';
  answer: string;
}

export interface StubAttempt {
  id: number;
  guid: string;
  status: number;
  redelivery: boolean;
}

export interface CronCredentials {
  authorization?: string;
  query?: string;
  body?: string;
}

export interface CronOptions {
  method?: string;
  send?: (secret: string) => CronCredentials;
}

export interface EventRow {
  id: number;
  source: string;
  delivery_id: string;
  type: string;
  repo_id: number;
  prs: number[];
  head_sha: string | null;
  data: Record<string, unknown>;
}

export interface Reply {
  status: number;
  body: string;
  headers: Headers;
}

export interface WaitedEvent {
  id: number;
  type: string;
  repo_id: number;
  prs: number[];
  head_sha: string;
  data: Record<string, unknown>;
  received_at: string;
}

export interface WaitOutcome extends Reply {
  isError: boolean;
  text: string;
  events: WaitedEvent[] | null;
  cursor: number | null;
  truncated: boolean | null;
}

// ToolResult is the result of a tools/call of wait_for_github_events, as an MCP client returns it.
export interface ToolResult {
  isError?: boolean;
  content?: { type: string; text: string }[];
  structuredContent?: { events: WaitedEvent[]; cursor: number };
}

export interface McpError {
  code: number;
  message: string;
  data?: unknown;
}

export interface McpReply<R = Record<string, unknown>> extends Reply {
  id: number | string | null;
  result: R | null;
  error: McpError | null;
}

// McpPath is a path of the GitHub events MCP endpoint. /mcp is its alias for one release.
export type McpPath = '/github/mcp' | '/mcp';

export interface McpOptions {
  // path defaults to /github/mcp.
  path?: McpPath;
  token?: string | null;
  // as 'other' sends the second principal's token, the one the stub answers as verify-other. token must then be unset.
  as?: 'other';
  name?: string | null;
  id?: number | string;
}

// SubscriptionRow is a faucet_subscriptions row as Postgres holds it. Times are epoch milliseconds, key_hash and secret_enc are hex.
export interface SubscriptionRow {
  key_hash: string;
  principal: string;
  callback_url: string;
  event_type: string;
  // event_types are the stored event types that event_type matched at the last subscribe.
  event_types: string[];
  arguments: Record<string, unknown>;
  repo_id: number;
  secret_enc: string;
  // previous_secret_enc and previous_secret_until are null unless a rotation window runs or ended and no delivery run has cleared it.
  previous_secret_enc: string | null;
  previous_secret_until: number | null;
  cursor: number;
  created_at: number;
  updated_at: number;
  expires_at: number;
}

// DeliveryRow is a faucet_deliveries row as Postgres holds it: a delivery that has not ended. Times are epoch milliseconds
// and lease_until is null while no run holds the delivery. The table has no column for a secret or a URL.
export interface DeliveryRow {
  key_hash: string;
  event_id: number;
  message_id: string;
  enqueued_at: number;
  next_attempt_at: number;
  attempts: number;
  lease_until: number | null;
}

export interface Delivery {
  event?: string;
  delivery?: string;
  body: string;
  signature?: string | null;
}

function isSecret(value: unknown): value is string {
  return typeof value === 'string' && value.length >= 16;
}

function readCronState(value: unknown): CronState {
  const { port, db, webhookSecret, cronSecret, hookToken, stubControl, stubUrl } = (value ?? {}) as Partial<CronState>;
  if (
    typeof port !== 'number' ||
    typeof db !== 'string' ||
    !isSecret(webhookSecret) ||
    !isSecret(cronSecret) ||
    !isSecret(hookToken) ||
    !isSecret(stubControl) ||
    typeof stubUrl !== 'string'
  ) {
    throw new Error('the run state written by app.mjs is malformed');
  }
  return { port, db, webhookSecret, cronSecret, hookToken, stubControl, stubUrl };
}

function readTlsState(value: unknown): TlsState {
  const { cert, key } = (value ?? {}) as Partial<TlsState>;
  if (typeof cert !== 'string' || typeof key !== 'string') throw new Error('the run state written by app.mjs is malformed');
  return { cert, key };
}

function readGitHubAppState(value: unknown): GitHubAppState {
  const { clientId, clientSecret } = (value ?? {}) as Partial<GitHubAppState>;
  if (typeof clientId !== 'string' || !isSecret(clientSecret)) throw new Error('the run state written by app.mjs is malformed');
  return { clientId, clientSecret };
}

function readReceiverState(value: unknown): ReceiverState {
  const { url, control } = (value ?? {}) as Partial<ReceiverState>;
  if (typeof url !== 'string' || !isSecret(control)) throw new Error('the run state written by app.mjs is malformed');
  return { url, control };
}

function readState(baseUrl: string): RunState {
  const port = Number(new URL(baseUrl).port);
  const state: unknown = JSON.parse(readFileSync(stateFile(port), 'utf8'));
  const {
    dbHost, dbPort, psql, secret, githubToken, otherToken, webhookKey, binary, e2eBinary, agentBinary, loginAgentBinary, tls, githubApp, cron, receiver,
  } = state as Partial<RunState>;
  if (
    typeof dbHost !== 'string' ||
    typeof dbPort !== 'number' ||
    typeof psql !== 'string' ||
    !isSecret(secret) ||
    !isSecret(githubToken) ||
    !isSecret(otherToken) ||
    !isSecret(webhookKey) ||
    typeof binary !== 'string' ||
    typeof e2eBinary !== 'string' ||
    typeof agentBinary !== 'string' ||
    typeof loginAgentBinary !== 'string'
  ) {
    throw new Error('the run state written by app.mjs is malformed');
  }
  return {
    dbHost, dbPort, psql, secret, githubToken, otherToken, webhookKey, binary, e2eBinary, agentBinary, loginAgentBinary,
    tls: readTlsState(tls), githubApp: readGitHubAppState(githubApp), cron: readCronState(cron), receiver: readReceiverState(receiver),
  };
}

export function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const probe = createServer();
    probe.once('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const address = probe.address();
      probe.close(() => (address !== null && typeof address === 'object' ? resolve(address.port) : reject(new Error('no port'))));
    });
  });
}

export class Faucet {
  readonly baseUrl: string;
  readonly #state: RunState;
  readonly #target: Target;
  readonly #cronSecret: string;

  private constructor(state: RunState, target: Target, cronSecret: string) {
    this.baseUrl = target.baseUrl;
    this.#state = state;
    this.#target = target;
    this.#cronSecret = cronSecret;
  }

  // main is the server that has no CRON_SECRET. It has webhook mode on, with the key in the run state.
  static main(appUrl: string): Faucet {
    const state = readState(appUrl);
    const target = { baseUrl: appUrl, webhookSecret: state.secret, database: 'postgres', webhookKey: state.webhookKey, tokenKey: null };
    return new Faucet(state, target, state.cron.cronSecret);
  }

  // cron is the server that has CRON_SECRET. It has its own database, because a purge moves the purge mark of the whole table.
  static cron(appUrl: string): Faucet {
    const state = readState(appUrl);
    const target = {
      baseUrl: `http://127.0.0.1:${state.cron.port}`,
      webhookSecret: state.cron.webhookSecret,
      database: state.cron.db,
      webhookKey: null,
      tokenKey: null,
    };
    return new Faucet(state, target, state.cron.cronSecret);
  }

  get binary(): string {
    return this.#state.binary;
  }

  // hookEnv is the environment that turns on redelivery for the webhook that the stub serves. A test passes it to
  // withFaucet, and never names the token.
  hookEnv(): Record<string, string> {
    return { FAUCET_HOOK_TOKEN: this.#state.cron.hookToken, FAUCET_HOOK_ORG: 'verify', FAUCET_HOOK_ID: '7' };
  }

  // webhookTarget is where the stub posts a redelivery, and the secret that it signs the post with.
  webhookTarget(): { url: string; secret: string } {
    return { url: this.baseUrl, secret: this.#target.webhookSecret };
  }

  // withFaucet starts one more faucet on its own database, runs fn against it and stops it. The new faucet has a random
  // CRON_SECRET, webhook mode off and the raw events of RAW_EVENTS. env adds the rest, and FAUCET_RAW_EVENTS: '' offers none. fn gets the faucet and a function that returns its
  // output so far. opts.e2eBuild runs the binary that was built with -tags faucet_e2e, whose callback client, document client
  // and client for the keys of workload issuers reach loopback, over http too, and which trusts the certificate that Documents
  // and Issuer serve. opts.publicUrl sets FAUCET_PUBLIC_URL to the new faucet's own URL, which turns on its login server.
  // opts.browserLogin also turns on its browser login, with the stub as GitHub and its GitHub App, and a random token key, and
  // runs the e2e build.
  async withFaucet<T>(
    env: Record<string, string>,
    fn: (started: Faucet, output: () => string) => Promise<T>,
    opts: { e2eBuild?: boolean; publicUrl?: boolean; browserLogin?: boolean } = {},
  ): Promise<T> {
    const database = `faucet_${randomBytes(6).toString('hex')}`;
    const port = await freePort();
    const webhookSecret = randomBytes(32).toString('hex');
    const cronSecret = randomBytes(32).toString('hex');
    const { dbHost, dbPort, cron, githubApp, tls } = this.#state;
    const login = opts.browserLogin === true;
    const e2eBuild = opts.e2eBuild === true || login;
    const binary = e2eBuild ? this.#state.e2eBinary : this.#state.binary;
    const tokenKey = login ? `e2e:${randomBytes(32).toString('base64')}` : null;
    const databaseUrl = `postgres://postgres@/${database}?${new URLSearchParams({ host: dbHost, port: String(dbPort), sslmode: 'disable' })}`;

    await this.sql(`create database ${database}`);
    const child = spawn(binary, [], {
      env: {
        DATABASE_URL: databaseUrl,
        GITHUB_WEBHOOK_SECRET: webhookSecret,
        PORT: String(port),
        FAUCET_GITHUB_API_URL: cron.stubUrl,
        CRON_SECRET: cronSecret,
        FAUCET_RAW_EVENTS: RAW_EVENTS,
        ...(opts.publicUrl === true || login ? { FAUCET_PUBLIC_URL: `http://127.0.0.1:${port}` } : {}),
        // Go reads its roots from SSL_CERT_FILE in place of the system's.
        ...(e2eBuild ? { SSL_CERT_FILE: tls.cert } : {}),
        ...(tokenKey !== null
          ? {
              FAUCET_GITHUB_APP_CLIENT_ID: githubApp.clientId,
              FAUCET_GITHUB_APP_CLIENT_SECRET: githubApp.clientSecret,
              FAUCET_TOKEN_KEY: tokenKey,
              FAUCET_GITHUB_URL: cron.stubUrl,
            }
          : {}),
        ...env,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let output = '';
    child.stdout.on('data', (chunk: Buffer) => (output += chunk.toString()));
    child.stderr.on('data', (chunk: Buffer) => (output += chunk.toString()));
    const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
    const running = (): boolean => child.exitCode === null && child.signalCode === null;

    try {
      const baseUrl = `http://127.0.0.1:${port}`;
      for (let waited = 0; !(await isHealthy(baseUrl)); waited += 100) {
        if (!running() || waited > 20_000) throw new Error(`the started faucet did not become healthy:\n${output}`);
        await sleep(100);
      }
      const target = { baseUrl, webhookSecret, database, webhookKey: env.FAUCET_WEBHOOK_KEY ?? null, tokenKey };
      return await fn(new Faucet(this.#state, target, cronSecret), () => output);
    } finally {
      if (running()) child.kill('SIGTERM');
      await Promise.race([exited, sleep(10_000)]);
      if (running()) child.kill('SIGKILL');
      await exited;
      await this.sql(`drop database ${database} with (force)`);
    }
  }

  sign(body: string, secret: string = this.#target.webhookSecret): string {
    return `sha256=${createHmac('sha256', secret).update(body).digest('hex')}`;
  }

  async deliver(delivery: Delivery): Promise<Reply> {
    const headers = new Headers();
    if (delivery.event !== undefined) headers.set('X-GitHub-Event', delivery.event);
    if (delivery.delivery !== undefined) headers.set('X-GitHub-Delivery', delivery.delivery);
    if (delivery.signature !== null) {
      headers.set('X-Hub-Signature-256', delivery.signature ?? this.sign(delivery.body));
    }
    return this.request('/webhook/github', { method: 'POST', headers, body: delivery.body });
  }

  // cronJob calls GET /cron/<job> the way Vercel Cron does, with the run's cron secret as a bearer token. opts.send says
  // what the request carries instead, as a function of the secret, so that no test names the secret.
  async cronJob(job: 'purge' | 'deliver' | 'redeliver', opts: CronOptions = {}): Promise<Reply> {
    const send = opts.send ?? ((secret: string): CronCredentials => ({ authorization: `Bearer ${secret}` }));
    const { authorization, query, body } = send(this.#cronSecret);
    const headers = new Headers();
    if (authorization !== undefined) headers.set('Authorization', authorization);
    return this.request(`/cron/${job}${query === undefined ? '' : `?${query}`}`, { method: opts.method ?? 'GET', headers, body });
  }

  // waitForEvents calls the wait_for_github_events tool on /github/mcp the way a
  // stateless 2026-07-28 client does. A string token replaces the run's valid
  // GitHub token and null sends no Authorization header.
  async waitForEvents(args: Record<string, unknown>, token: string | null = this.#state.githubToken): Promise<WaitOutcome> {
    const protocolVersion = '2026-07-28';
    const headers = new Headers({
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
      'MCP-Protocol-Version': protocolVersion,
      'Mcp-Method': 'tools/call',
      'Mcp-Name': 'wait_for_github_events',
    });
    if (token !== null) headers.set('Authorization', `Bearer ${token}`);
    const body = JSON.stringify({
      jsonrpc: '2.0',
      id: 1,
      method: 'tools/call',
      params: {
        name: 'wait_for_github_events',
        arguments: args,
        _meta: {
          'io.modelcontextprotocol/protocolVersion': protocolVersion,
          'io.modelcontextprotocol/clientInfo': { name: 'faucet-verify', version: '0' },
          'io.modelcontextprotocol/clientCapabilities': {},
        },
      },
    });
    const reply = await this.request('/github/mcp', { method: 'POST', headers, body });
    const outcome: WaitOutcome = { ...reply, isError: false, text: '', events: null, cursor: null, truncated: null };
    if (reply.status !== 200) return outcome;

    const { result } = JSON.parse(reply.body) as {
      result: {
        isError?: boolean;
        content: { text: string }[];
        structuredContent?: { events: WaitedEvent[]; cursor: number; truncated: boolean };
      };
    };
    outcome.isError = result.isError === true;
    outcome.text = result.content[0]?.text ?? '';
    outcome.events = result.structuredContent?.events ?? null;
    outcome.cursor = result.structuredContent?.cursor ?? null;
    outcome.truncated = result.structuredContent?.truncated ?? null;
    return outcome;
  }

  async mcp<R = Record<string, unknown>>(
    method: string,
    params: Record<string, unknown> = {},
    opts: McpOptions = {},
  ): Promise<McpReply<R>> {
    const protocolVersion = '2026-07-28';
    if (opts.as !== undefined && opts.token !== undefined) throw new Error('set opts.as or opts.token, not both');
    const token = opts.as === 'other' ? this.#state.otherToken : opts.token === undefined ? this.#state.githubToken : opts.token;
    const name = opts.name === undefined ? (typeof params.name === 'string' ? params.name : null) : opts.name;
    const headers = new Headers({
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
      'MCP-Protocol-Version': protocolVersion,
      'Mcp-Method': method,
    });
    if (name !== null) headers.set('Mcp-Name', name);
    if (token !== null) headers.set('Authorization', `Bearer ${token}`);
    const body = JSON.stringify({
      jsonrpc: '2.0',
      id: opts.id ?? 1,
      method,
      params: {
        ...params,
        _meta: {
          'io.modelcontextprotocol/protocolVersion': protocolVersion,
          'io.modelcontextprotocol/clientInfo': { name: 'faucet-verify', version: '0' },
          'io.modelcontextprotocol/clientCapabilities': {},
        },
      },
    });
    const reply = await this.request(opts.path ?? '/github/mcp', { method: 'POST', headers, body });
    if (!(reply.headers.get('content-type') ?? '').includes('json')) {
      return { ...reply, id: null, result: null, error: null };
    }
    const parsed = JSON.parse(reply.body) as { id?: number | string; result?: R; error?: McpError };
    return { ...reply, id: parsed.id ?? null, result: parsed.result ?? null, error: parsed.error ?? null };
  }

  async request(path: string, init: RequestInit = {}): Promise<Reply> {
    const response = await fetch(`${this.baseUrl}${path}`, init);
    return { status: response.status, body: await response.text(), headers: response.headers };
  }

  async rows(repoId: number): Promise<EventRow[]> {
    if (!Number.isSafeInteger(repoId)) throw new Error('repoId must be an integer');
    const query = `select coalesce(json_agg(t order by id), '[]') from (
      select id, source, delivery_id, type, repo_id, prs, head_sha, data
      from faucet_events where repo_id = ${repoId}) t`;
    return JSON.parse(await this.sql(query)) as EventRow[];
  }

  async backdate(deliveryId: string, days: number): Promise<void> {
    if (!/^[0-9a-f-]{36}$/.test(deliveryId)) throw new Error('deliveryId must be a UUID');
    if (!Number.isInteger(days) || days < 1) throw new Error('days must be a positive integer');
    const query = `update faucet_events set received_at = now() - interval '${days} days' where delivery_id = '${deliveryId}' returning id`;
    if ((await this.sql(query)) === '') throw new Error(`no event has the delivery id ${deliveryId}`);
  }

  async purgeMark(): Promise<number> {
    return Number(await this.sql('select purged_through from faucet_purge_mark'));
  }

  async subscriptions(marker: string): Promise<SubscriptionRow[]> {
    if (!/^[0-9a-z-]{8,}$/.test(marker)) throw new Error('marker must be 8 or more lowercase letters, digits or hyphens');
    const ms = (column: string): string => `floor(extract(epoch from ${column}) * 1000)::bigint as ${column}`;
    const query = `select coalesce(json_agg(t order by created_at, callback_url), '[]') from (
      select encode(key_hash, 'hex') as key_hash, principal, callback_url, event_type, event_types, arguments, repo_id,
        encode(secret_enc, 'hex') as secret_enc, encode(previous_secret_enc, 'hex') as previous_secret_enc, ${ms('previous_secret_until')},
        cursor, ${ms('created_at')}, ${ms('updated_at')}, ${ms('expires_at')}
      from faucet_subscriptions where position('${marker}' in callback_url) > 0) t`;
    return JSON.parse(await this.sql(query)) as SubscriptionRow[];
  }

  // secretCopies counts the rows of faucet_subscriptions that hold the subscriber secret in any column: as text, as the
  // text without its whsec_ prefix, or as the decoded bytes, which is how a plaintext bytea column (secret_enc or
  // previous_secret_enc) would hold it.
  async secretCopies(secret: string): Promise<number> {
    if (!/^whsec_[A-Za-z0-9+/]+=*$/.test(secret)) throw new Error('secret must be a whsec_ value in standard base64');
    const encoded = secret.slice('whsec_'.length);
    const query = `select count(*) from faucet_subscriptions t where strpos(t::text, '${encoded}') > 0
      or position(convert_to('${encoded}', 'UTF8') in t.secret_enc) > 0
      or position(decode('${encoded}', 'base64') in t.secret_enc) > 0
      or position(convert_to('${encoded}', 'UTF8') in t.previous_secret_enc) > 0
      or position(decode('${encoded}', 'base64') in t.previous_secret_enc) > 0`;
    return Number(await this.sql(query));
  }

  // expireSubscriptions moves the expiry of the rows whose callback URL holds marker into the past, which the clock of
  // the server cannot be made to do. It returns how many rows it changed.
  async expireSubscriptions(marker: string): Promise<number> {
    if (!/^[0-9a-z-]{8,}$/.test(marker)) throw new Error('marker must be 8 or more lowercase letters, digits or hyphens');
    const query = `with moved as (update faucet_subscriptions set expires_at = now() - interval '1 minute'
      where position('${marker}' in callback_url) > 0 returning 1) select count(*) from moved`;
    return Number(await this.sql(query));
  }

  // endRotationWindows moves the end of the rotation window of the rows whose callback URL holds marker into the past, which
  // the clock of the server cannot be made to do (a window lasts up to 10 minutes). It returns how many rows it changed.
  async endRotationWindows(marker: string): Promise<number> {
    if (!/^[0-9a-z-]{8,}$/.test(marker)) throw new Error('marker must be 8 or more lowercase letters, digits or hyphens');
    const query = `with moved as (update faucet_subscriptions set previous_secret_until = now() - interval '1 second'
      where position('${marker}' in callback_url) > 0 and previous_secret_enc is not null returning 1) select count(*) from moved`;
    return Number(await this.sql(query));
  }

  async deliveries(): Promise<DeliveryRow[]> {
    const ms = (column: string): string => `floor(extract(epoch from ${column}) * 1000)::bigint as ${column}`;
    const query = `select coalesce(json_agg(t order by event_id, key_hash), '[]') from (
      select encode(key_hash, 'hex') as key_hash, event_id, message_id, ${ms('enqueued_at')}, ${ms('next_attempt_at')},
        attempts, ${ms('lease_until')}
      from faucet_deliveries) t`;
    return JSON.parse(await this.sql(query)) as DeliveryRow[];
  }

  // makeDeliveriesDue moves the next attempt of every queued delivery into the past and ends its lease, which is how a test
  // gets past a backoff of 15 seconds to 6 minutes that it cannot wait out. It returns how many rows it changed.
  async makeDeliveriesDue(): Promise<number> {
    const query = `with moved as (update faucet_deliveries set next_attempt_at = now() - interval '1 second', lease_until = null
      returning 1) select count(*) from moved`;
    return Number(await this.sql(query));
  }

  // leaseDeliveries plays a run that claimed every queued delivery and then died before it recorded a result: the lease
  // ends `seconds` from now (a negative number puts it in the past) and attempts is what the claim left. It returns how many
  // rows it changed.
  async leaseDeliveries(seconds: number, attempts: number): Promise<number> {
    if (!Number.isInteger(seconds)) throw new Error('seconds must be an integer');
    if (!Number.isInteger(attempts) || attempts < 0) throw new Error('attempts must be a non-negative integer');
    const query = `with moved as (update faucet_deliveries
      set lease_until = now() + interval '${seconds} seconds', attempts = ${attempts} returning 1) select count(*) from moved`;
    return Number(await this.sql(query));
  }

  // openSecret opens the sealed secret of a row with this server's key, or with key when given. It throws when the key or the
  // row's key_hash is not the one that sealed it.
  openSecret(row: SubscriptionRow, key?: Buffer): string {
    const sealKey = key ?? (this.#target.webhookKey === null ? null : Buffer.from(this.#target.webhookKey, 'base64'));
    if (sealKey === null) throw new Error('this faucet has no webhook key');
    return openSealedSecret(sealKey, row.secret_enc, row.key_hash);
  }

  // openPreviousSecret opens previous_secret_enc, sealed like secret_enc, with this server's key.
  openPreviousSecret(row: SubscriptionRow): string {
    if (row.previous_secret_enc === null) throw new Error('the row has no previous secret');
    if (this.#target.webhookKey === null) throw new Error('this faucet has no webhook key');
    return openSealedSecret(Buffer.from(this.#target.webhookKey, 'base64'), row.previous_secret_enc, row.key_hash);
  }

  // connectionToken opens the sealed access or refresh token of the faucet_connections row with the ID id, with this server's
  // token key. It is an independent reading of the layout: the version byte 0x01, a 12-byte nonce, then the AES-256-GCM
  // ciphertext and tag, with "faucet connection token v1", the provider, the connection ID and the field joined by NUL bytes
  // as additional data. It throws when the key or the row's key_id is not the one that sealed it.
  async connectionToken(id: string, field: 'access' | 'refresh'): Promise<string> {
    if (!/^github:\d+$/.test(id)) throw new Error('id must be github:<user id>');
    const { tokenKey } = this.#target;
    if (tokenKey === null) throw new Error('this faucet has no token key');
    const [keyId, key] = tokenKey.split(':');
    const query = `select row_to_json(t) from (select provider, key_id, encode(${field}_token_enc, 'hex') as sealed
      from faucet_connections where id = '${id}') t`;
    const row = JSON.parse((await this.sql(query)) || 'null') as { provider: string; key_id: string; sealed: string } | null;
    if (row === null) throw new Error(`no connection has the ID ${id}`);
    if (row.key_id !== keyId) throw new Error(`the row is sealed under key ${row.key_id}`);
    const aad = Buffer.from(['faucet connection token v1', row.provider, id, field].join('\0'));
    return openSealedSecret(Buffer.from(key, 'base64'), row.sealed, aad.toString('hex'));
  }

  // clients runs `faucet clients` with args against this faucet's database, as an operator does.
  async clients(args: string[]): Promise<{ code: number; stdout: string; stderr: string }> {
    const { dbHost, dbPort } = this.#state;
    const env = { DATABASE_URL: `postgres://postgres@/${this.#target.database}?${new URLSearchParams({ host: dbHost, port: String(dbPort), sslmode: 'disable' })}` };
    try {
      const { stdout, stderr } = await execFileAsync(this.#state.binary, ['clients', ...args], { env });
      return { code: 0, stdout, stderr };
    } catch (err) {
      const failure = err as { code?: unknown; stdout?: string; stderr?: string };
      return { code: typeof failure.code === 'number' ? failure.code : -1, stdout: failure.stdout ?? '', stderr: failure.stderr ?? '' };
    }
  }

  // serviceAgent runs the go-sdk service agent once: it logs in to the endpoint at path with the client ID and secret alone and
  // calls the wait tool with args.
  async serviceAgent(
    path: McpPath,
    clientId: string,
    secret: string,
    args: { repo: string; types: string[]; cursor?: number; timeout?: number },
  ): Promise<{ code: number; stdout: string; stderr: string }> {
    const argv = [
      '-url', `${this.baseUrl}${path}`, '-client-id', clientId, '-repo', args.repo, '-types', args.types.join(','),
      '-cursor', String(args.cursor ?? 0), '-timeout', String(args.timeout ?? 1),
    ];
    try {
      const { stdout, stderr } = await execFileAsync(this.#state.agentBinary, argv, { env: { FAUCET_CLIENT_SECRET: secret } });
      return { code: 0, stdout, stderr };
    } catch (err) {
      const failure = err as { code?: unknown; stdout?: string; stderr?: string };
      return { code: typeof failure.code === 'number' ? failure.code : -1, stdout: failure.stdout ?? '', stderr: failure.stderr ?? '' };
    }
  }

  // loginAgent runs the go-sdk login agent once: it logs in to the endpoint at path as the client whose document is at
  // clientId, with its person going through the consent page and the stub's GitHub, and calls the wait tool with args.
  // args.refresh then refreshes its token, calls again and presents the refresh token that the refresh replaced.
  async loginAgent(
    path: McpPath,
    clientId: string,
    args: { repo: string; types: string[]; cursor?: number; timeout?: number; refresh?: boolean },
  ): Promise<LoginAgentRun> {
    const argv = [
      '-url', `${this.baseUrl}${path}`, '-client-id', clientId, '-repo', args.repo, '-types', args.types.join(','),
      '-cursor', String(args.cursor ?? 0), '-timeout', String(args.timeout ?? 1), ...(args.refresh === true ? ['-refresh'] : []),
    ];
    let code = 0;
    let stdout = '';
    let stderr = '';
    try {
      ({ stdout, stderr } = await execFileAsync(this.#state.loginAgentBinary, argv, { env: {} }));
    } catch (err) {
      const failure = err as { code?: unknown; stdout?: string; stderr?: string };
      code = typeof failure.code === 'number' ? failure.code : -1;
      stdout = failure.stdout ?? '';
      stderr = failure.stderr ?? '';
    }
    const run: LoginAgentRun = { code, stderr, result: null, refreshed: null, reused: null, logins: 0, tokens: [] };
    if (code !== 0) return run;
    const printed = JSON.parse(stdout) as { result: ToolResult; refreshed?: ToolResult; reused?: string; logins: number; tokens: string[] };
    return { ...run, result: printed.result, refreshed: printed.refreshed ?? null, reused: printed.reused ?? null, logins: printed.logins, tokens: printed.tokens };
  }

  // appLog lists the tokens that the stub's GitHub App issued and the token requests that it answered, in order.
  async appLog(): Promise<{ tokens: string[]; exchanges: StubExchange[] }> {
    const response = await fetch(`${this.#state.cron.stubUrl}/__stub/app`, { headers: { 'X-Stub-Control': this.#state.cron.stubControl } });
    if (response.status !== 200) throw new Error(`the stub answered GET /__stub/app with ${response.status}`);
    return (await response.json()) as { tokens: string[]; exchanges: StubExchange[] };
  }

  async sql(query: string): Promise<string> {
    const { stdout } = await execFileAsync(this.#state.psql, [
      '-X', '-A', '-t', '-q',
      '-h', this.#state.dbHost,
      '-p', String(this.#state.dbPort),
      '-U', 'postgres',
      '-d', this.#target.database,
      '-v', 'ON_ERROR_STOP=1',
      '-c', query,
    ]);
    return stdout.trim();
  }

  leaksSecret(text: string): boolean {
    const { secret, githubToken, otherToken, webhookKey, cron, receiver, githubApp } = this.#state;
    const secrets = [
      secret, githubToken, otherToken, webhookKey, cron.webhookSecret, cron.cronSecret, cron.hookToken, cron.stubControl, receiver.control,
      githubApp.clientSecret,
    ];
    const { webhookKey: ownWebhookKey, tokenKey } = this.#target;
    const own = [
      this.#target.webhookSecret, this.#cronSecret, ...(ownWebhookKey === null ? [] : [ownWebhookKey]),
      ...(tokenKey === null ? [] : [tokenKey, tokenKey.slice(tokenKey.indexOf(':') + 1)]),
    ];
    return [...secrets, ...own].some((value) => text.includes(value));
  }
}

const SEAL_VERSION = 1;
const GCM_NONCE_BYTES = 12;
const GCM_TAG_BYTES = 16;

// openSealedSecret opens secret_enc, which is the version byte 0x01, a 12-byte nonce, then the AES-256-GCM ciphertext and its
// tag, with the row's key_hash as additional data. It is an independent reading of that layout: it shares nothing with faucet.
export function openSealedSecret(key: Buffer, secretEncHex: string, keyHashHex: string): string {
  const sealed = Buffer.from(secretEncHex, 'hex');
  if (sealed[0] !== SEAL_VERSION) throw new Error(`unknown seal version ${sealed[0]}`);
  const nonce = sealed.subarray(1, 1 + GCM_NONCE_BYTES);
  const ciphertext = sealed.subarray(1 + GCM_NONCE_BYTES, sealed.length - GCM_TAG_BYTES);
  const decipher = createDecipheriv('aes-256-gcm', key, nonce);
  decipher.setAAD(Buffer.from(keyHashHex, 'hex'));
  decipher.setAuthTag(sealed.subarray(sealed.length - GCM_TAG_BYTES));
  return Buffer.concat([decipher.update(ciphertext), decipher.final()]).toString('utf8');
}

// The runner appends to app.log across runs, so the log of this run starts at the last line that app.mjs writes once Postgres is up.
// Without that cut, a line left by an earlier run satisfies a test of this one.
const RUN_START = 'faucet-verify: postgres listening on ';

export function appLog(): string {
  const log = readFileSync('.e2e/logs/app.log', 'utf8');
  const start = log.lastIndexOf(RUN_START);
  if (start < 0) throw new Error('app.log has no line that marks the start of this run');
  return log.slice(start);
}

async function isHealthy(baseUrl: string): Promise<boolean> {
  try {
    return (await fetch(`${baseUrl}/healthz`)).status === 200;
  } catch {
    return false;
  }
}

export interface ReceivedRequest {
  id: string;
  behavior: string;
  method: string;
  path: string;
  headers: Record<string, string>;
  body: string;
  // valid, invalid, missing (a header is absent) or unregistered (the test registered no secret for the id).
  signature: 'valid' | 'invalid' | 'missing' | 'unregistered';
}

// Receiver reads and sets what the callback receiver in app.mjs does. A callback URL is <url>/hook/<behavior>/<id>, and
// app.mjs lists the behaviors. It checks signatures with the secret that register gave for the id, and keeps every request.
export class Receiver {
  readonly #url: string;
  readonly #control: string;

  constructor(appUrl: string) {
    const { receiver } = readState(appUrl);
    this.#url = receiver.url;
    this.#control = receiver.control;
  }

  callback(behavior: string, id: string, opts: { port?: number; query?: string } = {}): string {
    const url = new URL(`${this.#url}/hook/${behavior}/${id}`);
    if (opts.port !== undefined) url.port = String(opts.port);
    if (opts.query !== undefined) url.search = opts.query;
    return url.toString();
  }

  get port(): number {
    return Number(new URL(this.#url).port);
  }

  async register(id: string, secret: string): Promise<void> {
    await this.#call('PUT', '/__receiver/secrets', { id, secret });
  }

  // revoke makes the GitHub stub answer 404 for the repository from now on, as it does after a principal loses access.
  async revoke(repoId: number): Promise<void> {
    await this.#call('PUT', '/__receiver/revoked', { repoId });
  }

  async requests(id: string): Promise<ReceivedRequest[]> {
    return (await this.#call('GET', `/__receiver/requests?id=${encodeURIComponent(id)}`)) as ReceivedRequest[];
  }

  async #call(method: string, path: string, body?: object): Promise<unknown> {
    const response = await fetch(`${this.#url}${path}`, {
      method,
      headers: { 'X-Stub-Control': this.#control, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (response.status !== 200) throw new Error(`the receiver answered ${method} ${path} with ${response.status}`);
    return response.json();
  }
}

// LoginAgentRun is what one run of the login agent printed. result, refreshed and reused are null when the agent failed.
export interface LoginAgentRun {
  code: number;
  stderr: string;
  result: ToolResult | null;
  // refreshed is the result of the call after the refresh, and reused what the token endpoint answered to the refresh token
  // that the refresh replaced: 'issued', or its OAuth error code. Both are null without args.refresh.
  refreshed: ToolResult | null;
  reused: string | null;
  // logins counts the consent pages that the agent's person went through.
  logins: number;
  // tokens are the faucet tokens that the agent held, for a test to look for in other text and never to print.
  tokens: string[];
}

// ClientDocument is what a client ID metadata document says beyond its client_id.
export interface ClientDocument {
  client_name?: string;
  redirect_uris: string[];
}

// Documents serves client ID metadata documents over https on 127.0.0.1 with the certificate that app.mjs wrote, which a
// faucet that withFaucet starts with { browserLogin: true } trusts.
export class Documents {
  readonly #server: Server;
  readonly #documents: Map<string, object>;

  private constructor(server: Server, documents: Map<string, object>) {
    this.#server = server;
    this.#documents = documents;
  }

  static async start(appUrl: string): Promise<Documents> {
    const { tls } = readState(appUrl);
    const documents = new Map<string, object>();
    const server = createHttpsServer({ cert: readFileSync(tls.cert), key: readFileSync(tls.key) }, (req, res) => {
      const document = documents.get(req.url ?? '');
      if (document === undefined) return void res.writeHead(404).end();
      res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(document));
    });
    await listen(server, '127.0.0.1');
    return new Documents(server, documents);
  }

  // add serves a document at /<name>.json and returns its URL, which is the client's ID.
  add(name: string, document: ClientDocument): string {
    const clientId = `https://127.0.0.1:${portOf(this.#server)}/${name}.json`;
    this.#documents.set(`/${name}.json`, {
      client_id: clientId, token_endpoint_auth_method: 'none', grant_types: ['authorization_code', 'refresh_token'], response_types: ['code'],
      ...document,
    });
    return clientId;
  }

  async close(): Promise<void> {
    await close(this.#server);
  }
}

// Issuer is the OpenID Connect issuer of a workload's platform, such as a Kubernetes cluster, on 127.0.0.1. It serves its
// discovery document and its key set over https with the certificate that app.mjs wrote, which the faucets of the e2e build
// trust, counts the requests for each path, and signs tokens with its RS256 key, as the platform does.
export class Issuer {
  static readonly kid = 'verify-issuer';
  readonly #server: Server;
  readonly #key: KeyObject;
  readonly #hits: Map<string, number>;

  private constructor(server: Server, key: KeyObject, hits: Map<string, number>) {
    this.#server = server;
    this.#key = key;
    this.#hits = hits;
  }

  static async start(appUrl: string): Promise<Issuer> {
    const { tls } = readState(appUrl);
    const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
    const jwk = { ...publicKey.export({ format: 'jwk' }), kid: Issuer.kid, use: 'sig', alg: 'RS256' };
    const hits = new Map<string, number>();
    const server = createHttpsServer({ cert: readFileSync(tls.cert), key: readFileSync(tls.key) }, (req, res) => {
      const path = req.url ?? '';
      const url = `https://127.0.0.1:${portOf(server)}`;
      hits.set(path, (hits.get(path) ?? 0) + 1);
      const documents: Record<string, object> = {
        '/.well-known/openid-configuration': { issuer: url, jwks_uri: `${url}/openid/v1/jwks`, id_token_signing_alg_values_supported: ['RS256'] },
        '/openid/v1/jwks': { keys: [jwk] },
      };
      const document = documents[path];
      if (document === undefined) return void res.writeHead(404).end();
      res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(document));
    });
    await listen(server, '127.0.0.1');
    return new Issuer(server, privateKey, hits);
  }

  // url is the issuer, which its tokens name as iss.
  get url(): string {
    return `https://127.0.0.1:${portOf(this.#server)}`;
  }

  hits(path: string): number {
    return this.#hits.get(path) ?? 0;
  }

  // sign returns a JWT of the issuer with claims, issued now and valid for lifetimeS seconds.
  sign(claims: Record<string, unknown>, lifetimeS = 600): string {
    const now = Math.floor(Date.now() / 1000);
    const part = (v: object): string => Buffer.from(JSON.stringify(v)).toString('base64url');
    const input = `${part({ alg: 'RS256', typ: 'JWT', kid: Issuer.kid })}.${part({ iss: this.url, iat: now, nbf: now, exp: now + lifetimeS, ...claims })}`;
    return `${input}.${sign('sha256', Buffer.from(input), this.#key).toString('base64url')}`;
  }

  async close(): Promise<void> {
    await close(this.#server);
  }
}

// Site is a web server on 127.0.0.1 that a test serves pages from, such as a redirect URI or a page of another site, and that
// records the path and query of every request. A browser reaches it as http://127.0.0.1:<port>, and as another site than
// faucet's as http://localhost:<port>.
export class Site {
  readonly #server: Server;
  readonly #pages: Map<string, string>;
  readonly #requests: { path: string; query: string }[];

  private constructor(server: Server, pages: Map<string, string>, requests: { path: string; query: string }[]) {
    this.#server = server;
    this.#pages = pages;
    this.#requests = requests;
  }

  static async start(): Promise<Site> {
    const pages = new Map<string, string>();
    const requests: { path: string; query: string }[] = [];
    const server = createHttpServer((req: IncomingMessage, res: ServerResponse) => {
      const url = new URL(req.url ?? '/', 'http://site');
      requests.push({ path: url.pathname, query: url.search });
      const page = pages.get(url.pathname);
      if (page === undefined) return void res.writeHead(404).end();
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }).end(page);
    });
    await listen(server, '127.0.0.1');
    return new Site(server, pages, requests);
  }

  url(path: string, host: '127.0.0.1' | 'localhost' = '127.0.0.1'): string {
    return `http://${host}:${portOf(this.#server)}${path}`;
  }

  serve(path: string, html: string): void {
    this.#pages.set(path, html);
  }

  requests(path: string): { path: string; query: string }[] {
    return this.#requests.filter((r) => r.path === path);
  }

  async close(): Promise<void> {
    this.#server.closeAllConnections();
    await close(this.#server);
  }
}

async function listen(server: Server, host: string): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, host, () => resolve());
  });
}

function portOf(server: Server): number {
  const address = server.address();
  if (address === null || typeof address !== 'object') throw new Error('the server does not listen');
  return address.port;
}

function close(server: Server): Promise<void> {
  return new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
}

// HookStub sets and reads what the GitHub stub in app.mjs serves for the webhook that redelivery manages. It plays GitHub's
// redelivery too: it posts the stored payload, signed, to the faucet that the test armed it with.
export class HookStub {
  readonly #url: string;
  readonly #control: string;

  constructor(appUrl: string) {
    const { cron } = readState(appUrl);
    this.#url = cron.stubUrl;
    this.#control = cron.stubControl;
  }

  // arm replaces the attempts that the stub lists and clears its request log. pageSize caps a page, redeliveryStatus is the
  // status of a redelivery request (GitHub answers 202) and listDelayMs holds every list answer back.
  async arm(
    faucet: Faucet,
    deliveries: StubDelivery[],
    opts: { pageSize?: number; redeliveryStatus?: number; listDelayMs?: number } = {},
  ): Promise<void> {
    await this.#call('PUT', '/__stub/deliveries', { deliveries, target: faucet.webhookTarget(), ...opts });
  }

  // add lists more attempts, as if GitHub delivered them after the last run.
  async add(deliveries: StubDelivery[]): Promise<void> {
    await this.#call('POST', '/__stub/deliveries', { deliveries });
  }

  async requests(): Promise<StubRequest[]> {
    return (await this.#call('GET', '/__stub/requests')) as StubRequest[];
  }

  async attempts(): Promise<StubAttempt[]> {
    return (await this.#call('GET', '/__stub/attempts')) as StubAttempt[];
  }

  async #call(method: string, path: string, body?: object): Promise<unknown> {
    const response = await fetch(`${this.#url}${path}`, {
      method,
      headers: { 'X-Stub-Control': this.#control, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (response.status !== 200) throw new Error(`the stub answered ${method} ${path} with ${response.status}`);
    return response.json();
  }
}

export const test = base.extend<{
  faucet: Faucet;
  cron: Faucet;
  receiver: Receiver;
  hooks: HookStub;
  documents: Documents;
  issuer: Issuer;
  site: Site;
}>({
  faucet: async ({ app }, use) => {
    await use(Faucet.main(app.baseUrl));
  },
  cron: async ({ app }, use) => {
    await use(Faucet.cron(app.baseUrl));
  },
  receiver: async ({ app }, use) => {
    await use(new Receiver(app.baseUrl));
  },
  hooks: async ({ app }, use) => {
    await use(new HookStub(app.baseUrl));
  },
  documents: async ({ app }, use) => {
    const documents = await Documents.start(app.baseUrl);
    try {
      await use(documents);
    } finally {
      await documents.close();
    }
  },
  issuer: async ({ app }, use) => {
    const issuer = await Issuer.start(app.baseUrl);
    try {
      await use(issuer);
    } finally {
      await issuer.close();
    }
  },
  site: async ({}, use) => {
    const site = await Site.start();
    try {
      await use(site);
    } finally {
      await site.close();
    }
  },
});

export function newRepoId(): number {
  return randomInt(1_000_000, 2_000_000_000);
}

export function newDeliveryId(): string {
  return randomUUID();
}

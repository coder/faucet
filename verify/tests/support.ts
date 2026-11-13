import { execFile, spawn } from 'node:child_process';
import { createDecipheriv, createHmac, randomBytes, randomInt, randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { setTimeout as sleep } from 'node:timers/promises';
import { promisify } from 'node:util';
import { test as base } from '@e2e-dev/web';
import { stateFile } from '../runstate.mjs';

export { describe } from '@e2e-dev/web';
export { expect } from 'e2e';

const execFileAsync = promisify(execFile);

interface CronState {
  port: number;
  db: string;
  webhookSecret: string;
  cronSecret: string;
  stubUrl: string;
}

interface ReceiverState {
  url: string;
  control: string;
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

export interface McpOptions {
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
  const { port, db, webhookSecret, cronSecret, stubUrl } = (value ?? {}) as Partial<CronState>;
  if (
    typeof port !== 'number' ||
    typeof db !== 'string' ||
    !isSecret(webhookSecret) ||
    !isSecret(cronSecret) ||
    typeof stubUrl !== 'string'
  ) {
    throw new Error('the run state written by app.mjs is malformed');
  }
  return { port, db, webhookSecret, cronSecret, stubUrl };
}

function readReceiverState(value: unknown): ReceiverState {
  const { url, control } = (value ?? {}) as Partial<ReceiverState>;
  if (typeof url !== 'string' || !isSecret(control)) throw new Error('the run state written by app.mjs is malformed');
  return { url, control };
}

function readState(baseUrl: string): RunState {
  const port = Number(new URL(baseUrl).port);
  const state: unknown = JSON.parse(readFileSync(stateFile(port), 'utf8'));
  const { dbHost, dbPort, psql, secret, githubToken, otherToken, webhookKey, binary, e2eBinary, cron, receiver } = state as Partial<RunState>;
  if (
    typeof dbHost !== 'string' ||
    typeof dbPort !== 'number' ||
    typeof psql !== 'string' ||
    !isSecret(secret) ||
    !isSecret(githubToken) ||
    !isSecret(otherToken) ||
    !isSecret(webhookKey) ||
    typeof binary !== 'string' ||
    typeof e2eBinary !== 'string'
  ) {
    throw new Error('the run state written by app.mjs is malformed');
  }
  return { dbHost, dbPort, psql, secret, githubToken, otherToken, webhookKey, binary, e2eBinary, cron: readCronState(cron), receiver: readReceiverState(receiver) };
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
    const target = { baseUrl: appUrl, webhookSecret: state.secret, database: 'postgres', webhookKey: state.webhookKey };
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
    };
    return new Faucet(state, target, state.cron.cronSecret);
  }

  get binary(): string {
    return this.#state.binary;
  }

  // withFaucet starts one more faucet on its own database, runs fn against it and stops it. The new faucet has a random
  // CRON_SECRET and webhook mode off. env adds the rest. fn gets the faucet and a function that returns its
  // output so far. opts.e2eBuild runs the binary that was built with -tags faucet_e2e, whose callback client reaches loopback
  // over http.
  async withFaucet<T>(
    env: Record<string, string>,
    fn: (started: Faucet, output: () => string) => Promise<T>,
    opts: { e2eBuild?: boolean } = {},
  ): Promise<T> {
    const database = `faucet_${randomBytes(6).toString('hex')}`;
    const port = await freePort();
    const webhookSecret = randomBytes(32).toString('hex');
    const cronSecret = randomBytes(32).toString('hex');
    const { dbHost, dbPort, cron } = this.#state;
    const binary = opts.e2eBuild === true ? this.#state.e2eBinary : this.#state.binary;
    const databaseUrl = `postgres://postgres@/${database}?${new URLSearchParams({ host: dbHost, port: String(dbPort), sslmode: 'disable' })}`;

    await this.sql(`create database ${database}`);
    const child = spawn(binary, [], {
      env: {
        DATABASE_URL: databaseUrl,
        GITHUB_WEBHOOK_SECRET: webhookSecret,
        PORT: String(port),
        FAUCET_GITHUB_API_URL: cron.stubUrl,
        CRON_SECRET: cronSecret,
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
      const target = { baseUrl, webhookSecret, database, webhookKey: env.FAUCET_WEBHOOK_KEY ?? null };
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
  async cronJob(job: 'purge' | 'deliver', opts: CronOptions = {}): Promise<Reply> {
    const send = opts.send ?? ((secret: string): CronCredentials => ({ authorization: `Bearer ${secret}` }));
    const { authorization, query, body } = send(this.#cronSecret);
    const headers = new Headers();
    if (authorization !== undefined) headers.set('Authorization', authorization);
    return this.request(`/cron/${job}${query === undefined ? '' : `?${query}`}`, { method: opts.method ?? 'GET', headers, body });
  }

  // waitForEvents calls the wait_for_github_events tool on /mcp the way a
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
    const reply = await this.request('/mcp', { method: 'POST', headers, body });
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
    const reply = await this.request('/mcp', { method: 'POST', headers, body });
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
      select encode(key_hash, 'hex') as key_hash, principal, callback_url, event_type, arguments, repo_id,
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
    const { secret, githubToken, otherToken, webhookKey, cron, receiver } = this.#state;
    const secrets = [secret, githubToken, otherToken, webhookKey, cron.webhookSecret, cron.cronSecret, receiver.control];
    const own = [this.#target.webhookSecret, this.#cronSecret, ...(this.#target.webhookKey === null ? [] : [this.#target.webhookKey])];
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

export function appLog(): string {
  return readFileSync('.e2e/logs/app.log', 'utf8');
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

export const test = base.extend<{ faucet: Faucet; cron: Faucet; receiver: Receiver }>({
  faucet: async ({ app }, use) => {
    await use(Faucet.main(app.baseUrl));
  },
  cron: async ({ app }, use) => {
    await use(Faucet.cron(app.baseUrl));
  },
  receiver: async ({ app }, use) => {
    await use(new Receiver(app.baseUrl));
  },
});

export function newRepoId(): number {
  return randomInt(1_000_000, 2_000_000_000);
}

export function newDeliveryId(): string {
  return randomUUID();
}

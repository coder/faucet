import { execFile } from 'node:child_process';
import { createHmac, randomInt, randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { promisify } from 'node:util';
import { test as base } from '@e2e-dev/web';
import { stateFile } from '../runstate.mjs';

export { describe } from '@e2e-dev/web';
export { expect } from 'e2e';

const execFileAsync = promisify(execFile);

interface RunState {
  dbHost: string;
  dbPort: number;
  psql: string;
  secret: string;
  binary: string;
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

export interface Delivery {
  event?: string;
  delivery?: string;
  body: string;
  signature?: string | null;
}

function readState(baseUrl: string): RunState {
  const port = Number(new URL(baseUrl).port);
  const state: unknown = JSON.parse(readFileSync(stateFile(port), 'utf8'));
  const { dbHost, dbPort, psql, secret, binary } = state as Partial<RunState>;
  if (
    typeof dbHost !== 'string' ||
    typeof dbPort !== 'number' ||
    typeof psql !== 'string' ||
    typeof secret !== 'string' ||
    secret.length < 16 ||
    typeof binary !== 'string'
  ) {
    throw new Error('the run state written by app.mjs is malformed');
  }
  return { dbHost, dbPort, psql, secret, binary };
}

export class Faucet {
  readonly baseUrl: string;
  readonly #state: RunState;

  constructor(baseUrl: string) {
    this.baseUrl = baseUrl;
    this.#state = readState(baseUrl);
  }

  sign(body: string, secret: string = this.#state.secret): string {
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

  async sql(query: string): Promise<string> {
    const { stdout } = await execFileAsync(this.#state.psql, [
      '-X', '-A', '-t', '-q',
      '-h', this.#state.dbHost,
      '-p', String(this.#state.dbPort),
      '-U', 'postgres',
      '-d', 'postgres',
      '-v', 'ON_ERROR_STOP=1',
      '-c', query,
    ]);
    return stdout.trim();
  }

  leaksSecret(text: string): boolean {
    return text.includes(this.#state.secret);
  }
}

export const test = base.extend<{ faucet: Faucet }>({
  faucet: async ({ app }, use) => {
    await use(new Faucet(app.baseUrl));
  },
});

export function newRepoId(): number {
  return randomInt(1_000_000, 2_000_000_000);
}

export function newDeliveryId(): string {
  return randomUUID();
}

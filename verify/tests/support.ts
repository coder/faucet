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
  githubToken: string;
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
  name?: string | null;
  id?: number | string;
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
  const { dbHost, dbPort, psql, secret, githubToken, binary } = state as Partial<RunState>;
  if (
    typeof dbHost !== 'string' ||
    typeof dbPort !== 'number' ||
    typeof psql !== 'string' ||
    typeof secret !== 'string' ||
    secret.length < 16 ||
    typeof githubToken !== 'string' ||
    githubToken.length < 16 ||
    typeof binary !== 'string'
  ) {
    throw new Error('the run state written by app.mjs is malformed');
  }
  return { dbHost, dbPort, psql, secret, githubToken, binary };
}

export class Faucet {
  readonly baseUrl: string;
  readonly #state: RunState;

  constructor(baseUrl: string) {
    this.baseUrl = baseUrl;
    this.#state = readState(baseUrl);
  }

  get binary(): string {
    return this.#state.binary;
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
    const outcome: WaitOutcome = { ...reply, isError: false, text: '', events: null, cursor: null };
    if (reply.status !== 200) return outcome;

    const { result } = JSON.parse(reply.body) as {
      result: {
        isError?: boolean;
        content: { text: string }[];
        structuredContent?: { events: WaitedEvent[]; cursor: number };
      };
    };
    outcome.isError = result.isError === true;
    outcome.text = result.content[0]?.text ?? '';
    outcome.events = result.structuredContent?.events ?? null;
    outcome.cursor = result.structuredContent?.cursor ?? null;
    return outcome;
  }

  async mcp<R = Record<string, unknown>>(
    method: string,
    params: Record<string, unknown> = {},
    opts: McpOptions = {},
  ): Promise<McpReply<R>> {
    const protocolVersion = '2026-07-28';
    const token = opts.token === undefined ? this.#state.githubToken : opts.token;
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
    return text.includes(this.#state.secret) || text.includes(this.#state.githubToken);
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

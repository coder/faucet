import { runPayload } from './delivery';
import { samples } from './payloads';
import { expect, newDeliveryId, type Faucet, type McpPath } from './support';

const run = samples['workflow_run.completed'];
const SECRET = /^client_id: (?<id>[a-z0-9-]+)\nclient_secret: (?<secret>fcs_[A-Za-z0-9_-]{43})\n$/;

// Owner is the user or organization that a payload's repository names.
export interface Owner {
  id: number;
  login: string;
}

// VERIFY is the owner of the repositories verify/r<id> that the GitHub stub knows.
export const VERIFY: Owner = { id: 1, login: 'verify' };

// createClient makes a client as an operator does, with `faucet clients create`, and returns its secret. scope is a comma
// separated list.
export async function createClient(started: Faucet, id: string, path: McpPath, grants: string[], scope = 'events:read'): Promise<string> {
  const args = ['create', '-resource', `${started.baseUrl}${path}`, '-scope', scope, ...grants.flatMap((g) => ['-grant', g]), id];
  const made = await started.clients(args);
  expect(made.code, made.stderr).toBe(0);
  expect(made.stderr).toContain('cannot show it again');
  const secret = SECRET.exec(made.stdout)?.groups?.secret;
  expect(secret, 'the command prints the client ID and the secret once').toBeDefined();
  return secret as string;
}

// accessToken logs in as a client does, with its ID and secret in HTTP Basic and no scope or resource, and returns the token.
export async function accessToken(started: Faucet, id: string, secret: string): Promise<string> {
  const reply = await started.request('/oauth/token', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/x-www-form-urlencoded',
      Authorization: `Basic ${Buffer.from(`${id}:${secret}`).toString('base64')}`,
    },
    body: 'grant_type=client_credentials',
  });
  expect(reply.status, reply.body).toBe(200);
  const token = (JSON.parse(reply.body) as { access_token: string }).access_token;
  expect(token).toMatch(/^fct_[A-Za-z0-9_-]{43}$/);
  return token;
}

// deliverRun delivers a signed workflow_run whose payload names the repository in full, <owner login>/r<repoId>, with its
// owner, as GitHub's does. runId tells the events of a test apart in their data.
export async function deliverRun(started: Faucet, repoId: number, opts: { runId?: number; owner?: Owner } = {}): Promise<void> {
  const owner = opts.owner ?? VERIFY;
  const payload = opts.runId === undefined ? run.payload(repoId) : runPayload(repoId, opts.runId);
  const body = { ...payload, repository: { id: repoId, full_name: `${owner.login}/r${repoId}`, owner } };
  const stored = await started.deliver({ event: run.event, delivery: newDeliveryId(), body: JSON.stringify(body) });
  expect(stored.body).toBe('stored\n');
}

import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { ClientCredentialsProvider } from '@modelcontextprotocol/sdk/client/auth-extensions.js';
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type Faucet, type McpPath } from './support';

const run = samples['workflow_run.completed'];
const SECRET = /^client_id: (?<id>[a-z0-9-]+)\nclient_secret: (?<secret>fcs_[A-Za-z0-9_-]{43})\n$/;

interface WaitResult {
  isError?: boolean;
  content?: { type: string; text: string }[];
  structuredContent?: { events: { type: string; repo_id: number }[]; cursor: number };
}

// createClient makes a client as an operator does, with `faucet clients create`, and returns its secret.
async function createClient(started: Faucet, id: string, path: McpPath, grants: string[], scope = 'events:read'): Promise<string> {
  const args = ['create', '-resource', `${started.baseUrl}${path}`, '-scope', scope, ...grants.flatMap((g) => ['-grant', g]), id];
  const made = await started.clients(args);
  expect(made.code, made.stderr).toBe(0);
  expect(made.stderr).toContain('cannot show it again');
  const secret = SECRET.exec(made.stdout)?.groups?.secret;
  expect(secret, 'the command prints the client ID and the secret once').toBeDefined();
  return secret as string;
}

// deliverRun delivers a signed workflow_run whose payload names the repository in full, as GitHub's does.
async function deliverRun(started: Faucet, repoId: number): Promise<void> {
  const body = { ...run.payload(repoId), repository: { id: repoId, full_name: `verify/r${repoId}`, owner: { id: 1, login: 'verify' } } };
  const stored = await started.deliver({ event: run.event, delivery: newDeliveryId(), body: JSON.stringify(body) });
  expect(stored.body).toBe('stored\n');
}

function waited(stdout: string): WaitResult {
  return JSON.parse(stdout) as WaitResult;
}

describe('service accounts', () => {
  test('a service agent logs in with a client ID and secret alone and waits for an event', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started, output) => {
      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      const secret = await createClient(started, 'ci-watcher', '/github/mcp', [`repo:${repo}`]);

      const early = await started.serviceAgent('/github/mcp', 'ci-watcher', secret, { repo, types: ['ci.run_completed'] });
      expect(early.code, early.stderr).toBe(0);
      expect(waited(early.stdout).isError, 'no delivery has named the repository yet').toBe(true);
      expect(waited(early.stdout).content?.[0]?.text).toContain(`faucet has received no webhook delivery from ${repo} yet`);

      await deliverRun(started, repoId);
      const reply = await started.serviceAgent('/github/mcp', 'ci-watcher', secret, { repo, types: ['ci.run_completed'] });
      expect(reply.code, reply.stderr).toBe(0);
      const result = waited(reply.stdout);
      expect(result.isError, reply.stdout).not.toBe(true);
      expect(result.structuredContent?.events).toEqual([expect.objectContaining({ type: 'ci.run_completed', repo_id: repoId })]);

      const other = await started.serviceAgent('/github/mcp', 'ci-watcher', secret, { repo: 'verify/r1', types: ['ci.run_completed'] });
      expect(waited(other.stdout).content?.[0]?.text).toContain('no grant of this service names verify/r1');

      expect(output()).not.toContain(secret);
      expect(started.leaksSecret(output())).toBe(false);
    }, { publicUrl: true });
  });

  // The TypeScript SDK's discovery parses the metadata with a schema that needs authorization_endpoint, even for a client that
  // only uses client credentials.
  test('a TypeScript SDK client logs in with client credentials alone and waits for an event', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      const secret = await createClient(started, 'ts-agent', '/github/mcp', [`repo:${repo}`]);
      await deliverRun(started, repoId);

      const provider = new ClientCredentialsProvider({ clientId: 'ts-agent', clientSecret: secret, expectedIssuer: started.baseUrl });
      const transport = new StreamableHTTPClientTransport(new URL(`${started.baseUrl}/github/mcp`), { authProvider: provider });
      const client = new Client({ name: 'ts-service-agent', version: '0' });
      await client.connect(transport);
      try {
        const result = (await client.callTool({
          name: 'wait_for_github_events',
          arguments: { repo, types: ['ci.run_completed'], cursor: 0, timeout_s: 1 },
        })) as WaitResult;
        expect(result.isError, JSON.stringify(result)).not.toBe(true);
        expect(result.structuredContent?.events).toEqual([expect.objectContaining({ type: 'ci.run_completed', repo_id: repoId })]);
        expect(provider.tokens()?.access_token).toMatch(/^fct_[A-Za-z0-9_-]{43}$/);
      } finally {
        await client.close();
      }
    }, { publicUrl: true });
  });

  test('every 401 names the metadata of its path, and /mcp has its own', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      for (const path of ['/github/mcp', '/mcp'] as const) {
        const reply = await started.mcp('events/list', {}, { path, token: null });
        expect(reply.status).toBe(401);
        expect(reply.body).toBe('unauthorized\n');
        expect(reply.headers.get('www-authenticate')).toBe(
          `Bearer resource_metadata="${started.baseUrl}/.well-known/oauth-protected-resource${path}", scope="events:read events:subscribe"`,
        );
        const prm = await started.request(`/.well-known/oauth-protected-resource${path}`);
        expect(prm.status).toBe(200);
        expect(JSON.parse(prm.body)).toEqual({
          resource: `${started.baseUrl}${path}`,
          authorization_servers: [started.baseUrl],
          scopes_supported: ['events:read', 'events:subscribe'],
        });
      }
      const meta = await started.request('/.well-known/oauth-authorization-server');
      expect(meta.status).toBe(200);
      expect(JSON.parse(meta.body)).toMatchObject({
        issuer: started.baseUrl,
        authorization_endpoint: `${started.baseUrl}/oauth/authorize`,
        token_endpoint: `${started.baseUrl}/oauth/token`,
        response_types_supported: [],
        grant_types_supported: ['client_credentials'],
        token_endpoint_auth_methods_supported: ['client_secret_basic'],
      });

      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      const secret = await createClient(started, 'old-agent', '/mcp', [`repo:${repo}`]);
      await deliverRun(started, repoId);
      const alias = await started.serviceAgent('/mcp', 'old-agent', secret, { repo, types: ['ci.run_completed'] });
      expect(alias.code, alias.stderr).toBe(0);
      expect(waited(alias.stdout).structuredContent?.events).toHaveLength(1);
      const elsewhere = await started.serviceAgent('/github/mcp', 'old-agent', secret, { repo, types: ['ci.run_completed'] });
      expect(elsewhere.code, 'a client\'s tokens are for its one endpoint').toBe(1);
    }, { publicUrl: true });
  });

  test('a disabled client logs in no more, and the list says when it was disabled', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      const secret = await createClient(started, 'retired', '/github/mcp', [`repo:${repo}`]);
      await deliverRun(started, repoId);
      expect((await started.serviceAgent('/github/mcp', 'retired', secret, { repo, types: ['ci.run_completed'] })).code).toBe(0);

      const disabled = await started.clients(['disable', 'retired']);
      expect(disabled.code, disabled.stderr).toBe(0);
      expect(disabled.stdout).toContain('Its tokens fail on their next request');
      const refused = await started.serviceAgent('/github/mcp', 'retired', secret, { repo, types: ['ci.run_completed'] });
      expect(refused.code).toBe(1);
      expect(refused.stderr).toContain('invalid_client');

      const list = await started.clients(['list']);
      expect(list.code).toBe(0);
      expect(list.stdout).toMatch(/^retired\s+\S+\/github\/mcp\s+events:read\s+repo:verify\/r\d+\s+\S+Z\s+\S+Z$/m);
      expect(list.stdout).not.toContain(secret);
      const again = await started.clients(['create', '-resource', `${started.baseUrl}/github/mcp`, '-grant', `repo:${repo}`, 'retired']);
      expect(again.code, 'a disabled client keeps its ID').toBe(1);
    }, { publicUrl: true });
  });

  test('without FAUCET_PUBLIC_URL there is no login server, and no delivery writes a repository', async ({ faucet }) => {
    const repoId = newRepoId();
    await deliverRun(faucet, repoId);
    expect(await faucet.sql(`select count(*) from faucet_repos where repo_id = ${repoId}`)).toBe('0');

    for (const path of ['/.well-known/oauth-authorization-server', '/.well-known/oauth-protected-resource/github/mcp', '/.well-known/oauth-protected-resource/mcp', '/oauth/authorize']) {
      expect((await faucet.request(path)).status, path).toBe(404);
    }
    const token = await faucet.request('/oauth/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'grant_type=client_credentials',
    });
    expect(token.status).toBe(404);
  });
});

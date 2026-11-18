import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { ClientCredentialsProvider, StaticPrivateKeyJwtProvider } from '@modelcontextprotocol/sdk/client/auth-extensions.js';
import type { OAuthClientProvider } from '@modelcontextprotocol/sdk/client/auth.js';
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js';
import { accessToken, createClient, deliverRun } from './services';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';

interface WaitResult {
  isError?: boolean;
  content?: { type: string; text: string }[];
  structuredContent?: { events: { type: string; repo_id: number }[]; cursor: number };
}

function waited(stdout: string): WaitResult {
  return JSON.parse(stdout) as WaitResult;
}

// The SDK has no provider for the RFC 8693 token exchange grant, so this is its provider for a static client assertion with the
// assertion swapped for the subject token.
function tokenExchangeProvider(clientId: string, expectedIssuer: string, subjectToken: () => Promise<string>): OAuthClientProvider {
  return Object.assign(new StaticPrivateKeyJwtProvider({ clientId, expectedIssuer, jwtBearerAssertion: '' }), {
    addClientAuthentication: (_headers: Headers, params: URLSearchParams): void => params.set('client_id', clientId),
    prepareTokenRequest: async (scope?: string): Promise<URLSearchParams> => {
      const params = new URLSearchParams({
        grant_type: 'urn:ietf:params:oauth:grant-type:token-exchange',
        subject_token: await subjectToken(),
        subject_token_type: 'urn:ietf:params:oauth:token-type:jwt',
      });
      if (scope !== undefined) params.set('scope', scope);
      return params;
    },
  });
}

interface RawPoll {
  events: { eventId: string; name: string; data: Record<string, unknown> }[];
  cursor: string;
  truncated: boolean;
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

  test('a workload logs in through the TypeScript SDK with its platform token, waits for an event and lists events', async ({ faucet, issuer }) => {
    await faucet.withFaucet({}, async (started, output) => {
      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      const made = await started.clients(['create', '-no-secret', '-resource', `${started.baseUrl}/github/mcp`, '-grant', `repo:${repo}`, 'pod-agent']);
      expect(made.code, made.stderr).toBe(0);
      expect(made.stdout, 'no secret to keep').toBe('client_id: pod-agent\n');
      const sub = 'system:serviceaccount:apps:pod-agent';
      const trusted = await started.clients(['trust', 'pod-agent', '-issuer', issuer.url, '-claim', `sub=${sub}`]);
      expect(trusted.code, trusted.stderr).toBe(0);
      const ruleId = /^rule_id: (?<id>\d+)\n$/.exec(trusted.stdout)?.groups?.id;
      expect(ruleId, trusted.stdout).toBeDefined();
      await deliverRun(started, repoId);

      const subjects: string[] = [];
      const provider = tokenExchangeProvider('pod-agent', started.baseUrl, async () => {
        const subject = issuer.sign({ sub, aud: [started.baseUrl] });
        subjects.push(subject);
        return subject;
      });
      const transport = new StreamableHTTPClientTransport(new URL(`${started.baseUrl}/github/mcp`), { authProvider: provider });
      const client = new Client({ name: 'ts-workload', version: '0' });
      await client.connect(transport);
      let token: string;
      try {
        const result = (await client.callTool({
          name: 'wait_for_github_events',
          arguments: { repo, types: ['ci.run_completed'], cursor: 0, timeout_s: 1 },
        })) as WaitResult;
        expect(result.isError, JSON.stringify(result)).not.toBe(true);
        expect(result.structuredContent?.events).toEqual([expect.objectContaining({ type: 'ci.run_completed', repo_id: repoId })]);
        token = (await provider.tokens())?.access_token as string;
        expect(token).toMatch(/^fct_[A-Za-z0-9_-]{43}$/);
      } finally {
        await client.close();
      }
      expect(subjects, 'the SDK asked for one subject token for its one login').toHaveLength(1);
      expect(issuer.hits('/.well-known/openid-configuration'), 'faucet read the keys from the discovery document').toBe(1);
      expect(issuer.hits('/openid/v1/jwks')).toBe(1);

      const list = await started.mcp<{ events: { name: string }[] }>('events/list', {}, { token });
      expect(list.error, list.body).toBeNull();
      expect(list.result?.events.map((e) => e.name)).toContain('ci.run_completed');

      const untrusted = await started.clients(['untrust', 'pod-agent', ruleId as string]);
      expect(untrusted.code, untrusted.stderr).toBe(0);
      const fresh = issuer.sign({ sub, aud: [started.baseUrl] });
      subjects.push(fresh);
      const refused = await started.request('/oauth/token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          grant_type: 'urn:ietf:params:oauth:grant-type:token-exchange',
          client_id: 'pod-agent',
          subject_token: fresh,
          subject_token_type: 'urn:ietf:params:oauth:token-type:jwt',
        }).toString(),
      });
      expect(refused.status, refused.body).toBe(400);
      expect(refused.headers.get('www-authenticate'), 'the client sends no Authorization header').toBeNull();
      expect(JSON.parse(refused.body), 'the answer tells the caller nothing about the client').toEqual({
        error: 'invalid_client',
        error_description: "the client takes no such subject token: check client_id and the token, and faucet's log for the reason",
      });
      expect(output()).toContain('the client trusts no workload issuer: add one with faucet clients trust');

      for (const secret of [...subjects, token]) expect(output()).not.toContain(secret);
      expect(started.leaksSecret(output())).toBe(false);
    }, { publicUrl: true, e2eBuild: true });
  });

  // A service that reads only raw events can start. The push that nobody wants is not stored, and it still names the
  // repository, so the first poll after it opens the lease that makes faucet store the next push.
  test('a service agent polls the raw events of a granted repository once an ignored delivery named it', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started, output) => {
      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      const secret = await createClient(started, 'push-watcher', '/github/mcp', [`repo:${repo}`]);
      const token = await accessToken(started, 'push-watcher', secret);
      const poll = (cursor?: string) => started.mcp<RawPoll>('events/poll', { name: 'github.push', arguments: { repo }, cursor }, { token });
      const payload = { ref: 'refs/heads/main', repository: { id: repoId, full_name: repo, owner: { id: 1, login: 'verify' } } };
      const push = () => started.deliver({ event: 'push', delivery: newDeliveryId(), body: JSON.stringify(payload) });

      const early = await poll();
      expect(early.error?.code, early.body).toBe(-32024);
      expect(early.error?.message).toContain(`faucet has received no webhook delivery from ${repo} yet`);

      const unwanted = await push();
      expect(unwanted.status).toBe(202);
      expect(unwanted.body).toBe('ignored\n');
      expect(await started.sql(`select count(*) from faucet_raw_events where repo_id = ${repoId}`)).toBe('0');
      expect(await started.sql(`select count(*) from faucet_repos where repo_id = ${repoId}`), 'the ignored push named the repository').toBe('1');

      const opened = await poll();
      expect(opened.error, opened.body).toBeNull();
      expect(opened.result?.events).toEqual([]);
      const wanted = await push();
      expect(wanted.status).toBe(200);
      expect(wanted.body).toBe('stored\n');

      const page = await poll(opened.result?.cursor);
      expect(page.result?.events, page.body).toEqual([expect.objectContaining({ name: 'github.push', data: payload })]);
      expect(page.result?.truncated).toBe(false);

      expect(output()).not.toContain(secret);
      expect(output()).not.toContain(token);
      expect(started.leaksSecret(output())).toBe(false);
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
        grant_types_supported: ['client_credentials', 'urn:ietf:params:oauth:grant-type:token-exchange'],
        token_endpoint_auth_methods_supported: ['client_secret_basic', 'none'],
        revocation_endpoint_auth_methods_supported: ['client_secret_basic'],
      });

      const old = await started.clients(['create', '-resource', `${started.baseUrl}/mcp`, '-grant', 'repo:verify/r1', 'old-agent']);
      expect(old.code, '/mcp, the old path, takes no new client').toBe(1);
      expect(old.stderr).toContain('takes no new client');
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
      expect(list.stdout).toMatch(/^retired\s+\S+\/github\/mcp\s+events:read\s+repo:verify\/r\d+\s+\S+Z\s+\S+Z\s+yes\s+0$/m);
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

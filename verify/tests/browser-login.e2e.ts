import { createHash, randomBytes } from 'node:crypto';
import { samples } from './payloads';
import { appLog, describe, expect, newDeliveryId, newRepoId, test, type Faucet, type StubRequest } from './support';

const run = samples['workflow_run.completed'];
const AGENT = { client_name: 'Coding Agent', redirect_uris: ['http://127.0.0.1/callback'] };
// TOKEN matches a token of the stub's GitHub App (ghu_, ghr_) or of faucet's browser login (fct_, fcr_).
const TOKEN = /\b(?:ghu|ghr|fct|fcr)_[A-Za-z0-9_-]{20}/;

// deliverRun delivers a signed workflow_run whose payload names the repository in full, as GitHub's does.
async function deliverRun(started: Faucet, repoId: number): Promise<void> {
  const body = { ...run.payload(repoId), repository: { id: repoId, full_name: `verify/r${repoId}`, owner: { id: 1, login: 'verify' } } };
  const stored = await started.deliver({ event: run.event, delivery: newDeliveryId(), body: JSON.stringify(body) });
  expect(stored.body).toBe('stored\n');
}

// authorizationPath is the path and query of the authorization request of a client for the /github/mcp endpoint of started,
// with a PKCE challenge of verifier.
function authorizationPath(started: Faucet, clientId: string, redirectUri: string, state: string, verifier: string): string {
  return `/oauth/authorize?${new URLSearchParams({
    response_type: 'code',
    client_id: clientId,
    redirect_uri: redirectUri,
    state,
    code_challenge: createHash('sha256').update(verifier).digest('base64url'),
    code_challenge_method: 'S256',
    resource: `${started.baseUrl}/github/mcp`,
    scope: 'events:read',
  })}`;
}

// ContinueTimes is what the page records about its Continue button: whether it was disabled when the page had loaded, and the
// times of performance.now() when it appeared and when it was first enabled.
type ContinueTimes = { disabledAtLoad?: boolean; added?: number; enabled?: number };

// githubLogins are the requests to the stub's authorize page that send the browser back to the callback of started.
function githubLogins(requests: StubRequest[], started: Faucet): StubRequest[] {
  const callback = `${started.baseUrl}/oauth/github/callback`;
  return requests.filter((r) => r.path === '/login/oauth/authorize' && new URLSearchParams(r.query).get('redirect_uri') === callback);
}

describe('browser login', () => {
  test('an agent that knows only the endpoint and its client ID logs in through the consent page and GitHub, and waits for an event', async ({
    faucet,
    documents,
    hooks,
  }) => {
    await faucet.withFaucet({}, async (started, output) => {
      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      await deliverRun(started, repoId);
      const clientId = documents.add('agent', AGENT);

      const waited = await started.loginAgent('/github/mcp', clientId, { repo, types: ['ci.run_completed'] });
      const hidden = await started.loginAgent('/github/mcp', clientId, { repo: `hidden/r${repoId}`, types: ['ci.run_completed'] });

      const asked = (await hooks.requests()).filter((r) => r.path === `/repos/${repo}`);
      const known = [...waited.tokens, ...hidden.tokens, ...(await started.appLog()).tokens];
      expect(waited.code, waited.stderr).toBe(0);
      expect(waited.result?.isError, JSON.stringify(waited.result?.content)).not.toBe(true);
      expect(waited.result?.structuredContent?.events).toEqual([expect.objectContaining({ type: 'ci.run_completed', repo_id: repoId })]);
      expect(waited.logins).toBe(1);
      expect(asked.map((r) => r.auth), 'faucet asked GitHub with the GitHub token that it keeps for the person').toEqual(['person']);
      expect(hidden.code, hidden.stderr).toBe(0);
      expect(hidden.result?.isError, 'a repository that the person cannot read is refused').toBe(true);
      expect(hidden.result?.content?.[0]?.text).toContain('repository not found or not accessible');
      expect(output()).toMatch(/wait denied.*login=verify-person/);
      expect([waited.tokens, hidden.tokens].map((t) => t.filter((v) => TOKEN.test(v)).length), 'each login held an access and a refresh token').toEqual([2, 2]);
      for (const [name, text] of [['faucet output', output()], ['app log', appLog()]]) {
        expect(TOKEN.test(text), `${name} holds no token of the login`).toBe(false);
        expect(known.some((t) => text.includes(t)), `${name} holds no token that the test knows`).toBe(false);
        expect(started.leaksSecret(text), `${name} holds neither the App secret nor the token key`).toBe(false);
      }
    }, { browserLogin: true });
  });

  test('the connection keeps GitHub tokens sealed, and the request of the login ends redeemed', async ({ faucet, documents }) => {
    await faucet.withFaucet({}, async (started) => {
      const clientId = documents.add('sealed', AGENT);

      const agent = await started.loginAgent('/github/mcp', clientId, { repo: `verify/r${newRepoId()}`, types: ['ci.run_completed'] });

      const { tokens } = await started.appLog();
      const connections = JSON.parse(await started.sql(`select json_agg(t) from (select id, provider, upstream_user_id, login from faucet_connections) t`));
      const inClear = await started.sql(`select count(*) from faucet_connections c, unnest(array['${tokens.join("','")}']) as t(v)
        where strpos(c::text, t.v) > 0 or position(convert_to(t.v, 'UTF8') in c.access_token_enc) > 0
          or position(convert_to(t.v, 'UTF8') in c.refresh_token_enc) > 0`);
      const access = await started.connectionToken('github:4242', 'access');
      const refresh = await started.connectionToken('github:4242', 'refresh');
      const requests = JSON.parse(await started.sql(`select json_agg(t) from (select state, client_id, client_name, resource, connection_id from faucet_auth_requests) t`));
      expect(agent.code, agent.stderr).toBe(0);
      expect(connections).toEqual([{ id: 'github:4242', provider: 'github', upstream_user_id: 4242, login: 'verify-person' }]);
      expect(inClear, 'no GitHub token is in a row in clear').toBe('0');
      expect(access === tokens.at(-2) && refresh === tokens.at(-1), 'the sealed pair opens, with the token key, to the pair GitHub issued last').toBe(true);
      expect(requests).toEqual([
        { state: 'redeemed', client_id: clientId, client_name: 'Coding Agent', resource: `${started.baseUrl}/github/mcp`, connection_id: 'github:4242' },
      ]);
    }, { browserLogin: true });
  });

  test("the agent's session keeps working after a refresh, and the replaced refresh token is refused and ends the session", async ({
    faucet,
    documents,
  }) => {
    await faucet.withFaucet({}, async (started) => {
      const repoId = newRepoId();
      await deliverRun(started, repoId);
      const clientId = documents.add('refresher', AGENT);
      const before = (await started.appLog()).exchanges.length;

      const agent = await started.loginAgent('/github/mcp', clientId, { repo: `verify/r${repoId}`, types: ['ci.run_completed'], refresh: true });

      const exchanges = (await started.appLog()).exchanges.slice(before);
      const ended = await started.sql(`select ended_at is not null from faucet_user_sessions where client_id = '${clientId}'`);
      expect(agent.code, agent.stderr).toBe(0);
      expect(agent.refreshed?.isError, JSON.stringify(agent.refreshed?.content)).not.toBe(true);
      expect(agent.refreshed?.structuredContent?.events).toEqual([expect.objectContaining({ type: 'ci.run_completed', repo_id: repoId })]);
      expect(agent.logins, 'the agent went through the consent page once: the refresh worked').toBe(1);
      expect(agent.tokens.filter((t) => t.startsWith('fct_')).length, 'the refresh gave a new access token').toBe(2);
      expect(agent.reused).toBe('invalid_grant');
      expect(exchanges, "faucet's refresh grant also refreshed the GitHub token, which expires within its margin").toEqual([
        { grant: 'authorization_code', answer: 'issued' },
        { grant: 'refresh_token', answer: 'issued' },
      ]);
      expect(ended, 'the replaced refresh token ended the session').toBe('t');
    }, { browserLogin: true });
  });

  test('a person in a real browser sees who asks on the consent page, presses Continue, and ends on the redirect URI with a code', async ({
    faucet,
    documents,
    site,
    hooks,
    browser,
    screen,
  }) => {
    await faucet.withFaucet({}, async (started) => {
      const clientId = documents.add('browser-agent', AGENT);
      const redirectUri = site.url('/callback');
      site.serve('/callback', '<!doctype html><title>Logged in</title><h1>You can close this tab</h1>');
      const state = randomBytes(16).toString('hex');
      const verifier = randomBytes(32).toString('base64url');

      await browser.addInitScript(() => {
        const times: ContinueTimes = {};
        (globalThis as unknown as { continueTimes: ContinueTimes }).continueTimes = times;
        new MutationObserver(() => {
          const button = document.querySelector('button');
          if (button === null) return;
          times.added ??= performance.now();
          if (!button.disabled) times.enabled ??= performance.now();
        }).observe(document, { subtree: true, childList: true, attributes: true, attributeFilter: ['disabled'] });
        addEventListener('load', () => { times.disabledAtLoad = document.querySelector('button')?.disabled; });
      });

      await browser.goto(`${started.baseUrl}${authorizationPath(started, clientId, redirectUri, state, verifier)}`);
      await expect(screen.getByRole('heading', { name: new URL(clientId).host })).toBeVisible();
      await expect(screen.getByText('Coding Agent', { exact: true })).toBeVisible();
      const continueButton = screen.getByRole('button', { name: 'Continue with GitHub' });
      await expect(continueButton).toBeEnabled();
      const times = await browser.evaluate(() => {
        const { disabledAtLoad, added, enabled } = (globalThis as unknown as { continueTimes: ContinueTimes }).continueTimes;
        return { disabledAtLoad: disabledAtLoad ?? null, waited: added === undefined || enabled === undefined ? null : enabled - added };
      });
      await continueButton.click();
      await browser.waitForURL(new RegExp(`^${redirectUri}\\?`));

      const landed = new URL(await browser.url());
      const redeemed = await started.request('/oauth/token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          grant_type: 'authorization_code', client_id: clientId, code: landed.searchParams.get('code') ?? '', code_verifier: verifier, redirect_uri: redirectUri,
        }).toString(),
      });
      const answer = JSON.parse(redeemed.body) as Record<string, unknown>;
      expect(times.disabledAtLoad, 'Continue is off when the page has loaded').toBe(true);
      expect(times.waited, 'Continue turns on about 500 ms after it appears in a visible, focused page').toBeGreaterThanOrEqual(490);
      expect(landed.searchParams.get('state')).toBe(state);
      expect(landed.searchParams.get('iss')).toBe(started.baseUrl);
      expect(landed.searchParams.get('code')).toMatch(/^[A-Za-z0-9_-]{43}$/);
      expect(site.requests('/callback')).toHaveLength(1);
      expect(githubLogins(await hooks.requests(), started), 'the browser went through GitHub once').toHaveLength(1);
      expect(redeemed.status, 'the code that the browser brought back redeems').toBe(200);
      expect({ ...answer, access_token: TOKEN.test(String(answer.access_token)), refresh_token: TOKEN.test(String(answer.refresh_token)) }).toEqual({
        access_token: true, refresh_token: true, token_type: 'Bearer', expires_in: 900, scope: 'events:read',
      });
    }, { browserLogin: true });
  });

  // R1: a login that an attacker started, and whose consent form the attacker's page makes the person's browser post, must
  // not reach GitHub with the person's browser, or the attacker's redirect URI would get a code for the person's GitHub login.
  test("a page of another site that posts the consent form of the attacker's login gets a 403, and the login never reaches GitHub", async ({
    faucet,
    documents,
    site,
    hooks,
    browser,
    screen,
  }) => {
    await faucet.withFaucet({}, async (started, output) => {
      const stolen = site.url('/steal', 'localhost');
      const clientId = documents.add('attacker', { client_name: 'Coding Agent', redirect_uris: ['http://localhost/steal'] });
      const consent = await started.request(authorizationPath(started, clientId, stolen, 'attacker', randomBytes(32).toString('base64url')));
      const fields = Object.fromEntries([...consent.body.matchAll(/<input type="hidden" name="(request|token)" value="([^"]+)">/g)].map((m) => [m[1], m[2]]));
      site.serve('/', `<!doctype html><title>Free stickers</title>
<form method="post" action="${started.baseUrl}/oauth/authorize">
<input type="hidden" name="request" value="${fields.request}"><input type="hidden" name="token" value="${fields.token}">
</form><script>document.forms[0].submit()</script>`);
      const posted = browser.waitForResponse(`${started.baseUrl}/oauth/authorize`);

      await browser.goto(site.url('/', 'localhost'));

      const answer = await posted;
      await expect(screen.getByRole('heading', { name: 'Login stopped' })).toBeVisible();
      const state = await started.sql(`select state from faucet_auth_requests where id = '${fields.request}'`);
      expect(consent.status).toBe(200);
      expect(Object.keys(fields)).toEqual(['request', 'token']);
      expect(answer.status).toBe(403);
      expect(output(), 'the browser sent the Origin of the page that posted the form').toContain("the Origin header is not faucet's issuer");
      expect(githubLogins(await hooks.requests(), started)).toEqual([]);
      expect(site.requests('/steal'), "no code reached the attacker's redirect URI").toEqual([]);
      expect(state, 'the refused consent ended the login').toBe('expired');
    }, { browserLogin: true });
  });

  test('without the browser login there is no GitHub callback, and the metadata offers no authorization code', async ({ faucet }) => {
    const main = await faucet.request('/oauth/github/callback?state=s&code=c');
    const publicOnly = await faucet.withFaucet({}, async (started) => ({
      callback: (await started.request('/oauth/github/callback?state=s&code=c')).status,
      metadata: JSON.parse((await started.request('/.well-known/oauth-authorization-server')).body) as Record<string, unknown>,
    }), { publicUrl: true });
    const withLogin = await faucet.withFaucet({}, async (started) => (
      JSON.parse((await started.request('/.well-known/oauth-authorization-server')).body) as Record<string, unknown>
    ), { browserLogin: true });

    expect(main.status).toBe(404);
    expect(publicOnly.callback).toBe(404);
    expect(publicOnly.metadata.grant_types_supported).toEqual(['client_credentials']);
    expect(publicOnly.metadata).not.toHaveProperty('client_id_metadata_document_supported');
    expect(withLogin).toMatchObject({
      response_types_supported: ['code'],
      grant_types_supported: ['authorization_code', 'client_credentials', 'refresh_token'],
      code_challenge_methods_supported: ['S256'],
      client_id_metadata_document_supported: true,
      authorization_response_iss_parameter_supported: true,
    });
  });
});

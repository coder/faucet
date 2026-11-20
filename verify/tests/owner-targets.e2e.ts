import { counts, deliverDue, endpoint, eventRequests, expectSigned, withDeliveryFaucet, type Endpoint } from './delivery';
import { accessToken, createClient, deliverRun } from './services';
import { describe, expect, newRepoId, test } from './support';
import { EVENT, delivery, newMarker, subscribe, subscribed, type Params } from './subscriptions';

const SERVICE_ONLY = 'owner and all targets are for service clients; name one repository with repo';
const UNSEEN_LOGIN = 'faucet has received no webhook delivery from verify or a repository of it, so it cannot tell which account org:verify names';
const BOTH_SCOPES = 'events:read,events:subscribe';

function paramsAt(at: Endpoint, args: Params): Params {
  return { name: EVENT, arguments: args, delivery: { url: at.url, secret: at.secret } };
}

describe('owner targets', () => {
  // An org grant names the account that holds its login, so the client can be made only once a delivery named the owner.
  // The second repository has never sent one before the subscription exists. Both reach the one subscription, and an
  // event of another owner does not.
  test('a service with an org grant gets the events of every repository of the owner through one subscription', async ({ faucet, receiver }) => {
    await withDeliveryFaucet(faucet, async (started, output) => {
      const known = newRepoId();
      const fresh = newRepoId();
      const early = await started.clients(['create', '-resource', `${started.baseUrl}/github/mcp`, '-grant', 'org:verify', 'org-watcher']);
      expect(early.code, 'faucet cannot tell which account the login names yet').not.toBe(0);
      expect(early.stderr).toContain(UNSEEN_LOGIN);
      expect(early.stdout, 'a refused create prints no secret').toBe('');

      await deliverRun(started, known, { runId: 9000 });
      const secret = await createClient(started, 'org-watcher', '/github/mcp', ['org:verify'], BOTH_SCOPES);
      const token = await accessToken(started, 'org-watcher', secret);
      const everything = await endpoint(receiver, 'ok');
      const one = await endpoint(receiver, 'ok');
      const owned = await subscribed(started, paramsAt(everything, { owner: 'verify' }), { token });
      const narrowed = await subscribed(started, paramsAt(one, { owner: 'verify', repos: [`Verify/R${fresh}`] }), { token });
      const target = `select target_kind, owner_id, repo_id from faucet_subscriptions where position('${everything.id}' in callback_url) > 0`;
      expect(await started.sql(target), 'the row holds the owner by its GitHub ID').toBe('owner|1|0');

      await deliverRun(started, known, { runId: 9101 });
      await deliverRun(started, fresh, { runId: 9102 });
      await deliverRun(started, newRepoId(), { runId: 9103, owner: { id: 2, login: 'elsewhere' } });
      expect(await deliverDue(started)).toEqual(counts({ enqueued: 3, claimed: 3, sent: 3, delivered: 3 }));

      const all = (await eventRequests(receiver, everything)).map((request) => expectSigned(request, owned.id));
      expect(all.map((body) => body.data.run_id).sort(), 'the run before the subscribe and the run of the other owner are left out').toEqual([9101, 9102]);
      const named = (await eventRequests(receiver, one)).map((request) => expectSigned(request, narrowed.id));
      expect(named.map((body) => body.data.run_id), 'repos keeps the repository it names, ignoring case').toEqual([9102]);

      const waited = await started.waitForEvents({ owner: 'verify', types: [EVENT], cursor: 'v2.0', timeout_s: 1 }, token);
      expect(waited.isError, waited.text).toBe(false);
      expect(waited.events?.map((event) => event.repo_id)).toEqual([known, known, fresh]);
      const ownerLine = [...(await started.rows(known)), ...(await started.rows(fresh))].map((row) => row.owner_seq);
      expect(waited.events?.map((event) => event.id), 'an event of an owner target is named by its place on the owner\'s line').toEqual(ownerLine);

      const list = await started.clients(['list']);
      expect(list.stdout, 'the grant names the account that held the login').toMatch(/^org-watcher\s+\S+\/github\/mcp\s+events:read,events:subscribe\s+org:verify#1\s/m);
      expect(output()).not.toContain(secret);
      expect(output()).not.toContain(token);
      expect(started.leaksSecret(output())).toBe(false);
    });
  });

  test('an owner or all target is refused to the person of the browser login', async ({ faucet }) => {
    await withDeliveryFaucet(faucet, async (started) => {
      const person = started.as('person');
      const marker = newMarker();
      await deliverRun(started, newRepoId());
      for (const target of [{ owner: 'verify' }, { all: true }]) {
        const polled = await person.mcp('events/poll', { name: EVENT, arguments: target });
        expect(polled.error?.code, polled.body).toBe(-32024);
        expect(polled.error?.message).toContain(SERVICE_ONLY);

        const refused = await subscribe(person, { name: EVENT, arguments: target, delivery: delivery(marker) });
        expect(refused.error?.code, refused.body).toBe(-32024);
        expect(refused.error?.message).toContain(SERVICE_ONLY);

        const waited = await person.waitForEvents({ ...target, types: [EVENT], cursor: 'v2.0', timeout_s: 1 });
        expect(waited.isError, waited.text).toBe(true);
        expect(waited.text).toContain(SERVICE_ONLY);
      }
      expect(await started.subscriptions(marker)).toEqual([]);
    }, { browserLogin: true });
  });

  // The owner is known, so each refusal comes from the grants alone.
  test('a service whose grants do not cover the target is refused', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const repoId = newRepoId();
      await deliverRun(started, repoId);
      const repoOnly = await accessToken(started, 'one-repo', await createClient(started, 'one-repo', '/github/mcp', [`repo:verify/r${repoId}`]));
      const orgOnly = await accessToken(started, 'one-org', await createClient(started, 'one-org', '/github/mcp', ['org:verify']));
      const cases: [string, string, Params][] = [
        ['a repo grant, for its owner', repoOnly, { owner: 'verify' }],
        ['an org grant, for all', orgOnly, { all: true }],
        ['an org grant, for another owner', orgOnly, { owner: 'elsewhere' }],
      ];
      for (const [title, token, target] of cases) {
        const polled = await started.mcp('events/poll', { name: EVENT, arguments: target }, { token });
        expect(polled.error?.code, `${title}: ${polled.body}`).toBe(-32024);
        expect(polled.error?.message, title).not.toContain(SERVICE_ONLY);
      }
      const covered = await started.mcp('events/poll', { name: EVENT, arguments: { repo: `verify/r${repoId}` } }, { token: orgOnly });
      expect(covered.error, `an org grant covers a repository of its owner: ${covered.body}`).toBeNull();
    });
  });
});

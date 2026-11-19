import { randomBytes } from 'node:crypto';
import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test, type McpPath } from './support';

const paths: McpPath[] = ['/github/mcp', '/mcp'];
const run = samples['workflow_run.completed'];

interface ToolResult {
  isError?: boolean;
  structuredContent?: { events: { type: string; repo_id: number }[]; cursor: number };
}

describe('mcp paths', () => {
  for (const path of paths) {
    test(`${path} answers 401 with its challenge to no token and to a GitHub token, and never asks GitHub`, async ({ faucet, hooks }) => {
      await hooks.arm(faucet, []);
      const none = await faucet.mcp('events/list', {}, { path, token: null });
      const github = await faucet.mcp('events/list', {}, { path, token: `ghp_${randomBytes(20).toString('hex')}` });

      for (const reply of [none, github]) {
        expect(reply.status).toBe(401);
        expect(reply.body).toBe('unauthorized\n');
        expect(reply.headers.get('www-authenticate')).toBe(faucet.challenge(path));
      }
      expect(await hooks.requests(), 'faucet takes no GitHub token, so it sends none to GitHub').toEqual([]);
    });
  }

  // A service client is for /github/mcp only, so the person of the browser login, whose token can name /mcp, calls both.
  test('/mcp lists the same events as /github/mcp', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const person = started.as('person');
      const [github, alias] = await Promise.all(paths.map((path) => person.mcp('events/list', {}, { path })));

      expect(github.status).toBe(200);
      expect(github.result?.events).not.toHaveLength(0);
      expect(alias.status).toBe(200);
      expect(alias.result).toEqual(github.result);
    }, { browserLogin: true });
  });

  test('the wait tool returns the same stored event on both paths', async ({ faucet }) => {
    await faucet.withFaucet({}, async (started) => {
      const person = started.as('person');
      const repoId = newRepoId();
      const repo = `verify/r${repoId}`;
      const idle = await person.waitForEvents({ repo, types: ['ci.run_completed'], timeout_s: 1 });
      expect(idle.events).toEqual([]);
      const stored = await started.deliver({ event: run.event, delivery: newDeliveryId(), body: JSON.stringify(run.payload(repoId)) });
      expect(stored.body).toBe('stored\n');

      for (const path of paths) {
        const reply = await person.mcp<ToolResult>(
          'tools/call',
          { name: 'wait_for_github_events', arguments: { repo, types: ['ci.run_completed'], cursor: idle.cursor, timeout_s: 1 } },
          { path },
        );

        expect(reply.status, path).toBe(200);
        expect(reply.result?.isError, path).not.toBe(true);
        expect(reply.result?.structuredContent?.events, path).toEqual([expect.objectContaining({ type: 'ci.run_completed', repo_id: repoId })]);
      }
    }, { browserLogin: true });
  });
});

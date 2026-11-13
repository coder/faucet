import { samples } from './payloads';
import { describe, expect, newDeliveryId, newRepoId, test } from './support';

describe('ingest ignored', () => {
  const ignored: [string, string, (repoId: number) => object][] = [
    ['an event name faucet has no rule for', 'push', (repoId) => ({ ref: 'refs/heads/main', repository: { id: repoId } })],
    ['a pull_request opened action', 'pull_request', (repoId) => ({ action: 'opened', number: 7, repository: { id: repoId } })],
    ['a check_run created action', 'check_run', (repoId) => ({ action: 'created', repository: { id: repoId } })],
    ['a workflow_run requested action', 'workflow_run', (repoId) => ({ action: 'requested', repository: { id: repoId } })],
    [
      'a comment on a plain issue',
      'issue_comment',
      (repoId) => ({
        action: 'created',
        repository: { id: repoId },
        issue: { number: 7 },
        comment: { id: 1, user: { login: 'octocat' }, html_url: 'https://github.com/example/repo/issues/7#issuecomment-1' },
      }),
    ],
  ];

  for (const [title, event, payload] of ignored) {
    test(`${title} is acknowledged with 202 and stores nothing`, async ({ faucet }) => {
      const repoId = newRepoId();

      const reply = await faucet.deliver({ event, delivery: newDeliveryId(), body: JSON.stringify(payload(repoId)) });

      expect(reply.status).toBe(202);
      expect(reply.body).toBe('ignored\n');
      expect(await faucet.rows(repoId)).toHaveLength(0);
    });
  }

  test('a comment on a pull request is not ignored', async ({ faucet }) => {
    const sample = samples['issue_comment.created'];

    const reply = await faucet.deliver({ event: sample.event, delivery: newDeliveryId(), body: JSON.stringify(sample.payload(newRepoId())) });

    expect(reply.status).toBe(200);
  });
});

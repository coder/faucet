import { readFileSync } from 'node:fs';

const HEAD_SHA = '0123456789abcdef0123456789abcdef01234567';
const MERGE_SHA = 'fedcba9876543210fedcba9876543210fedcba98';
const BASE_SHA = '00112233445566778899aabbccddeeff00112233';
const PR = 7;
const FORK_PR = 99;
const SENDER = { login: 'octocat', id: 583231 };

function pullRequestRef(number: number, repoId: number, baseRepoId: number = repoId): object {
  return {
    number,
    head: { ref: 'feat/example', sha: HEAD_SHA, repo: { id: repoId } },
    base: { ref: 'main', sha: BASE_SHA, repo: { id: baseRepoId } },
  };
}

export interface Sample {
  event: string;
  payload: (repoId: number) => object;
  expected: {
    type: string;
    family: string;
    action: string;
    prs: number[];
    headSha: string | null;
    data: Record<string, unknown>;
  };
}

export function closedPullRequest(repoId: number): object {
  return {
    action: 'closed',
    number: PR,
    repository: { id: repoId },
    pull_request: {
      html_url: `https://github.com/example/repo/pull/${PR}`,
      title: 'Add an example',
      state: 'closed',
      draft: false,
      merged: true,
      merge_commit_sha: MERGE_SHA,
      user: { login: 'author', id: 3 },
      head: { ref: 'feat/example', sha: HEAD_SHA },
      base: { ref: 'main', sha: BASE_SHA },
    },
    sender: { login: 'octocat', id: 1 },
  };
}

export const samples: Record<string, Sample> = {
  'workflow_run.completed': {
    event: 'workflow_run',
    payload: (repoId) => ({
      action: 'completed',
      repository: { id: repoId },
      sender: SENDER,
      workflow_run: {
        id: 9001,
        name: 'ci',
        status: 'completed',
        conclusion: 'success',
        head_branch: 'feat/example',
        head_sha: HEAD_SHA,
        event: 'pull_request',
        html_url: 'https://github.com/example/repo/actions/runs/9001',
        run_attempt: 2,
        workflow_id: 9100,
        pull_requests: [pullRequestRef(PR, repoId), pullRequestRef(FORK_PR, repoId, repoId + 1)],
      },
    }),
    expected: {
      type: 'ci.run_completed',
      family: 'ci.runs',
      action: 'completed',
      prs: [PR],
      headSha: HEAD_SHA,
      data: {
        run_id: 9001,
        name: 'ci',
        status: 'completed',
        conclusion: 'success',
        head_branch: 'feat/example',
        trigger_event: 'pull_request',
        html_url: 'https://github.com/example/repo/actions/runs/9001',
        run_attempt: 2,
        workflow_id: 9100,
        sender_login: SENDER.login,
        sender_id: SENDER.id,
      },
    },
  },
  'check_run.completed': {
    event: 'check_run',
    payload: (repoId) => ({
      action: 'completed',
      repository: { id: repoId },
      sender: SENDER,
      check_run: {
        id: 9002,
        name: 'lint',
        status: 'completed',
        conclusion: 'failure',
        head_sha: HEAD_SHA,
        html_url: 'https://github.com/example/repo/runs/9002',
        pull_requests: [
          pullRequestRef(PR, repoId),
          pullRequestRef(PR + 1, repoId),
          pullRequestRef(FORK_PR, repoId, repoId + 1),
        ],
        app: { slug: 'github-actions' },
        check_suite: { id: 9200, head_branch: 'feat/example' },
      },
    }),
    expected: {
      type: 'ci.check_completed',
      family: 'ci.checks',
      action: 'completed',
      prs: [PR, PR + 1],
      headSha: HEAD_SHA,
      data: {
        check_run_id: 9002,
        name: 'lint',
        status: 'completed',
        conclusion: 'failure',
        html_url: 'https://github.com/example/repo/runs/9002',
        app_slug: 'github-actions',
        check_suite_id: 9200,
        head_branch: 'feat/example',
        sender_login: SENDER.login,
        sender_id: SENDER.id,
      },
    },
  },
  'pull_request_review.submitted': {
    event: 'pull_request_review',
    payload: (repoId) => ({
      action: 'submitted',
      repository: { id: repoId },
      review: {
        id: 9003,
        state: 'approved',
        user: { login: 'octocat' },
        html_url: `https://github.com/example/repo/pull/${PR}#pullrequestreview-9003`,
      },
      pull_request: { number: PR, head: { sha: HEAD_SHA } },
      sender: { login: 'octocat', id: 1 },
    }),
    expected: {
      type: 'pr.review_submitted',
      family: 'pr.reviews',
      action: 'submitted',
      prs: [PR],
      headSha: HEAD_SHA,
      data: {
        review_id: 9003,
        state: 'approved',
        user_login: 'octocat',
        html_url: `https://github.com/example/repo/pull/${PR}#pullrequestreview-9003`,
        sender_login: 'octocat',
        sender_id: 1,
      },
    },
  },
  'pull_request.closed': {
    event: 'pull_request',
    payload: closedPullRequest,
    expected: {
      type: 'pr.closed',
      family: 'pr.activity',
      action: 'closed',
      prs: [PR],
      headSha: HEAD_SHA,
      data: {
        merged: true,
        merge_commit_sha: MERGE_SHA,
        html_url: `https://github.com/example/repo/pull/${PR}`,
        title: 'Add an example',
        state: 'closed',
        draft: false,
        base_branch: 'main',
        head_branch: 'feat/example',
        author_login: 'author',
        sender_login: 'octocat',
        sender_id: 1,
      },
    },
  },
  'issue_comment.created': {
    event: 'issue_comment',
    payload: (repoId) => ({
      action: 'created',
      repository: { id: repoId },
      issue: { number: PR, pull_request: {} },
      comment: {
        id: 9005,
        user: { login: 'octocat' },
        html_url: `https://github.com/example/repo/pull/${PR}#issuecomment-9005`,
      },
      sender: { login: 'octocat', id: 1 },
    }),
    expected: {
      type: 'pr.comment_created',
      family: 'pr.comments',
      action: 'created',
      prs: [PR],
      headSha: null,
      data: {
        comment_id: 9005,
        user_login: 'octocat',
        html_url: `https://github.com/example/repo/pull/${PR}#issuecomment-9005`,
        sender_login: 'octocat',
        sender_id: 1,
      },
    },
  },
};

const RECORDED_SHA = 'ec26c3e57ca3a959ca5aad62de7213c562f8c821';
const RECORDED_PR = {
  html_url: 'https://github.com/Codertocat/Hello-World/pull/2',
  title: 'Update the README with new information.',
  state: 'open',
  draft: false,
  merged: false,
  base_branch: 'master',
  head_branch: 'changes',
  author_login: 'Codertocat',
  sender_login: 'Codertocat',
  sender_id: 21031067,
};

// recordedSamples are webhooks of actions that no exact type of events/list
// names. They are recorded payloads from faucet/testdata, so they are polled
// by their family.
export const recordedSamples: Record<string, Sample> = {
  'pull_request.opened': {
    event: 'pull_request',
    payload: (repoId) => recorded('pull_request_opened', repoId),
    expected: { type: 'pr.opened', family: 'pr.activity', action: 'opened', prs: [2], headSha: RECORDED_SHA, data: RECORDED_PR },
  },
  'pull_request.synchronize': {
    event: 'pull_request',
    payload: (repoId) => recorded('pull_request_synchronize', repoId),
    expected: {
      type: 'pr.synchronize',
      family: 'pr.activity',
      action: 'synchronize',
      prs: [2],
      headSha: RECORDED_SHA,
      data: { ...RECORDED_PR, before: 'f95f852bd8fca8fcc58a9a2d6c842781e32a215e', after: RECORDED_SHA },
    },
  },
  'pull_request_review.dismissed': {
    event: 'pull_request_review',
    payload: (repoId) => recorded('pull_request_review_dismissed', repoId),
    expected: {
      type: 'pr.review_dismissed',
      family: 'pr.reviews',
      action: 'dismissed',
      prs: [2],
      headSha: RECORDED_SHA,
      data: {
        review_id: 237895671,
        state: 'dismissed',
        user_login: 'Codertocat',
        html_url: 'https://github.com/Codertocat/Hello-World/pull/2#pullrequestreview-237895671',
        sender_login: 'Codertocat',
        sender_id: 21031067,
      },
    },
  },
  'issue_comment.deleted': {
    event: 'issue_comment',
    payload: (repoId) => recorded('issue_comment_deleted_on_pr', repoId),
    expected: {
      type: 'pr.comment_deleted',
      family: 'pr.comments',
      action: 'deleted',
      prs: [1],
      headSha: null,
      data: {
        comment_id: 492700400,
        user_login: 'Codertocat',
        html_url: 'https://github.com/Codertocat/Hello-World/issues/1#issuecomment-492700400',
        sender_login: 'Codertocat',
        sender_id: 21031067,
      },
    },
  },
  'pull_request_review_comment.created': {
    event: 'pull_request_review_comment',
    payload: (repoId) => recorded('pull_request_review_comment_created', repoId),
    expected: {
      type: 'pr.review_comment_created',
      family: 'pr.review_comments',
      action: 'created',
      prs: [2],
      headSha: RECORDED_SHA,
      data: {
        comment_id: 284312630,
        review_id: 237895671,
        user_login: 'Codertocat',
        html_url: 'https://github.com/Codertocat/Hello-World/pull/2#discussion_r284312630',
        sender_login: 'Codertocat',
        sender_id: 21031067,
      },
    },
  },
  'pull_request_review_thread.resolved': {
    event: 'pull_request_review_thread',
    payload: (repoId) => recorded('pull_request_review_thread_resolved', repoId),
    expected: {
      type: 'pr.review_thread_resolved',
      family: 'pr.review_threads',
      action: 'resolved',
      prs: [2],
      headSha: RECORDED_SHA,
      data: {
        thread_node_id: 'PRRT_kwDOFd42Pc4rQOUv',
        comment_ids: [284312630],
        user_login: 'Codertocat',
        html_url: 'https://github.com/Codertocat/Hello-World/pull/2#discussion_r284312630',
        sender_login: 'Codertocat',
        sender_id: 21031067,
      },
    },
  },
};

// dataOf is the data of an event of the sample, as events/poll and webhook delivery send it.
export function dataOf(sample: Sample): Record<string, unknown> {
  const { type, action, prs, headSha, data } = sample.expected;
  const sent = { ...data, prs, event: type, action };
  return headSha === null ? sent : { ...sent, head_sha: headSha };
}

// recorded is a GitHub webhook payload that octokit/webhooks recorded, from faucet/testdata, sent for repository repoId.
export function recorded(name: string, repoId: number): object {
  const payload = JSON.parse(readFileSync(new URL(`../../faucet/testdata/${name}.json`, import.meta.url), 'utf8')) as {
    repository: { id: number };
  };
  payload.repository.id = repoId;
  return payload;
}

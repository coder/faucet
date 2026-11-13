const HEAD_SHA = '0123456789abcdef0123456789abcdef01234567';
const MERGE_SHA = 'fedcba9876543210fedcba9876543210fedcba98';
const BASE_SHA = '00112233445566778899aabbccddeeff00112233';
const PR = 7;
const FORK_PR = 99;

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
      merged: true,
      merge_commit_sha: MERGE_SHA,
      html_url: `https://github.com/example/repo/pull/${PR}`,
      head: { sha: HEAD_SHA },
    },
  };
}

export const samples: Record<string, Sample> = {
  'workflow_run.completed': {
    event: 'workflow_run',
    payload: (repoId) => ({
      action: 'completed',
      repository: { id: repoId },
      workflow_run: {
        id: 9001,
        name: 'ci',
        conclusion: 'success',
        head_branch: 'feat/example',
        head_sha: HEAD_SHA,
        html_url: 'https://github.com/example/repo/actions/runs/9001',
        run_attempt: 2,
        pull_requests: [pullRequestRef(PR, repoId), pullRequestRef(FORK_PR, repoId, repoId + 1)],
      },
    }),
    expected: {
      type: 'ci.run_completed',
      prs: [PR],
      headSha: HEAD_SHA,
      data: {
        run_id: 9001,
        name: 'ci',
        conclusion: 'success',
        head_branch: 'feat/example',
        html_url: 'https://github.com/example/repo/actions/runs/9001',
        run_attempt: 2,
      },
    },
  },
  'check_run.completed': {
    event: 'check_run',
    payload: (repoId) => ({
      action: 'completed',
      repository: { id: repoId },
      check_run: {
        id: 9002,
        name: 'lint',
        conclusion: 'failure',
        head_sha: HEAD_SHA,
        html_url: 'https://github.com/example/repo/runs/9002',
        pull_requests: [
          pullRequestRef(PR, repoId),
          pullRequestRef(PR + 1, repoId),
          pullRequestRef(FORK_PR, repoId, repoId + 1),
        ],
        app: { slug: 'github-actions' },
      },
    }),
    expected: {
      type: 'ci.check_completed',
      prs: [PR, PR + 1],
      headSha: HEAD_SHA,
      data: {
        check_run_id: 9002,
        name: 'lint',
        conclusion: 'failure',
        html_url: 'https://github.com/example/repo/runs/9002',
        app_slug: 'github-actions',
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
    }),
    expected: {
      type: 'pr.review_submitted',
      prs: [PR],
      headSha: HEAD_SHA,
      data: {
        review_id: 9003,
        state: 'approved',
        user_login: 'octocat',
        html_url: `https://github.com/example/repo/pull/${PR}#pullrequestreview-9003`,
      },
    },
  },
  'pull_request.closed': {
    event: 'pull_request',
    payload: closedPullRequest,
    expected: {
      type: 'pr.closed',
      prs: [PR],
      headSha: HEAD_SHA,
      data: {
        merged: true,
        merge_commit_sha: MERGE_SHA,
        html_url: `https://github.com/example/repo/pull/${PR}`,
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
    }),
    expected: {
      type: 'pr.comment_created',
      prs: [PR],
      headSha: null,
      data: {
        comment_id: 9005,
        user_login: 'octocat',
        html_url: `https://github.com/example/repo/pull/${PR}#issuecomment-9005`,
      },
    },
  },
};

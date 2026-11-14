import { childEnv } from '../env.mjs';
import { describe, expect, test } from './support';

describe('harness environment', () => {
  test('a harness child gets the system and Go variables of the host and no credential', () => {
    const kept = {
      PATH: '/usr/bin',
      HOME: '/home/dev',
      TMPDIR: '/tmp',
      GOROOT: '/usr/local/go',
      GOPATH: '/home/dev/go',
      GOCACHE: '/home/dev/.cache/go-build',
      GOMODCACHE: '/home/dev/go/pkg/mod',
      GOFLAGS: '-mod=mod',
      GOTOOLCHAIN: 'local',
    };
    const dropped = {
      GOOGLE_APPLICATION_CREDENTIALS: '/home/dev/key.json',
      GOOGLE_API_KEY: 'not-a-real-key',
      GITHUB_TOKEN: 'not-a-real-token',
      AWS_SECRET_ACCESS_KEY: 'not-a-real-key',
      DATABASE_URL: 'postgres://user:not-a-real-password@db/prod',
    };

    expect(childEnv({ ...kept, ...dropped })).toEqual(kept);
  });
});

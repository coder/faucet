import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { endGroup } from '../group.mjs';
import { describe, expect, test } from './support';

describe('ending a process group', () => {
  test('a group that ends right after the first call to kill is gone, not an error', async () => {
    const realKill = process.kill;
    let calls = 0;
    process.kill = ((): true => {
      calls += 1;
      if (calls > 1) throw Object.assign(new Error('kill ESRCH'), { code: 'ESRCH' });
      return true;
    }) as typeof process.kill;
    try {
      await endGroup(4_000_000);
    } finally {
      process.kill = realKill;
    }

    expect(calls).toBeGreaterThan(0);
  });

  test('a group that is already gone is not an error', async () => {
    const leader = spawn(process.execPath, ['-e', ''], { detached: true, stdio: 'ignore' });
    await once(leader, 'exit');

    await endGroup(leader.pid as number);
  });
});

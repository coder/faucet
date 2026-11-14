import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { endGroup } from '../group.mjs';
import { describe, expect, test } from './support';

// A leader and a child in one process group. Both run until a signal ends them.
const GROUP_SCRIPT = `
const { spawn } = require('node:child_process');
const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: 'ignore' });
console.log(child.pid);
setInterval(() => {}, 1000);
`;

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ESRCH') return false;
    throw err;
  }
}

describe('ending a process group', () => {
  test('a group that ends under SIGINT is gone, and the SIGKILL that finds nobody is not an error', async () => {
    const leader = spawn(process.execPath, ['-e', GROUP_SCRIPT], { detached: true, stdio: ['ignore', 'pipe', 'ignore'] });
    const pgid = leader.pid as number;
    try {
      const [chunk] = (await once(leader.stdout, 'data')) as [Buffer];
      const child = Number(chunk.toString().trim());
      expect(alive(child), 'the group has a second process besides its leader').toBe(true);

      await endGroup(pgid);

      expect(alive(-pgid), 'no process of the group is left').toBe(false);
      expect(alive(child), 'the child is gone with its group').toBe(false);
    } finally {
      try {
        process.kill(-pgid, 'SIGKILL');
      } catch {
        // the group is already gone
      }
    }
  });

  test('a group that is already gone is not an error', async () => {
    const leader = spawn(process.execPath, ['-e', ''], { detached: true, stdio: 'ignore' });
    await once(leader, 'exit');

    await endGroup(leader.pid as number);

    expect(alive(-(leader.pid as number)), 'there is still no group').toBe(false);
  });
});

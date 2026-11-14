import { setTimeout as sleep } from 'node:timers/promises';

const GROUP_STOP_MS = 10_000;

// kill reports ESRCH once no process of the group is left, and a group can end between any two calls.
function signalGroup(pgid, signal) {
  try {
    process.kill(-pgid, signal);
    return true;
  } catch (err) {
    if (err.code === 'ESRCH') return false;
    throw err;
  }
}

// Postgres treats SIGINT as a fast shutdown, so the group gets it first and SIGKILL only when something ignores it.
export async function endGroup(pgid) {
  if (!signalGroup(pgid, 'SIGINT')) return;
  const deadline = Date.now() + GROUP_STOP_MS;
  while (signalGroup(pgid, 0) && Date.now() < deadline) await sleep(100);
  signalGroup(pgid, 'SIGKILL');
}

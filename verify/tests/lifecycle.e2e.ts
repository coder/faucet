import { spawn, type ChildProcess } from 'node:child_process';
import { closeSync, existsSync, mkdirSync, openSync, readFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { runDir } from '../runstate.mjs';
import { describe, expect, test } from './support';

const READY_MS = 120_000;
const TEARDOWN_MS = 20_000;
const DETACH_GRACE_MS = 4_000;

const PARENT_SCRIPT = `
const { spawn } = require('node:child_process');
const child = spawn(process.execPath, process.argv.slice(1), { stdio: ['ignore', 'ignore', 'inherit'] });
console.log(child.pid);
const runner = process.ppid;
setInterval(() => process.ppid === runner || process.exit(0), 1000);
`;

function freePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const probe = createServer();
    probe.once('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const { port } = probe.address() as { port: number };
      probe.close(() => resolvePort(port));
    });
  });
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function healthy(port: number): Promise<boolean> {
  try {
    return (await fetch(`http://127.0.0.1:${port}/healthz`)).status === 200;
  } catch {
    return false;
  }
}

async function waitFor(what: string, ms: number, done: () => boolean | Promise<boolean>): Promise<void> {
  const deadline = Date.now() + ms;
  while (!(await done())) {
    if (Date.now() > deadline) throw new Error(`timed out after ${ms} ms waiting for ${what}`);
    await sleep(100);
  }
}

class Stack {
  readonly port: number;
  readonly dir: string;
  readonly parent: ChildProcess;
  readonly appPid: number;

  private constructor(port: number, parent: ChildProcess, appPid: number) {
    this.port = port;
    this.dir = runDir(port);
    this.parent = parent;
    this.appPid = appPid;
  }

  static async launch(extraArgs: string[]): Promise<Stack> {
    const port = await freePort();
    mkdirSync('.e2e/logs', { recursive: true });
    const log = openSync(`.e2e/logs/lifecycle-${port}.log`, 'w');
    const parent = spawn(
      process.execPath,
      ['-e', PARENT_SCRIPT, resolve('app.mjs'), '--port', String(port), ...extraArgs],
      { stdio: ['ignore', 'pipe', log] },
    );
    closeSync(log);
    const firstLine = await new Promise<string>((resolveLine, reject) => {
      parent.once('error', reject);
      parent.stdout?.once('data', (chunk: Buffer) => resolveLine(chunk.toString()));
    });
    const appPid = Number(firstLine.trim());
    if (!Number.isInteger(appPid) || appPid < 2) throw new Error('the parent did not report the app pid');
    return new Stack(port, parent, appPid);
  }

  ready(): Promise<void> {
    return waitFor('the stack to answer /healthz', READY_MS, () => healthy(this.port));
  }

  postgresPid(): number {
    const pid = Number(readFileSync(`${this.dir}/pgdata/postmaster.pid`, 'utf8').split('\n')[0]);
    if (!Number.isInteger(pid) || pid < 2) throw new Error('postmaster.pid does not name a process');
    return pid;
  }

  async teardown(): Promise<void> {
    this.parent.kill('SIGKILL');
    if (existsSync(this.dir) && alive(this.appPid)) process.kill(this.appPid, 'SIGTERM');
    await waitFor('the stack directory to go', TEARDOWN_MS, () => !existsSync(this.dir)).catch(() => undefined);
  }
}

describe('app lifecycle', () => {
  test('SIGKILL of the parent tears faucet, Postgres and the run directory down', async () => {
    const stack = await Stack.launch([]);
    try {
      await stack.ready();
      const postgres = stack.postgresPid();

      stack.parent.kill('SIGKILL');
      await waitFor('the run directory to go', TEARDOWN_MS, () => !existsSync(stack.dir));
      await waitFor('app.mjs and Postgres to exit', TEARDOWN_MS, () => !alive(stack.appPid) && !alive(postgres));

      expect(await healthy(stack.port)).toBe(false);
    } finally {
      await stack.teardown();
    }
  });

  test('SIGKILL of the parent while initdb runs still removes the run directory', async () => {
    const stack = await Stack.launch([]);
    try {
      await waitFor('initdb to start', READY_MS, () => existsSync(`${stack.dir}/pgdata`));

      stack.parent.kill('SIGKILL');
      await waitFor('the run directory to go', TEARDOWN_MS, () => !existsSync(stack.dir));
      await waitFor('app.mjs to exit', TEARDOWN_MS, () => !alive(stack.appPid));

      expect(await healthy(stack.port)).toBe(false);
    } finally {
      await stack.teardown();
    }
  });

  test('--detach keeps the stack running after its parent dies and SIGTERM then tears it down', async () => {
    const stack = await Stack.launch(['--detach']);
    try {
      await stack.ready();
      const postgres = stack.postgresPid();

      stack.parent.kill('SIGKILL');
      await sleep(DETACH_GRACE_MS);
      expect(alive(stack.appPid)).toBe(true);
      expect(await healthy(stack.port)).toBe(true);

      process.kill(stack.appPid, 'SIGTERM');
      await waitFor('the run directory to go', TEARDOWN_MS, () => !existsSync(stack.dir));
      await waitFor('app.mjs and Postgres to exit', TEARDOWN_MS, () => !alive(stack.appPid) && !alive(postgres));
      expect(await healthy(stack.port)).toBe(false);
    } finally {
      await stack.teardown();
    }
  });
});

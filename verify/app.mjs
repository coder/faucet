#!/usr/bin/env node
import { execFile, spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { existsSync, mkdirSync, readdirSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { delimiter, dirname, join, resolve } from 'node:path';
import { parseArgs, promisify } from 'node:util';
import { runDir, stateFile } from './runstate.mjs';

const execFileAsync = promisify(execFile);
const FAUCET_STOP_MS = 20_000;
const DB_PORT = 5432;
const SOCKET_PATH_MAX = 100;
const PARENT_POLL_MS = 1000;

function log(message) {
  console.error(`faucet-verify: ${message}`);
}

function hasServerBinaries(dir) {
  return ['initdb', 'pg_ctl', 'postgres'].every((name) => existsSync(join(dir, name)));
}

function findPostgresBin() {
  if (process.env.PG_BIN) {
    if (!hasServerBinaries(process.env.PG_BIN)) {
      throw new Error('PG_BIN must name a directory holding initdb, pg_ctl and postgres');
    }
    return process.env.PG_BIN;
  }
  const candidates = [];
  for (const dir of (process.env.PATH ?? '').split(delimiter)) {
    const initdb = join(dir, 'initdb');
    if (existsSync(initdb)) candidates.push(dirname(realpathSync(initdb)));
  }
  const installed = '/usr/lib/postgresql';
  if (existsSync(installed)) {
    const versions = readdirSync(installed).sort((a, b) => Number(b) - Number(a));
    for (const version of versions) candidates.push(join(installed, version, 'bin'));
  }
  const found = candidates.find(hasServerBinaries);
  if (!found) {
    throw new Error('no PostgreSQL server binaries found; install PostgreSQL or set PG_BIN');
  }
  return found;
}

async function startPostgres(bin, dir, signal) {
  await execFileAsync(
    join(bin, 'pg_ctl'),
    [
      'start', '-D', join(dir, 'pgdata'), '-w', '-t', '60', '-l', join(dir, 'postgres.log'),
      '-o', `-p ${DB_PORT} -k '${dir}' -c listen_addresses='' -c unix_socket_permissions=0700 -c fsync=off`,
    ],
    { signal },
  );
}

async function stopPostgres(bin, dir) {
  try {
    await execFileAsync(join(bin, 'pg_ctl'), ['stop', '-D', join(dir, 'pgdata'), '-m', 'fast', '-w', '-t', '30']);
    return true;
  } catch (err) {
    log(`postgres did not stop: ${err.message}`);
    return false;
  }
}

async function stopFaucet(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = once(child, 'exit');
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), FAUCET_STOP_MS);
  await exited;
  clearTimeout(timer);
}

function stopWhenParentExits(abort) {
  const parent = process.ppid;
  setInterval(() => {
    if (process.ppid === parent) return;
    log('parent process exited, shutting down');
    abort.abort();
  }, PARENT_POLL_MS).unref();
}

function waitForFaucetOrAbort(child, signal) {
  return new Promise((resolveOutcome) => {
    if (signal.aborted) return resolveOutcome({ aborted: true });
    signal.addEventListener('abort', () => resolveOutcome({ aborted: true }), { once: true });
    child.once('error', (err) => resolveOutcome({ error: err.message }));
    child.once('exit', (code, sig) => resolveOutcome({ error: `faucet exited early (${sig ?? code})` }));
  });
}

async function main(abort) {
  const { signal } = abort;
  const { values } = parseArgs({ options: { port: { type: 'string' }, detach: { type: 'boolean' } } });
  const port = Number(values.port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('--port must be an integer from 1 to 65535');
  }
  if (!values.detach) stopWhenParentExits(abort);
  const root = resolve(process.env.FAUCET_ROOT ?? join(import.meta.dirname, '..'));
  if (!existsSync(join(root, 'cmd', 'faucet', 'main.go'))) {
    throw new Error(`${root} is not a faucet checkout: cmd/faucet/main.go is missing`);
  }
  const bin = findPostgresBin();
  const dir = runDir(port);
  const socketPath = join(dir, `.s.PGSQL.${DB_PORT}`);
  if (Buffer.byteLength(socketPath) >= SOCKET_PATH_MAX) {
    throw new Error(`${socketPath} is too long for a Unix socket; set TMPDIR to a shorter directory`);
  }
  try {
    mkdirSync(dir, { mode: 0o700 });
  } catch (err) {
    if (err.code === 'EEXIST') {
      const advice = existsSync(join(dir, 'pgdata'))
        ? `stop its app.mjs, faucet and Postgres, then delete ${dir} (Cleanup in the verify-faucet skill)`
        : `it has no pgdata, so there is no Postgres to stop; delete ${dir}`;
      throw new Error(`${dir} exists, so an earlier run was killed; ${advice}`);
    }
    throw err;
  }

  let faucet;
  let failure;
  try {
    await execFileAsync(
      join(bin, 'initdb'),
      ['-U', 'postgres', '--auth-local=trust', '--auth-host=reject', '--no-sync', '-D', join(dir, 'pgdata')],
      { signal },
    );
    await startPostgres(bin, dir, signal);
    log(`postgres listening on ${socketPath}`);

    const binary = join(dir, 'faucet');
    await execFileAsync('go', ['build', '-o', binary, './cmd/faucet'], { cwd: root, signal });

    const secret = randomBytes(32).toString('hex');
    const psql = existsSync(join(bin, 'psql')) ? join(bin, 'psql') : 'psql';
    writeFileSync(stateFile(port), JSON.stringify({ dbHost: dir, dbPort: DB_PORT, psql, secret, binary }), { mode: 0o600 });
    const databaseUrl = `postgres://postgres@/postgres?${new URLSearchParams({ host: dir, port: String(DB_PORT), sslmode: 'disable' })}`;

    faucet = spawn(binary, [], {
      env: {
        DATABASE_URL: databaseUrl,
        GITHUB_WEBHOOK_SECRET: secret,
        PORT: String(port),
      },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    const outcome = await waitForFaucetOrAbort(faucet, signal);
    failure = outcome.error;
  } catch (err) {
    if (!signal.aborted) failure = err.message;
  } finally {
    if (faucet) await stopFaucet(faucet);
    const postgresStopped = !existsSync(join(dir, 'pgdata', 'postmaster.pid')) || (await stopPostgres(bin, dir));
    if (postgresStopped) rmSync(dir, { recursive: true, force: true });
    else failure ??= `left ${dir} in place because Postgres is still running`;
  }
  if (failure) throw new Error(failure);
}

const abort = new AbortController();
for (const name of ['SIGINT', 'SIGTERM']) process.on(name, () => abort.abort());

main(abort).then(
  () => process.exit(0),
  (err) => {
    log(err.message);
    process.exit(1);
  },
);

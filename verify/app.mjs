#!/usr/bin/env node
import { execFile, spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { existsSync, mkdirSync, readdirSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { createServer as createHttpServer } from 'node:http';
import { createServer } from 'node:net';
import { delimiter, dirname, join, resolve } from 'node:path';
import { parseArgs, promisify } from 'node:util';
import { runDir, stateFile } from './runstate.mjs';

const execFileAsync = promisify(execFile);
const FAUCET_STOP_MS = 20_000;
const POSTGRES_START_ATTEMPTS = 5;

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

function freePort() {
  return new Promise((resolvePort, reject) => {
    const probe = createServer();
    probe.once('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const { port } = probe.address();
      probe.close(() => resolvePort(port));
    });
  });
}

// The stub knows one token and answers GET /user and GET /repos/verify/r<id>
// with repository id <id>. Every other repository is 404, like one the token
// cannot see.
async function startGitHubStub(token) {
  const reply = (res, status, body) => {
    res.writeHead(status, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(body));
  };
  const stub = createHttpServer((req, res) => {
    if (req.headers.authorization !== `Bearer ${token}`) return reply(res, 401, { message: 'Bad credentials' });
    if (req.url === '/user') return reply(res, 200, { login: 'verify-user' });
    const repo = /^\/repos\/verify\/r(\d+)$/.exec(req.url ?? '');
    if (repo) return reply(res, 200, { id: Number(repo[1]) });
    return reply(res, 404, { message: 'Not Found' });
  });
  stub.listen(0, '127.0.0.1');
  await once(stub, 'listening');
  return stub;
}

async function startPostgres(bin, dir, signal) {
  const dataDir = join(dir, 'pgdata');
  for (let attempt = 1; ; attempt++) {
    const port = await freePort();
    try {
      await execFileAsync(
        join(bin, 'pg_ctl'),
        [
          'start', '-D', dataDir, '-w', '-t', '60', '-l', join(dir, 'postgres.log'),
          '-o', `-p ${port} -k '${dir}' -c listen_addresses=127.0.0.1 -c fsync=off`,
        ],
        { signal },
      );
      return port;
    } catch (err) {
      if (attempt === POSTGRES_START_ATTEMPTS || signal.aborted) throw err;
      log(`postgres did not start on port ${port}, trying another port`);
    }
  }
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

function waitForFaucetOrAbort(child, signal) {
  return new Promise((resolveOutcome) => {
    if (signal.aborted) return resolveOutcome({ aborted: true });
    signal.addEventListener('abort', () => resolveOutcome({ aborted: true }), { once: true });
    child.once('error', (err) => resolveOutcome({ error: err.message }));
    child.once('exit', (code, sig) => resolveOutcome({ error: `faucet exited early (${sig ?? code})` }));
  });
}

async function main(signal) {
  const { values } = parseArgs({ options: { port: { type: 'string' } } });
  const port = Number(values.port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('--port must be an integer from 1 to 65535');
  }
  const root = resolve(process.env.FAUCET_ROOT ?? join(import.meta.dirname, '..'));
  if (!existsSync(join(root, 'cmd', 'faucet', 'main.go'))) {
    throw new Error(`${root} is not a faucet checkout: cmd/faucet/main.go is missing`);
  }
  const bin = findPostgresBin();
  const dir = runDir(port);
  try {
    mkdirSync(dir, { mode: 0o700 });
  } catch (err) {
    if (err.code === 'EEXIST') {
      throw new Error(
        `${dir} exists, so an earlier run was killed; stop its Postgres with pg_ctl -D ${dir}/pgdata stop, then delete ${dir}`,
      );
    }
    throw err;
  }

  let postgresStarted = false;
  let faucet;
  let githubStub;
  let failure;
  try {
    await execFileAsync(join(bin, 'initdb'), ['-U', 'postgres', '--auth=trust', '--no-sync', '-D', join(dir, 'pgdata')], { signal });
    const dbPort = await startPostgres(bin, dir, signal);
    postgresStarted = true;
    log(`postgres listening on 127.0.0.1:${dbPort}`);

    const binary = join(dir, 'faucet');
    await execFileAsync('go', ['build', '-o', binary, './cmd/faucet'], { cwd: root, signal });

    const secret = randomBytes(32).toString('hex');
    const githubToken = `ghp_${randomBytes(20).toString('hex')}`;
    githubStub = await startGitHubStub(githubToken);
    const psql = existsSync(join(bin, 'psql')) ? join(bin, 'psql') : 'psql';
    writeFileSync(stateFile(port), JSON.stringify({ dbPort, psql, secret, githubToken }), { mode: 0o600 });

    faucet = spawn(binary, [], {
      env: {
        DATABASE_URL: `postgres://postgres@127.0.0.1:${dbPort}/postgres?sslmode=disable`,
        GITHUB_WEBHOOK_SECRET: secret,
        PORT: String(port),
        FAUCET_GITHUB_API_URL: `http://127.0.0.1:${githubStub.address().port}`,
      },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    const outcome = await waitForFaucetOrAbort(faucet, signal);
    failure = outcome.error;
  } catch (err) {
    if (!signal.aborted) failure = err.message;
  } finally {
    if (faucet) await stopFaucet(faucet);
    githubStub?.close();
    const postgresStopped = !postgresStarted || (await stopPostgres(bin, dir));
    if (postgresStopped) rmSync(dir, { recursive: true, force: true });
    else failure ??= `left ${dir} in place because Postgres is still running`;
  }
  if (failure) throw new Error(failure);
}

const abort = new AbortController();
for (const name of ['SIGINT', 'SIGTERM']) process.on(name, () => abort.abort());

main(abort.signal).then(
  () => process.exit(0),
  (err) => {
    log(err.message);
    process.exit(1);
  },
);

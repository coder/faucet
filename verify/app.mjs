#!/usr/bin/env node
import { execFile, spawn } from 'node:child_process';
import { createHmac, randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { existsSync, mkdirSync, readdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { createServer as createHttpServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { delimiter, dirname, join, resolve } from 'node:path';
import { parseArgs, promisify } from 'node:util';
import { CALLBACK_ORIGIN, runDir, stateFile } from './runstate.mjs';

const execFileAsync = promisify(execFile);
const FAUCET_STOP_MS = 20_000;
const DB_PORT = 5432;
const SOCKET_PATH_MAX = 100;
const PARENT_POLL_MS = 1000;
const HEALTHY_MS = 120_000;
// An aborted initdb or go build can still be writing into the directory when it is removed.
const RM_OPTIONS = { recursive: true, force: true, maxRetries: 20, retryDelay: 100 };

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

const HOOK_PATH = '/orgs/verify/hooks/7';
const CRON_DB = 'faucet_cron';

function freePort() {
  return new Promise((resolvePort, reject) => {
    const probe = createNetServer();
    probe.once('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const { port } = probe.address();
      probe.close(() => resolvePort(port));
    });
  });
}

async function readJson(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

// The stub plays GitHub. `token` is the caller's token: GET /user answers
// verify-user, GET /repos/verify/r<id> answers repository id <id>, and every
// other repository is 404, like one the token cannot see. `otherToken` is a
// second caller: GET /user answers verify-other and it reads the same
// repositories.
//
// It also serves the deliveries API of the org webhook verify/7 to `hookToken`
// alone, and it plays what GitHub does on a redelivery: it POSTs the stored
// payload, signed, to the cron faucet. A test sets the deliveries and reads the
// requests through /__stub/, which needs `control`. The records name the kind
// of credential each request carried and never the credential.
async function startGitHubStub({ token, otherToken, hookToken, control, cronSecret, cronUrl, webhookSecret }) {
  const reply = (res, status, body, headers = {}) => {
    res.writeHead(status, { 'Content-Type': 'application/json', ...headers });
    res.end(JSON.stringify(body));
  };
  const kindOf = (auth) => {
    if (auth === `Bearer ${hookToken}`) return 'hook';
    if (auth === `Bearer ${token}` || auth === `Bearer ${otherToken}`) return 'user';
    if (auth === `Bearer ${cronSecret}`) return 'cron';
    return auth ? 'other' : 'none';
  };
  const loginOf = (auth) => {
    if (auth === `Bearer ${token}`) return 'verify-user';
    if (auth === `Bearer ${otherToken}`) return 'verify-other';
    return null;
  };
  let deliveries = [];
  let pageSize = 100;
  let redeliveryStatus = 202;
  let requests = [];
  let address;

  const handleControl = async (req, res, url) => {
    if (req.headers['x-stub-control'] !== control) return reply(res, 403, { message: 'Forbidden' });
    if (req.method === 'PUT' && url.pathname === '/__stub/deliveries') {
      const body = await readJson(req);
      const now = Date.now();
      deliveries = body.deliveries.map((d) => ({ ...d, deliveredAt: now - d.ageMs, redelivery: false }));
      pageSize = body.pageSize ?? 100;
      redeliveryStatus = body.redeliveryStatus ?? 202;
      requests = [];
      return reply(res, 200, {});
    }
    if (req.method === 'GET' && url.pathname === '/__stub/requests') return reply(res, 200, requests);
    return reply(res, 404, { message: 'Not Found' });
  };

  const redeliver = async (res, id) => {
    const original = deliveries.find((d) => d.id === id);
    if (!original) return reply(res, 404, { message: 'Not Found' });
    if (redeliveryStatus !== 202) return reply(res, redeliveryStatus, { message: 'refused' });
    let status = 500;
    if (original.payload) {
      const body = JSON.stringify(original.payload);
      const signature = `sha256=${createHmac('sha256', webhookSecret).update(body).digest('hex')}`;
      const delivered = await fetch(`${cronUrl}/webhook/github`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-GitHub-Event': original.event,
          'X-GitHub-Delivery': original.guid,
          'X-Hub-Signature-256': signature,
        },
        body,
      });
      status = delivered.status;
    }
    const nextId = Math.max(...deliveries.map((d) => d.id)) + 1;
    deliveries.push({ id: nextId, guid: original.guid, deliveredAt: Date.now(), status, event: original.event, redelivery: true });
    return reply(res, 202, {});
  };

  const hooks = async (req, res, url) => {
    if (req.headers.authorization !== `Bearer ${hookToken}`) return reply(res, 401, { message: 'Bad credentials' });
    if (req.method === 'GET' && url.pathname === `${HOOK_PATH}/deliveries`) {
      const sorted = [...deliveries].sort((a, b) => b.deliveredAt - a.deliveredAt || b.id - a.id);
      const perPage = Math.min(Number(url.searchParams.get('per_page')) || 30, pageSize);
      const offset = Number(/^c(\d+)$/.exec(url.searchParams.get('cursor') ?? '')?.[1] ?? 0);
      const page = sorted.slice(offset, offset + perPage);
      const headers = {};
      if (offset + perPage < sorted.length) {
        headers.Link = `<http://127.0.0.1:${address.port}${url.pathname}?per_page=${perPage}&cursor=c${offset + perPage}>; rel="next"`;
      }
      return reply(
        res,
        200,
        page.map((d) => ({
          id: d.id,
          guid: d.guid,
          delivered_at: new Date(d.deliveredAt).toISOString(),
          redelivery: d.redelivery,
          duration: 0.1,
          status: String(d.status),
          status_code: d.status,
          event: d.event ?? 'workflow_run',
          action: null,
          installation_id: null,
          repository_id: null,
          throttled_at: null,
        })),
        headers,
      );
    }
    const attempt = req.method === 'POST' && new RegExp(`^${HOOK_PATH}/deliveries/(\\d+)/attempts$`).exec(url.pathname);
    if (attempt) return redeliver(res, Number(attempt[1]));
    return reply(res, 404, { message: 'Not Found' });
  };

  const stub = createHttpServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', 'http://stub');
      if (url.pathname.startsWith('/__stub/')) return await handleControl(req, res, url);
      requests.push({ method: req.method, path: url.pathname, query: url.search, auth: kindOf(req.headers.authorization) });
      if (url.pathname.startsWith(HOOK_PATH)) return await hooks(req, res, url);
      const login = loginOf(req.headers.authorization);
      if (login === null) return reply(res, 401, { message: 'Bad credentials' });
      if (url.pathname === '/user') return reply(res, 200, { login });
      const repo = /^\/repos\/verify\/r(\d+)$/.exec(url.pathname);
      if (repo) return reply(res, 200, { id: Number(repo[1]) });
      return reply(res, 404, { message: 'Not Found' });
    } catch (err) {
      log(`stub: ${err.message}`);
      return reply(res, 500, { message: 'stub error' });
    }
  });
  stub.listen(0, '127.0.0.1');
  await once(stub, 'listening');
  address = stub.address();
  return stub;
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

function postmasterRuns(dir) {
  try {
    const pid = Number(readFileSync(join(dir, 'pgdata', 'postmaster.pid'), 'utf8').split('\n', 1)[0]);
    if (!(pid > 0)) return false;
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err.code === 'EPERM';
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

function stopWhenParentExits(abort) {
  const parent = process.ppid;
  setInterval(() => {
    if (process.ppid === parent) return;
    log('parent process exited, shutting down');
    abort.abort();
  }, PARENT_POLL_MS).unref();
}

function waitForFaucetOrAbort(children, signal) {
  return new Promise((resolveOutcome) => {
    if (signal.aborted) return resolveOutcome({ aborted: true });
    signal.addEventListener('abort', () => resolveOutcome({ aborted: true }), { once: true });
    for (const child of children) {
      child.once('error', (err) => resolveOutcome({ error: err.message }));
      child.once('exit', (code, sig) => resolveOutcome({ error: `faucet exited early (${sig ?? code})` }));
    }
  });
}

async function waitUntilHealthy(port, child, signal) {
  const deadline = Date.now() + HEALTHY_MS;
  while (child.exitCode === null && child.signalCode === null) {
    signal.throwIfAborted();
    try {
      if ((await fetch(`http://127.0.0.1:${port}/healthz`, { signal })).status === 200) return;
    } catch (err) {
      if (signal.aborted) throw err;
    }
    if (Date.now() > deadline) throw new Error(`the cron faucet did not answer /healthz within ${HEALTHY_MS} ms`);
    await new Promise((resolveWait) => setTimeout(resolveWait, 100));
  }
  throw new Error('the cron faucet exited before it answered /healthz');
}

async function createDatabase(psql, dir, name) {
  await execFileAsync(psql, [
    '-X', '-q', '-h', dir, '-p', String(DB_PORT), '-U', 'postgres', '-d', 'postgres',
    '-v', 'ON_ERROR_STOP=1', '-c', `CREATE DATABASE ${name}`,
  ]);
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
  let cronFaucet;
  let githubStub;
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
    const githubToken = `ghp_${randomBytes(20).toString('hex')}`;
    const otherToken = `ghp_${randomBytes(20).toString('hex')}`;
    const webhookKey = randomBytes(32).toString('base64');
    const psql = existsSync(join(bin, 'psql')) ? join(bin, 'psql') : 'psql';
    const cron = {
      port: await freePort(),
      db: CRON_DB,
      webhookSecret: randomBytes(32).toString('hex'),
      cronSecret: randomBytes(32).toString('hex'),
      hookToken: `ghp_${randomBytes(20).toString('hex')}`,
      stubControl: randomBytes(16).toString('hex'),
    };
    githubStub = await startGitHubStub({
      token: githubToken,
      otherToken,
      hookToken: cron.hookToken,
      control: cron.stubControl,
      cronSecret: cron.cronSecret,
      cronUrl: `http://127.0.0.1:${cron.port}`,
      webhookSecret: cron.webhookSecret,
    });
    const stubUrl = `http://127.0.0.1:${githubStub.address().port}`;
    await createDatabase(psql, dir, CRON_DB);
    writeFileSync(
      stateFile(port),
      JSON.stringify({ dbHost: dir, dbPort: DB_PORT, psql, secret, githubToken, otherToken, webhookKey, binary, cron: { ...cron, stubUrl } }),
      { mode: 0o600 },
    );
    const databaseUrl = (name) =>
      `postgres://postgres@/${name}?${new URLSearchParams({ host: dir, port: String(DB_PORT), sslmode: 'disable' })}`;

    cronFaucet = spawn(binary, [], {
      env: {
        DATABASE_URL: databaseUrl(CRON_DB),
        GITHUB_WEBHOOK_SECRET: cron.webhookSecret,
        PORT: String(cron.port),
        FAUCET_GITHUB_API_URL: stubUrl,
        CRON_SECRET: cron.cronSecret,
        FAUCET_HOOK_TOKEN: cron.hookToken,
        FAUCET_HOOK_ORG: 'verify',
        FAUCET_HOOK_ID: '7',
      },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    await waitUntilHealthy(cron.port, cronFaucet, signal);
    log(`cron faucet listening on ${cron.port}`);

    faucet = spawn(binary, [], {
      env: {
        DATABASE_URL: databaseUrl('postgres'),
        GITHUB_WEBHOOK_SECRET: secret,
        PORT: String(port),
        FAUCET_GITHUB_API_URL: stubUrl,
        FAUCET_WEBHOOK_KEY: webhookKey,
        FAUCET_WEBHOOK_ALLOWED_ORIGINS: CALLBACK_ORIGIN,
      },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    const outcome = await waitForFaucetOrAbort([faucet, cronFaucet], signal);
    failure = outcome.error;
  } catch (err) {
    if (!signal.aborted) failure = err.message;
  } finally {
    if (faucet) await stopFaucet(faucet);
    if (cronFaucet) await stopFaucet(cronFaucet);
    githubStub?.close();
    const postgresStopped = !postmasterRuns(dir) || (await stopPostgres(bin, dir));
    if (postgresStopped) rmSync(dir, RM_OPTIONS);
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

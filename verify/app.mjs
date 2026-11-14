#!/usr/bin/env node
import { execFile, spawn } from 'node:child_process';
import { createHmac, randomBytes, timingSafeEqual } from 'node:crypto';
import { once } from 'node:events';
import { closeSync, existsSync, mkdirSync, openSync, readdirSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { createServer as createHttpServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { delimiter, dirname, join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { parseArgs, promisify } from 'node:util';
import { childEnv } from './env.mjs';
import { CALLBACK_ORIGIN, ENDPOINT_SAYS, runDir, stateFile } from './runstate.mjs';

const execFileAsync = promisify(execFile);
const STOP_MS = 20_000;
const GROUP_STOP_MS = 10_000;
const POSTGRES_START_MS = 60_000;
const DB_PORT = 5432;
const SOCKET_PATH_MAX = 100;
const PARENT_POLL_MS = 1000;
const PARENT_ENV = 'FAUCET_VERIFY_PARENT';
const HEALTHY_MS = 120_000;
// An aborted initdb or go build can still be writing into the directory when it is removed.
const RM_OPTIONS = { recursive: true, force: true, maxRetries: 20, retryDelay: 100 };

function log(message) {
  console.error(`faucet-verify: ${message}`);
}

function hasServerBinaries(dir) {
  return ['initdb', 'postgres'].every((name) => existsSync(join(dir, name)));
}

function findPostgresBin() {
  if (process.env.PG_BIN) {
    if (!hasServerBinaries(process.env.PG_BIN)) {
      throw new Error('PG_BIN must name a directory holding initdb and postgres');
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

const HOOK_PATH = '/orgs/verify/hooks/7';
const RETENTION_MS = 72 * 60 * 60 * 1000;

async function readJson(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

// The stub plays GitHub. It knows two tokens and answers GET /user with the login of the token (verify-user and
// verify-other) and GET /repos/verify/r<id> with repository id <id> for both. Every other repository is 404, like one the
// token cannot see, and so is one that the receiver's control channel has revoked.
//
// It also serves the deliveries API of the org webhook verify/7 to `hookToken` alone, as GitHub does: attempts newest first,
// 100 at most per page, a Link header with a keyset cursor (v1_<attempt id>), nothing older than 72 hours. A redelivery
// request makes the stub post the stored payload, signed, to the faucet that the test named, and add the answer as a new
// attempt. A test sets the attempts and reads the requests through /__stub/, which needs `control`. The request log names
// the kind of credential each request carried and never the credential.
async function startGitHubStub({ token, otherToken, hookToken, control, revoked }) {
  const reply = (res, status, body, headers = {}) => {
    res.writeHead(status, { 'Content-Type': 'application/json', ...headers });
    res.end(JSON.stringify(body));
  };
  const kindOf = (auth) => {
    if (auth === `Bearer ${hookToken}`) return 'hook';
    if (auth === `Bearer ${token}` || auth === `Bearer ${otherToken}`) return 'user';
    return auth ? 'other' : 'none';
  };
  let attempts = [];
  let nextId = 1000;
  let pageSize = 100;
  let listDelayMs = 0;
  let redeliveryStatus = 202;
  let target = null;
  let requests = [];
  let address;

  const add = (items) => {
    const now = Date.now();
    for (const d of items) {
      attempts.push({
        id: d.id ?? nextId++,
        guid: d.guid,
        at: now - d.ageMs,
        status: d.status,
        event: d.event ?? 'workflow_run',
        action: d.action === undefined ? 'completed' : d.action,
        redelivery: false,
        payload: d.payload,
      });
    }
  };

  const handleControl = async (req, res, url) => {
    if (req.headers['x-stub-control'] !== control) return reply(res, 403, { message: 'Forbidden' });
    if (req.method === 'PUT' && url.pathname === '/__stub/deliveries') {
      const body = await readJson(req);
      attempts = [];
      add(body.deliveries);
      target = body.target ?? null;
      pageSize = body.pageSize ?? 100;
      listDelayMs = body.listDelayMs ?? 0;
      redeliveryStatus = body.redeliveryStatus ?? 202;
      requests = [];
      return reply(res, 200, {});
    }
    if (req.method === 'POST' && url.pathname === '/__stub/deliveries') {
      add((await readJson(req)).deliveries);
      return reply(res, 200, {});
    }
    if (req.method === 'GET' && url.pathname === '/__stub/requests') return reply(res, 200, requests);
    if (req.method === 'GET' && url.pathname === '/__stub/attempts') {
      return reply(res, 200, attempts.map(({ id, guid, status, redelivery }) => ({ id, guid, status, redelivery })));
    }
    return reply(res, 404, { message: 'Not Found' });
  };

  const newestFirst = (a, b) => b.at - a.at || b.id - a.id;

  const list = async (res, url) => {
    if (listDelayMs > 0) await new Promise((done) => setTimeout(done, listDelayMs));
    const visible = attempts.filter((a) => a.at <= Date.now() && a.at > Date.now() - RETENTION_MS).sort(newestFirst);
    const perPage = Math.min(Number(url.searchParams.get('per_page')) || 30, pageSize);
    const cursor = /^v1_(\d+)$/.exec(url.searchParams.get('cursor') ?? '')?.[1];
    let start = 0;
    if (cursor !== undefined) {
      const from = attempts.find((a) => a.id === Number(cursor));
      if (!from) return reply(res, 422, { message: 'invalid cursor' });
      start = visible.filter((a) => newestFirst(a, from) <= 0).length;
    }
    const page = visible.slice(start, start + perPage);
    const headers = {};
    if (start + perPage < visible.length) {
      headers.Link = `<http://127.0.0.1:${address.port}${url.pathname}?per_page=${perPage}&cursor=v1_${page.at(-1).id}>; rel="next"`;
    }
    return reply(
      res,
      200,
      page.map((d) => ({
        id: d.id,
        guid: d.guid,
        delivered_at: new Date(d.at).toISOString().replace(/\.\d+Z$/, 'Z'),
        redelivery: d.redelivery,
        duration: 0.1,
        status: String(d.status),
        status_code: d.status,
        event: d.event,
        action: d.action,
        installation_id: null,
        repository_id: null,
        throttled_at: null,
      })),
      headers,
    );
  };

  const redeliver = async (res, id) => {
    const original = attempts.find((a) => a.id === id);
    if (!original) return reply(res, 404, { message: 'Not Found' });
    if (redeliveryStatus !== 202) return reply(res, redeliveryStatus, { message: 'refused' });
    let status = 500;
    if (original.payload && target) {
      const body = JSON.stringify(original.payload);
      const signature = `sha256=${createHmac('sha256', target.secret).update(body).digest('hex')}`;
      const delivered = await fetch(`${target.url}/webhook/github`, {
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
    attempts.push({ ...original, id: nextId++, at: Date.now(), status, redelivery: true });
    return reply(res, 202, {});
  };

  const hooks = async (req, res, url) => {
    if (req.headers.authorization !== `Bearer ${hookToken}`) return reply(res, 401, { message: 'Bad credentials' });
    if (req.method === 'GET' && url.pathname === `${HOOK_PATH}/deliveries`) return list(res, url);
    const attempt = req.method === 'POST' && new RegExp(`^${HOOK_PATH}/deliveries/(\\d+)/attempts$`).exec(url.pathname);
    if (attempt) return redeliver(res, Number(attempt[1]));
    return reply(res, 404, { message: 'Not Found' });
  };

  const stub = createHttpServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', 'http://stub');
      if (url.pathname.startsWith('/__stub/')) return await handleControl(req, res, url);
      requests.push({ method: req.method, path: url.pathname, query: url.search, auth: kindOf(req.headers.authorization) });
      if (url.pathname.startsWith('/orgs/')) return await hooks(req, res, url);
      const login = { [`Bearer ${token}`]: 'verify-user', [`Bearer ${otherToken}`]: 'verify-other' }[req.headers.authorization ?? ''];
      if (login === undefined) return reply(res, 401, { message: 'Bad credentials' });
      if (url.pathname === '/user') return reply(res, 200, { login });
      const repo = /^\/repos\/verify\/r(\d+)$/.exec(url.pathname);
      if (repo && !revoked.has(Number(repo[1]))) return reply(res, 200, { id: Number(repo[1]) });
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

// checkSignature is an independent reading of Standard Webhooks, written for this stub and sharing nothing with faucet: the
// key is the base64 decoding of the whsec_ secret, and the signature is v1, and the base64 of the HMAC-SHA256 of
// "<webhook-id>.<webhook-timestamp>.<body>".
function checkSignature(secret, headers, body) {
  if (secret === undefined) return 'unregistered';
  const id = headers['webhook-id'];
  const timestamp = headers['webhook-timestamp'];
  const signatures = headers['webhook-signature'];
  if (!id || !timestamp || !signatures) return 'missing';
  const key = Buffer.from(secret.slice('whsec_'.length), 'base64');
  const expected = createHmac('sha256', key).update(`${id}.${timestamp}.`).update(body).digest();
  const matches = signatures.split(' ').some((candidate) => {
    const [version, value] = candidate.split(',');
    const given = Buffer.from(value ?? '', 'base64');
    return version === 'v1' && given.length === expected.length && timingSafeEqual(given, expected);
  });
  return matches ? 'valid' : 'invalid';
}

const EVENT_STATUSES = {
  ok: [200],
  gone: [410],
  toolarge: [413],
  fail500: [500],
  'fail-once': [500, 200],
  'gone-once': [410, 200],
  'toolarge-once': [413, 200],
};

// The receiver plays the endpoint behind a callback URL. A callback URL is /hook/<behavior>/<id>, and the behavior says how
// the endpoint answers a verification request:
//   echo      checks the signature with the secret that the test registered for <id> and answers the nonce, or 401
//   wrong     answers 200 with another nonce
//   reflect   answers 200 with the request body
//   error500, notfound   answer that status
//   redirect  answers 302 to /hook/internal/<id>, which answers the nonce like an internal service would
//   slow      never answers
//   large     answers the nonce and then 4 KiB of spaces
// The delivery behaviors answer a verification request like echo, and answer an event request (a body with no type) by the
// statuses of the behavior, one per request for the callback id, the last one for every request after that. A 200 needs a
// valid signature and is 401 without one:
//   ok 200   gone 410   toolarge 413   fail500 500   fail-once 500, then 200   gone-once 410, then 200   toolarge-once 413, then 200
//   redirect-events   302 to /hook/internal/<id>
// The answers to events, except the redirect, have the body ENDPOINT_SAYS, which no log or table of faucet may hold.
// It records every request. A test registers secrets, reads the records and revokes the access of the GitHub stub to a
// repository (PUT /__receiver/revoked) through /__receiver/, which needs `control`.
// The records hold the signature headers and never a secret.
async function startReceiver({ control, revoked }) {
  const secrets = new Map();
  const requests = [];
  const eventRequests = new Map();
  const reply = (res, status, body = '', headers = {}) => {
    res.writeHead(status, headers);
    res.end(body);
  };

  const handleControl = (req, res, url, body) => {
    if (req.headers['x-stub-control'] !== control) return reply(res, 403, '{}');
    if (req.method === 'PUT' && url.pathname === '/__receiver/secrets') {
      const { id, secret } = JSON.parse(body.toString('utf8'));
      secrets.set(id, secret);
      return reply(res, 200, '{}', { 'Content-Type': 'application/json' });
    }
    if (req.method === 'PUT' && url.pathname === '/__receiver/revoked') {
      revoked.add(Number(JSON.parse(body.toString('utf8')).repoId));
      return reply(res, 200, '{}', { 'Content-Type': 'application/json' });
    }
    if (req.method === 'GET' && url.pathname === '/__receiver/requests') {
      const id = url.searchParams.get('id');
      return reply(res, 200, JSON.stringify(requests.filter((r) => r.id === id)), { 'Content-Type': 'application/json' });
    }
    return reply(res, 404, '{}');
  };

  const server = createHttpServer(async (req, res) => {
    try {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = Buffer.concat(chunks);
      const url = new URL(req.url ?? '/', 'http://receiver');
      if (url.pathname.startsWith('/__receiver/')) return handleControl(req, res, url, body);
      const [, prefix, behavior, id] = url.pathname.split('/');
      if (prefix !== 'hook' || !behavior || !id) return reply(res, 404);
      const text = body.toString('utf8');
      const signature = checkSignature(secrets.get(id), req.headers, body);
      requests.push({ id, behavior, method: req.method, path: url.pathname, headers: req.headers, body: text, signature });
      let challenge = '';
      let verification = false;
      try {
        const parsed = JSON.parse(text);
        challenge = parsed.challenge ?? '';
        verification = parsed.type === 'verification';
      } catch {
        // A body that is not JSON has no challenge to answer.
      }
      const json = { 'Content-Type': 'application/json' };
      const answer = JSON.stringify({ challenge });
      const echo = () => (signature === 'valid' ? reply(res, 200, `${answer}\n`, json) : reply(res, 401, '{}', json));
      const statuses = Object.hasOwn(EVENT_STATUSES, behavior) ? EVENT_STATUSES[behavior] : undefined;
      if (statuses !== undefined || behavior === 'redirect-events') {
        if (verification) return echo();
        if (behavior === 'redirect-events') return reply(res, 302, '', { Location: `/hook/internal/${id}` });
        const seen = eventRequests.get(id) ?? 0;
        eventRequests.set(id, seen + 1);
        const status = statuses[Math.min(seen, statuses.length - 1)];
        const said = JSON.stringify({ said: ENDPOINT_SAYS });
        return status === 200 && signature !== 'valid' ? reply(res, 401, said, json) : reply(res, status, said, json);
      }
      switch (behavior) {
        case 'echo':
          return echo();
        case 'internal':
          return reply(res, 200, answer, json);
        case 'wrong':
          return reply(res, 200, JSON.stringify({ challenge: 'A'.repeat(43) }), json);
        case 'reflect':
          return reply(res, 200, text, json);
        case 'error500':
          return reply(res, 500, '{}', json);
        case 'notfound':
          return reply(res, 404, '{}', json);
        case 'redirect':
          return reply(res, 302, '', { Location: `/hook/internal/${id}` });
        case 'large':
          return reply(res, 200, answer + ' '.repeat(4096), json);
        case 'slow':
          return undefined;
        default:
          return reply(res, 404);
      }
    } catch (err) {
      log(`receiver: ${err.message}`);
      return reply(res, 500, '{}');
    }
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  return server;
}

function startPostgres(bin, dir) {
  const logFd = openSync(join(dir, 'postgres.log'), 'a');
  try {
    return spawn(
      join(bin, 'postgres'),
      ['-D', join(dir, 'pgdata'), '-p', String(DB_PORT), '-k', dir, '-c', 'listen_addresses=', '-c', 'unix_socket_permissions=0700', '-c', 'fsync=off'],
      { env: childEnv(), stdio: ['ignore', logFd, logFd] },
    );
  } finally {
    closeSync(logFd);
  }
}

async function waitUntilAcceptingConnections(psql, dir, postgres, signal) {
  const deadline = Date.now() + POSTGRES_START_MS;
  while (postgres.exitCode === null && postgres.signalCode === null) {
    signal.throwIfAborted();
    try {
      await execFileAsync(psql, ['-X', '-q', '-h', dir, '-p', String(DB_PORT), '-U', 'postgres', '-d', 'postgres', '-c', 'select 1'], { env: childEnv(), signal });
      return;
    } catch (err) {
      if (signal.aborted) throw err;
    }
    if (Date.now() > deadline) throw new Error(`postgres did not accept connections within ${POSTGRES_START_MS} ms`);
    await sleep(100);
  }
  throw new Error('postgres exited before it accepted connections');
}

async function stopChild(child, signal) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = once(child, 'exit');
  child.kill(signal);
  const timer = setTimeout(() => child.kill('SIGKILL'), STOP_MS);
  await exited;
  clearTimeout(timer);
}

function parseOptions() {
  const { values } = parseArgs({ options: { port: { type: 'string' }, detach: { type: 'boolean' }, stack: { type: 'boolean' } } });
  const port = Number(values.port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('--port must be an integer from 1 to 65535');
  }
  return { port, detach: values.detach === true, stack: values.stack === true };
}

function stopWhenLifelineEnds(abort) {
  process.stdin.once('close', () => {
    log('the watchdog exited, shutting down');
    abort.abort();
  });
  process.stdin.resume();
}

function groupExists(pgid) {
  try {
    process.kill(-pgid, 0);
    return true;
  } catch (err) {
    return err.code === 'EPERM';
  }
}

// Postgres treats SIGINT as a fast shutdown, so the group gets it first and SIGKILL only when something ignores it.
async function endGroup(pgid) {
  if (!groupExists(pgid)) return;
  process.kill(-pgid, 'SIGINT');
  const deadline = Date.now() + GROUP_STOP_MS;
  while (groupExists(pgid) && Date.now() < deadline) await sleep(100);
  if (groupExists(pgid)) process.kill(-pgid, 'SIGKILL');
}

// The watchdog is what the runner starts. It owns nothing but a stack process in a process group of its own, and every
// process of the stack (faucet, go build, initdb, Postgres) belongs to that group. Whichever of the runner, the watchdog and
// the stack process is killed, the stack's group still has a process that ends it: the watchdog on the stack's death, the
// stack process on the watchdog's (the pipe it reads closes), and the watchdog on the runner's.
async function supervise({ port, detach }) {
  const expected = process.env[PARENT_ENV] === undefined ? process.ppid : Number(process.env[PARENT_ENV]);
  if (!Number.isInteger(expected) || expected < 2) throw new Error(`${PARENT_ENV} must be a process ID`);
  if (!detach && process.ppid !== expected) {
    throw new Error(`process ${expected} is not the parent of app.mjs, so it exited before app.mjs started: no stack started`);
  }
  const stack = spawn(process.execPath, [import.meta.filename, ...process.argv.slice(2), '--stack'], {
    detached: true,
    stdio: ['pipe', 'inherit', 'inherit'],
  });
  for (const name of ['SIGINT', 'SIGTERM']) process.on(name, () => stack.kill(name));
  if (!detach) {
    setInterval(() => {
      if (process.ppid === expected) return;
      log('parent process exited, shutting down');
      stack.kill('SIGTERM');
    }, PARENT_POLL_MS).unref();
  }
  const [code, killedBy] = await once(stack, 'exit');
  await endGroup(stack.pid);
  if (killedBy !== null) rmSync(runDir(port), RM_OPTIONS);
  return code ?? 1;
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
  ], { env: childEnv() });
}

async function runStack({ port }, abort) {
  const { signal } = abort;
  stopWhenLifelineEnds(abort);
  const root = resolve(process.env.FAUCET_ROOT ?? join(import.meta.dirname, '..'));
  if (!existsSync(join(root, 'cmd', 'faucet', 'main.go'))) {
    throw new Error(`${root} is not a faucet checkout: cmd/faucet/main.go is missing`);
  }
  const bin = findPostgresBin();
  const psql = existsSync(join(bin, 'psql')) ? join(bin, 'psql') : 'psql';
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

  let postgres;
  let faucet;
  let cronFaucet;
  let githubStub;
  let receiver;
  let failure;
  try {
    await execFileAsync(
      join(bin, 'initdb'),
      ['-U', 'postgres', '--auth-local=trust', '--auth-host=reject', '--no-sync', '-D', join(dir, 'pgdata')],
      { env: childEnv(), signal },
    );
    postgres = startPostgres(bin, dir);
    await waitUntilAcceptingConnections(psql, dir, postgres, signal);
    log(`postgres listening on ${socketPath}`);

    const binary = join(dir, 'faucet');
    const goEnv = { ...childEnv(), GOTMPDIR: join(dir, 'gotmp') };
    mkdirSync(goEnv.GOTMPDIR);
    await execFileAsync('go', ['build', '-o', binary, './cmd/faucet'], { cwd: root, env: goEnv, signal });
    // The e2e build lets the callback client reach loopback over http, so the handshake can reach the receiver. Only tests that
    // start a faucet with it get that. The main faucet and the production binary above do not.
    const e2eBinary = join(dir, 'faucet-e2e');
    await execFileAsync('go', ['build', '-tags', 'faucet_e2e', '-o', e2eBinary, './cmd/faucet'], { cwd: root, env: goEnv, signal });

    const secret = randomBytes(32).toString('hex');
    const githubToken = `ghp_${randomBytes(20).toString('hex')}`;
    const otherToken = `ghp_${randomBytes(20).toString('hex')}`;
    const webhookKey = randomBytes(32).toString('base64');
    const cron = {
      port: await freePort(),
      db: CRON_DB,
      webhookSecret: randomBytes(32).toString('hex'),
      cronSecret: randomBytes(32).toString('hex'),
      hookToken: `ghp_${randomBytes(20).toString('hex')}`,
      stubControl: randomBytes(16).toString('hex'),
    };
    const revoked = new Set();
    githubStub = await startGitHubStub({ token: githubToken, otherToken, hookToken: cron.hookToken, control: cron.stubControl, revoked });
    const stubUrl = `http://127.0.0.1:${githubStub.address().port}`;
    await createDatabase(psql, dir, CRON_DB);
    const receiverControl = randomBytes(16).toString('hex');
    receiver = await startReceiver({ control: receiverControl, revoked });
    const receiverState = { url: `http://127.0.0.1:${receiver.address().port}`, control: receiverControl };
    writeFileSync(
      stateFile(port),
      JSON.stringify({
        dbHost: dir, dbPort: DB_PORT, psql, secret, githubToken, otherToken, webhookKey, binary, e2eBinary,
        cron: { ...cron, stubUrl }, receiver: receiverState,
      }),
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
    if (faucet) await stopChild(faucet, 'SIGTERM');
    if (cronFaucet) await stopChild(cronFaucet, 'SIGTERM');
    githubStub?.close();
    receiver?.closeAllConnections();
    receiver?.close();
    if (postgres) await stopChild(postgres, 'SIGINT');
    rmSync(dir, RM_OPTIONS);
  }
  if (failure) throw new Error(failure);
}

const abort = new AbortController();
const options = parseOptions();
if (options.stack) for (const name of ['SIGINT', 'SIGTERM']) process.on(name, () => abort.abort());

(options.stack ? runStack(options, abort).then(() => 0) : supervise(options)).then(
  (code) => process.exit(code),
  (err) => {
    log(err.message);
    process.exit(1);
  },
);

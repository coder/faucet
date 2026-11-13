import { randomBytes } from 'node:crypto';
import { samples } from './payloads';
import { expect, newDeliveryId, type Faucet, type ReceivedRequest, type Receiver, type Reply } from './support';
import { EVENT, newMarker, newSecret, repoOf, subscribed, type Params } from './subscriptions';

const HANDSHAKE_ENV = { FAUCET_WEBHOOK_KEY: randomBytes(32).toString('base64'), FAUCET_WEBHOOK_CALLBACK_VERIFICATION: 'handshake' };
const run = samples['workflow_run.completed'];

export const MESSAGE_ID = /^msg_[A-Za-z0-9_-]{22}$/;
export const SIGNATURE = /^v1,[A-Za-z0-9+/]{43}=$/;
export const CATEGORY = /^(connection_refused|timeout|tls_error|http_4xx|http_5xx|challenge_failed)$/;

// Endpoint is a callback URL on the receiver, with the secret that the receiver checks signatures with. The URL has the id in
// its path and token in its query, and the server must keep both out of its log.
export interface Endpoint {
  id: string;
  token: string;
  secret: string;
  url: string;
}

export async function endpoint(receiver: Receiver, behavior: string): Promise<Endpoint> {
  const id = newMarker();
  const token = randomBytes(8).toString('hex');
  const secret = newSecret();
  await receiver.register(id, secret);
  return { id, token, secret, url: receiver.callback(behavior, id, { query: `token=${token}` }) };
}

// withDeliveryFaucet starts a faucet that verifies callbacks by the handshake and reaches the receiver on loopback.
export function withDeliveryFaucet<T>(faucet: Faucet, fn: (started: Faucet, output: () => string) => Promise<T>): Promise<T> {
  return faucet.withFaucet(HANDSHAKE_ENV, fn, { e2eBuild: true });
}

export function paramsFor(repoId: number, at: Endpoint, args: Record<string, unknown> = {}, name: string = EVENT): Params {
  return { name, arguments: { repo: repoOf(repoId), ...args }, delivery: { url: at.url, secret: at.secret } };
}

// runPayload is a completed workflow run of the repository. runId tells the events of a test apart in their data.
export function runPayload(repoId: number, runId: number): object {
  const payload = run.payload(repoId) as { workflow_run: object };
  return { ...payload, workflow_run: { ...payload.workflow_run, id: runId } };
}

// storeRun posts a signed workflow_run event to the faucet and returns the GitHub delivery ID that events/poll names it by.
export async function storeRun(started: Faucet, repoId: number, runId: number): Promise<string> {
  const delivery = newDeliveryId();
  const reply = await started.deliver({ event: run.event, delivery, body: JSON.stringify(runPayload(repoId, runId)) });
  expect(reply.body, `workflow_run ${runId}`).toBe('stored\n');
  return delivery;
}

export interface DeliverReply {
  enqueued: number;
  gaps: number;
  claimed: number;
  sent: number;
  delivered: number;
  retried: number;
  gave_up: Record<string, number>;
  superseded: number;
  incomplete: boolean;
}

// counts is the reply of a run that did what over says and nothing else.
export function counts(over: Partial<DeliverReply> = {}): DeliverReply {
  return { enqueued: 0, gaps: 0, claimed: 0, sent: 0, delivered: 0, retried: 0, gave_up: {}, superseded: 0, incomplete: false, ...over };
}

export function parseReply(reply: Reply): DeliverReply {
  expect(reply.status, reply.body).toBe(200);
  expect(reply.headers.get('content-type')).toContain('application/json');
  return JSON.parse(reply.body) as DeliverReply;
}

export async function deliverDue(started: Faucet): Promise<DeliverReply> {
  return parseReply(await started.cronJob('deliver'));
}

export interface WebhookBody {
  eventId: string;
  name: string;
  timestamp: string;
  data: Record<string, unknown>;
  cursor: string;
}

// eventRequests are the requests that the endpoint got for events, without the verification request of the handshake.
export async function eventRequests(receiver: Receiver, at: Endpoint): Promise<ReceivedRequest[]> {
  const all = await receiver.requests(at.id);
  return all.filter((r) => (JSON.parse(r.body) as { type?: string }).type !== 'verification');
}

// expectSigned checks what every request of an event owes the receiver and returns its body: a signature that Node verified,
// the headers of Standard Webhooks, the subscription's ID, no credential, and the five members of the body.
export function expectSigned(request: ReceivedRequest, subscriptionId: string): WebhookBody {
  const { headers } = request;
  expect(request.method).toBe('POST');
  expect(request.signature, 'the receiver verified the HMAC with Node crypto, with the subscriber secret').toBe('valid');
  expect(headers['content-type']).toBe('application/json');
  expect(headers['webhook-id']).toMatch(MESSAGE_ID);
  expect(Math.abs(Number(headers['webhook-timestamp']) - Date.now() / 1000)).toBeLessThan(60);
  expect(headers['webhook-signature']).toMatch(SIGNATURE);
  expect(headers['x-mcp-subscription-id']).toBe(subscriptionId);
  expect(headers.authorization).toBeUndefined();
  expect(headers.cookie).toBeUndefined();
  const body = JSON.parse(request.body) as WebhookBody;
  expect(Object.keys(body).sort()).toEqual(['cursor', 'data', 'eventId', 'name', 'timestamp']);
  expect(body.eventId, 'eventId is the webhook-id').toBe(headers['webhook-id']);
  return body;
}

export interface PolledEvent {
  eventId: string;
  name: string;
  timestamp: string;
  data: Record<string, unknown>;
}

interface Polled {
  events: PolledEvent[];
  cursor: string;
}

async function poll(started: Faucet, repoId: number, cursor?: string): Promise<Polled> {
  const reply = await started.mcp<Polled>('events/poll', { name: EVENT, arguments: { repo: repoOf(repoId) }, ...(cursor === undefined ? {} : { cursor }) });
  expect(reply.error, reply.body).toBeNull();
  if (reply.result === null) throw new Error(`poll returned no result: ${reply.body}`);
  return reply.result;
}

export async function pollStart(started: Faucet, repoId: number): Promise<string> {
  return (await poll(started, repoId)).cursor;
}

export async function pollFrom(started: Faucet, repoId: number, cursor: string): Promise<PolledEvent[]> {
  return (await poll(started, repoId, cursor)).events;
}

export interface DeliveryStatus {
  active: boolean;
  lastDeliveryAt?: string;
  lastError: string | null;
  failedSince?: string;
}

// refreshed renews the subscription and returns the deliveryStatus that the renewal reports.
export async function refreshed(started: Faucet, params: Params): Promise<DeliveryStatus> {
  const result = (await subscribed(started, params)) as { deliveryStatus?: DeliveryStatus };
  if (result.deliveryStatus === undefined) throw new Error('a renewal must report deliveryStatus');
  return result.deliveryStatus;
}

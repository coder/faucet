import { randomBytes } from 'node:crypto';
import { CALLBACK_ORIGIN } from '../runstate.mjs';
import { expect, type Faucet, type McpOptions, type McpReply } from './support';

export { CALLBACK_ORIGIN };

export const EVENT = 'ci.run_completed';
export const REPO_DENIED = 'repository not found or not accessible';
export const SUBSCRIPTION_LIMIT = 100;
export const MIN_TTL_MS = 30_000;
export const MAX_TTL_MS = 600_000;

export interface SubscribeResult {
  resultType: string;
  id: string;
  refreshBefore: string;
  cursor: string;
  truncated: boolean;
}

export type Params = Record<string, unknown>;

export function repoOf(repoId: number): string {
  return `verify/r${repoId}`;
}

// newMarker is the part of a test's callback URLs that no other test uses, so that the test finds its own rows and its own
// log lines.
export function newMarker(): string {
  return randomBytes(8).toString('hex');
}

// callbackUrl has the marker in its path and a token in its query, both of which the server must keep out of its log.
export function callbackUrl(marker: string, suffix = ''): string {
  return `${CALLBACK_ORIGIN}/hook/${marker}${suffix}?token=q${marker}`;
}

export function newSecret(bytes = 32): string {
  return `whsec_${randomBytes(bytes).toString('base64')}`;
}

export function delivery(marker: string, over: Params = {}): Params {
  return { url: callbackUrl(marker), secret: newSecret(), ...over };
}

export function subscribeParams(repoId: number, marker: string, over: Params = {}): Params {
  return { name: EVENT, arguments: { repo: repoOf(repoId) }, delivery: delivery(marker), ...over };
}

// unsubscribeParams addresses the subscription that subscribe params make: the same name, arguments and URL, and no secret.
export function unsubscribeParams(subscribe: Params): Params {
  const { url } = subscribe.delivery as { url: string };
  return { name: subscribe.name, arguments: subscribe.arguments, delivery: { url } };
}

export function subscribe(faucet: Faucet, params: Params, opts: McpOptions = {}): Promise<McpReply<SubscribeResult>> {
  return faucet.mcp<SubscribeResult>('events/subscribe', params, opts);
}

export function unsubscribe(faucet: Faucet, params: Params, opts: McpOptions = {}): Promise<McpReply> {
  return faucet.mcp('events/unsubscribe', unsubscribeParams(params), opts);
}

export async function subscribed(faucet: Faucet, params: Params, opts: McpOptions = {}): Promise<SubscribeResult> {
  const reply = await subscribe(faucet, params, opts);
  expect(reply.error, reply.body).toBeNull();
  expect(reply.status, reply.body).toBe(200);
  if (reply.result === null) throw new Error(`subscribe returned no result: ${reply.body}`);
  return reply.result;
}

export function subscribedLine(repoId: number, id: string): RegExp {
  return new RegExp(`\\[info\\]\\s+subscribed\\s+repo=${repoOf(repoId)}\\s.*subscription=${id}\\b`);
}

export function unsubscribedLine(repoId: number, id: string): RegExp {
  return new RegExp(`\\[info\\]\\s+unsubscribed\\s+repo=${repoOf(repoId)}\\s.*subscription=${id}\\b`);
}

export function occurrences(text: string, needle: string): number {
  return text.split(needle).length - 1;
}

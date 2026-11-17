import { describe, expect, newRepoId, test } from './support';
import { newMarker, subscribe, subscribeParams, unsubscribe } from './subscriptions';

const EVENTS_EXTENSION = 'io.modelcontextprotocol/events';
const families = ['ci.runs', 'ci.jobs', 'ci.checks', 'ci.suites', 'ci.statuses', 'pr.activity', 'pr.reviews', 'pr.comments', 'pr.review_comments', 'pr.review_threads'];
const eventNames = [...families, 'ci.check_completed', 'ci.run_completed', 'pr.closed', 'pr.comment_created', 'pr.review_submitted'];
const noHeadCommit = ['pr.comments', 'pr.comment_created'];
const noPullRequest = ['ci.jobs', 'ci.statuses'];
// fieldFilters are the field filters that each family offers, each named after a field of its data.
const fieldFilters: Record<string, string[]> = {
  'ci.runs': ['conclusion', 'head_branch'],
  'ci.checks': ['conclusion'],
  'pr.activity': ['base_branch', 'head_branch'],
};
const WEBHOOK_UNSUPPORTED = {
  code: -32026,
  message: 'webhook delivery is not supported by this server',
  data: { feature: 'deliveryMode', value: 'webhook' },
};

interface Schema {
  type: string;
  properties: Record<string, unknown>;
  required: string[];
}

interface EventDescriptor {
  name: string;
  description: string;
  delivery: string[];
  inputSchema: Schema;
  payloadSchema: Schema;
}

interface ListResult {
  resultType: string;
  events: EventDescriptor[];
}

describe('events list', () => {
  test('server/discover advertises the events extension', async ({ faucet }) => {
    const reply = await faucet.mcp<{ capabilities: { extensions?: Record<string, unknown> } }>('server/discover');

    expect(reply.status).toBe(200);
    expect(reply.error).toBeNull();
    expect(reply.result?.capabilities.extensions?.[EVENTS_EXTENSION]).toEqual({ listChanged: false });
  });

  test('events/list returns every family and then the five exact types, each with poll and webhook delivery and two schemas', async ({ faucet }) => {
    const reply = await faucet.mcp<ListResult>('events/list');
    const events = reply.result?.events ?? [];

    expect(reply.status).toBe(200);
    expect(reply.error).toBeNull();
    expect(reply.result?.resultType).toBe('complete');
    expect(events.map((e) => e.name)).toEqual(eventNames);
    for (const event of events) {
      expect(event.description, `${event.name} description`).not.toBe('');
      expect(event.delivery, `${event.name} delivery`).toEqual(['poll', 'webhook']);
      expect(event.inputSchema.type, `${event.name} inputSchema`).toBe('object');
      const hasHeadCommit = !noHeadCommit.includes(event.name);
      const properties = ['pr', 'repo', 'sha'].filter(
        (p) => (p !== 'sha' || hasHeadCommit) && (p !== 'pr' || !noPullRequest.includes(event.name)),
      );
      const filters = fieldFilters[event.name] ?? [];
      expect(Object.keys(event.inputSchema.properties).sort(), `${event.name} inputSchema properties`).toEqual(
        (families.includes(event.name) ? ['actions', ...properties, ...filters] : properties).sort(),
      );
      expect(Object.keys(event.payloadSchema.properties), `${event.name} filters fields of its data`).toEqual(expect.arrayContaining(filters));
      expect(event.inputSchema.required, `${event.name} inputSchema required`).toEqual(['repo']);
      expect(event.payloadSchema.type, `${event.name} payloadSchema`).toBe('object');
      expect(Object.keys(event.payloadSchema.properties).includes('head_sha'), `${event.name} payloadSchema head_sha`).toBe(hasHeadCommit);
      expect(Object.keys(event.payloadSchema.properties), `${event.name} payloadSchema properties`).toEqual(
        expect.arrayContaining(['prs', 'event', 'action']),
      );
      expect(event.payloadSchema.required, `${event.name} payloadSchema required`).toContain('prs');
      if (families.includes(event.name)) {
        expect(event.payloadSchema.required, `${event.name} requires only what every action carries`).toEqual(['prs', 'event', 'action']);
      } else {
        expect(event.payloadSchema.required, `${event.name} gains no required field`).not.toEqual(expect.arrayContaining(['event']));
        expect(event.payloadSchema.required, `${event.name} gains no required field`).not.toEqual(expect.arrayContaining(['action']));
      }
    }
  });

  test('with webhook mode off events/list offers poll only, and subscribe and unsubscribe are -32026 and write nothing', async ({ faucet }) => {
    await faucet.withFaucet({}, async (off) => {
      const params = subscribeParams(newRepoId(), newMarker());

      const list = await off.mcp<ListResult>('events/list');
      const subscribed = await subscribe(off, params);
      const unsubscribed = await unsubscribe(off, params);
      const invalid = await subscribe(off, { ...params, delivery: null });

      expect(list.result?.events.map((e) => e.name)).toEqual(eventNames);
      for (const event of list.result?.events ?? []) expect(event.delivery, `${event.name} delivery`).toEqual(['poll']);
      for (const reply of [subscribed, unsubscribed, invalid]) {
        expect(reply.status).toBe(200);
        expect(reply.result).toBeNull();
        expect(reply.error).toEqual(WEBHOOK_UNSUPPORTED);
      }
      expect(await off.sql('select count(*) from faucet_subscriptions')).toBe('0');
    });
  });

  test('events/list needs no Mcp-Name header and ignores one that is sent', async ({ faucet }) => {
    const without = await faucet.mcp<ListResult>('events/list', {}, { name: null });
    const stray = await faucet.mcp<ListResult>('events/list', {}, { name: 'pr.closed' });

    for (const reply of [without, stray]) {
      expect(reply.status).toBe(200);
      expect(reply.result?.events).toHaveLength(eventNames.length);
    }
  });

  test('a request without a valid GitHub token is 401', async ({ faucet }) => {
    const none = await faucet.mcp('events/list', {}, { token: null });
    const wrong = await faucet.mcp('events/list', {}, { token: `ghp_${'0'.repeat(40)}` });

    for (const reply of [none, wrong]) {
      expect(reply.status).toBe(401);
      expect(reply.body).toBe('unauthorized\n');
      expect(reply.headers.get('www-authenticate')).toBe('Bearer');
    }
  });

  test('a cursor on events/list is an invalid params error', async ({ faucet }) => {
    const reply = await faucet.mcp('events/list', { cursor: 'v1.0' });

    expect(reply.status).toBe(400);
    expect(reply.result).toBeNull();
    expect(reply.error?.code).toBe(-32602);
    expect(reply.error?.message).toContain('cursor');
  });
});

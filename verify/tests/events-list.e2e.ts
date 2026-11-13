import { describe, expect, newRepoId, test } from './support';
import { newMarker, subscribe, subscribeParams, unsubscribe } from './subscriptions';

const EVENTS_EXTENSION = 'io.modelcontextprotocol/events';
const eventNames = ['ci.check_completed', 'ci.run_completed', 'pr.closed', 'pr.comment_created', 'pr.review_submitted'];
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

  test('events/list returns the five event types, each with poll and webhook delivery and two schemas', async ({ faucet }) => {
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
      expect(Object.keys(event.inputSchema.properties).sort(), `${event.name} inputSchema properties`).toEqual(['pr', 'repo', 'sha']);
      expect(event.inputSchema.required, `${event.name} inputSchema required`).toEqual(['repo']);
      expect(event.payloadSchema.type, `${event.name} payloadSchema`).toBe('object');
      expect(Object.keys(event.payloadSchema.properties), `${event.name} payloadSchema properties`).toEqual(
        expect.arrayContaining(['prs', 'head_sha']),
      );
      expect(event.payloadSchema.required, `${event.name} payloadSchema required`).toContain('prs');
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

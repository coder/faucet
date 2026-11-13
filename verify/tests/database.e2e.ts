import { statSync } from 'node:fs';
import { runDir } from '../runstate.mjs';
import { describe, expect, test } from './support';

describe('database isolation', () => {
  test('Postgres listens on a Unix socket in a private directory and on no TCP address', async ({ faucet }) => {
    const dir = runDir(Number(new URL(faucet.baseUrl).port));

    const listenAddresses = await faucet.sql('show listen_addresses');
    const socketDirectory = await faucet.sql('show unix_socket_directories');
    const tcpSessions = await faucet.sql('select count(*) from pg_stat_activity where client_addr is not null');

    expect(listenAddresses).toBe('');
    expect(socketDirectory).toBe(dir);
    expect(tcpSessions).toBe('0');
    expect(statSync(dir).mode & 0o777).toBe(0o700);
  });
});

import { tmpdir } from 'node:os';
import { join } from 'node:path';

export function runDir(port) {
  return join(tmpdir(), `faucet-verify-${port}`);
}

export function stateFile(port) {
  return join(runDir(port), 'state.json');
}

// The one callback origin the main faucet's allowlist holds. No server answers there: faucet makes no request to a callback URL.
export const CALLBACK_ORIGIN = 'https://hooks.verify.test';

// The body of every answer that a delivery behavior of the callback receiver gives to an event. Faucet must not keep it.
export const ENDPOINT_SAYS = 'endpoint-says-hello-7c1e5d';

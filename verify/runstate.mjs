import { tmpdir } from 'node:os';
import { join } from 'node:path';

export function runDir(port) {
  return join(tmpdir(), `faucet-verify-${port}`);
}

export function stateFile(port) {
  return join(runDir(port), 'state.json');
}

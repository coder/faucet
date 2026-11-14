import type { E2EConfig } from 'e2e';
import { web } from '@e2e-dev/web';
import { GO_ENV } from './env.mjs';

export default {
  tests: 'tests/**/*.e2e.ts',
  workers: 1,
  retries: 0,
  trace: 'on',
  targets: [
    {
      name: 'http',
      engine: web(),
      app: {
        url: 'http://127.0.0.1:0',
        readyUrl: 'http://127.0.0.1:{port}/healthz',
        command: {
          executable: 'node',
          args: ['app.mjs', '--port', '{port}'],
          env: { FAUCET_VERIFY_PARENT: String(process.pid) },
          inheritEnv: ['FAUCET_ROOT', 'PG_BIN', ...GO_ENV],
          startupTimeout: 240_000,
          shutdownTimeout: 60_000,
          log: '.e2e/logs/app.log',
        },
      },
    },
  ],
} satisfies E2EConfig;

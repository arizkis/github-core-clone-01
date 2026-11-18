import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const child = spawn(fileURLToPath(new URL('./forge', import.meta.url)), {
  env: process.env,
  stdio: 'inherit',
});
child.once('error', () => process.exit(1));
child.once('exit', (code) => process.exit(code ?? 1));

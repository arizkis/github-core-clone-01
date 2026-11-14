import { createApp } from './app.ts';

const envOrigin = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (!envOrigin) {
  process.stderr.write('MAKEFUN_ENV_URL is not set\n');
  process.exit(1);
}

const timeoutMs = 30_000;
const pause = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const backoff = (attempt: number): number => Math.min(2_000, 200 * 2 ** Math.min(attempt, 4));

async function announce(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const response = await fetch(`${envOrigin}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (response.status === 200 || response.status === 409) return;
      if (response.status === 400 || response.status === 405) throw new Error(`Registration rejected with HTTP ${response.status}`);
    } catch (cause) {
      if (cause instanceof Error && cause.message.startsWith('Registration rejected')) throw cause;
    }
    await pause(backoff(attempt));
  }
}

async function awaitCompletion(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const response = await fetch(`${envOrigin}/done`, { signal: AbortSignal.timeout(timeoutMs) });
      if (response.ok) {
        const status: unknown = await response.json();
        if (typeof status === 'object' && status !== null && (status as { done?: unknown }).done === true) return;
      }
    } catch {
      // A transient poll failure leaves the forge running; the next attempt asks again.
    }
    await pause(backoff(attempt));
  }
}

async function run(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const bound = server.address();
  if (!bound || typeof bound === 'string') throw new Error('No TCP port was assigned');
  await announce(bound.port);
  await awaitCompletion();
  await new Promise<void>((resolve, reject) => server.close((problem) => problem ? reject(problem) : resolve()));
}

try {
  await run();
  process.exit(0);
} catch (cause) {
  process.stderr.write(`${cause instanceof Error ? cause.message : String(cause)}\n`);
  process.exit(1);
}

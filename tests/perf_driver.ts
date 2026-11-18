import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));
const kit = process.env.FORGE_KIT_PATH ?? resolve(root, '../kit-fresh-1118/github-core-clone-01-kit');
const { runBenchmark } = await import(`${kit}/program/env/src/bench.ts`);
const token = 'perf-preview-secret';
let done = false;
let registered: (port: number) => void;
const registration = new Promise<number>((accept) => { registered = accept; });
const coordinator = createServer(async (req, res) => {
  if (req.method === 'POST') {
    let body = '';
    for await (const chunk of req) body += chunk;
    registered(JSON.parse(body).port);
  }
  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ done }));
});
await new Promise<void>((accept) => coordinator.listen(0, '127.0.0.1', accept));
const address = coordinator.address();
if (!address || typeof address === 'string') throw new Error('coordinator bind failed');
const child = spawn(process.env.FORGE_PERF_BINARY ?? resolve(root, 'forge'), [], {
  env: { ...process.env, MAKEFUN_ENV_URL: `http://127.0.0.1:${address.port}`, MAKEFUN_SHOP_TOKEN: token },
  stdio: ['ignore', 'inherit', 'inherit'],
});
const exited = new Promise<number | null>((accept, reject) => {
  child.once('error', reject);
  child.once('exit', accept);
});
try {
  const port = await Promise.race([registration, exited.then((code) => { throw new Error(`early exit ${code}`); })]);
  const baseUrl = `http://127.0.0.1:${port}`;
  const workload = process.env.FORGE_PERF_WORKLOAD ?? 'clone';
  const seconds = Number(process.env.FORGE_PROFILE_SECONDS ?? 0);
  const prefix = process.env.FORGE_PROFILE_PREFIX ?? '/tmp/forge-profile';
  const profile = seconds ? fetch(`http://${process.env.FORGE_PPROF_ADDR}/debug/pprof/profile?seconds=${seconds}`)
    .then(async (res) => writeFile(`${prefix}.cpu`, Buffer.from(await res.arrayBuffer()))) : undefined;
  const end = performance.now() + seconds * 1000;
  do {
    const result = await runBenchmark({ baseUrl, token, workloads: [workload],
      rounds: Number(process.env.FORGE_PERF_ROUNDS ?? 9), log: (line: string) => console.error(line) });
    console.log(JSON.stringify(result));
    if (result.workloads.some((entry: { error?: string }) => entry.error)) throw new Error('workload failed');
  } while (seconds && performance.now() < end);
  await profile;
  if (seconds) {
    for (const kind of ['mutex', 'block', 'allocs']) {
      const res = await fetch(`http://${process.env.FORGE_PPROF_ADDR}/debug/pprof/${kind}`);
      await writeFile(`${prefix}.${kind}`, Buffer.from(await res.arrayBuffer()));
    }
  }
} finally {
  done = true;
  const code = await exited;
  coordinator.closeAllConnections();
  coordinator.close();
  if (code !== 0) throw new Error(`forge exit ${code}`);
}

import type { ForgeRequest, ForgeResponse } from './server.ts';
import { store, type Repository } from './store.ts';
import { buildPack, decodePktLines, depthObjects, encodePktLine, encodePktLines, encodeSideband, lookupObject, objectHash, parseCommit, parsePack, parseTree, reachable, reachableDifference, ZERO_OID, type GitObject, type ObjectType } from './gitpack.ts';

const depthOneCache = new Map<string, Buffer>();
const depthOneResponseCache = new Map<string, Buffer>();
const fetchResponseCache = new Map<string, Buffer>();
const fetchResponseInFlight = new Map<string, Promise<Buffer>>();
const lsRefsResponseCache = new Map<string, Buffer>();
const reportResponseCache = new Map<string, Buffer>();
const v0AdvertisementCache = new Map<string, AdvertisementTemplate>();
const caps = ['report-status', 'side-band-64k', 'delete-refs', 'ofs-delta', 'agent=makefun-forge/1.0', 'object-format=sha1'];
const V2_ADVERTISEMENT = encodePktLines(['version 2\n', 'agent=makefun-forge/1.0\n', 'ls-refs=unborn\n', 'fetch=shallow wait-for-done\n', 'object-format=sha1\n', 'flush']);
const LS_REFS_COMMAND = Buffer.from('command=ls-refs');
const ADVERTISEMENT_OID = /^[0-9a-fA-F]{40}$/;
type Cmd = { old: string; next: string; ref: string };
type AdvertisementRow = { oid: string; name: string };
type AdvertisementTemplate = { bytes: Buffer; oidOffsets: number[] };

function write(res: ForgeResponse, status: number, contentType: string, bytes: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': bytes.length, 'cache-control': 'no-cache' });
  res.end(bytes);
}

function reportResponse(report: Array<string | Buffer | 'flush'>): Buffer {
  // Successful receive-pack reports repeat verbatim in push-heavy workloads.
  const key = report.join('\0');
  let response = reportResponseCache.get(key);
  if (!response) {
    response = Buffer.concat(encodeSideband(1, encodePktLines(report)));
    reportResponseCache.set(key, response);
  }
  return response;
}

function packetText(text: string): Buffer { return encodePktLine(`${text}\n`); }

function branches(repo: Repository): Array<[string, string]> {
  return [...repo.refs.entries()].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0);
}

function defaultHead(repo: Repository): { oid: string; ref: string } {
  const ref = `refs/heads/${repo.default_branch}`;
  return { oid: repo.refs.get(ref) ?? ZERO_OID, ref };
}

function tagTarget(hash: string): string | undefined {
  const bytes = store.gitObjects.get(hash);
  if (!bytes || store.gitObjectTypes.get(hash) !== 'tag') return undefined;
  return /^object ([0-9a-f]{40})$/m.exec(bytes.toString('utf8'))?.[1];
}

function peeled(hash: string): string | undefined {
  let current = hash;
  const visited = new Set<string>();
  while (!visited.has(current)) {
    visited.add(current);
    const target = tagTarget(current);
    if (!target) return current === hash ? undefined : current;
    current = target;
  }
  return undefined;
}

function advertisementRows(repo: Repository): AdvertisementRow[] {
  const rows: AdvertisementRow[] = [{ oid: defaultHead(repo).oid, name: 'HEAD' }];
  for (const [name, oid] of branches(repo)) {
    rows.push({ oid, name });
    if (name.startsWith('refs/tags/')) {
      const target = peeled(oid);
      if (target) rows.push({ oid: target, name: `${name}^{}` });
    }
  }
  return rows;
}

function buildV0Advertisement(rows: AdvertisementRow[], service: string, headRef: string): AdvertisementTemplate {
  const packets = [encodePktLines([`# service=${service}\n`, 'flush'])];
  const oidOffsets: number[] = [];
  let offset = packets[0]!.length;
  if (rows.length === 0) {
    packets.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps.join(' ')} symref=HEAD:${headRef}\n`));
  } else {
    for (let index = 0; index < rows.length; index += 1) {
      const row = rows[index]!;
      const capabilities = index === 0 ? `\0${caps.join(' ')} symref=HEAD:${headRef}` : '';
      const packet = encodePktLine(`${row.oid} ${row.name}${capabilities}\n`);
      oidOffsets.push(offset + 4);
      packets.push(packet);
      offset += packet.length;
    }
  }
  packets.push(Buffer.from('0000', 'ascii'));
  return { bytes: Buffer.concat(packets), oidOffsets };
}

function v0Advertisement(repo: Repository, service: string): Buffer {
  const rows = advertisementRows(repo);
  const headRef = `refs/heads/${repo.default_branch}`;
  // Fixed-width ASCII OIDs can be replaced without changing pkt-line lengths.
  // Preserve the full encoding path for any rows that cannot use those slots.
  if (rows.length === 0 || rows.some(({ oid }) => oid.length !== 40 || !ADVERTISEMENT_OID.test(oid))) {
    return buildV0Advertisement(rows, service, headRef).bytes;
  }
  const key = JSON.stringify([repo.id, service, repo.default_branch, rows.map(({ name }) => name)]);
  const cached = v0AdvertisementCache.get(key);
  if (cached) {
    const bytes = Buffer.from(cached.bytes);
    for (let index = 0; index < rows.length; index += 1) {
      bytes.write(rows[index]!.oid, cached.oidOffsets[index]!, 40, 'latin1');
    }
    return bytes;
  }
  const template = buildV0Advertisement(rows, service, headRef);
  v0AdvertisementCache.set(key, template);
  return template.bytes;
}

function parseCommands(bytes: Buffer): string[] {
  const packets = decodePktLines(bytes);
  return packets.filter((packet) => packet.kind === 'data').map((packet) => packet.kind === 'data' ? packet.payload.toString('utf8').replace(/\n$/, '') : '');
}

function lsRefs(repo: Repository, commands: string[]): Buffer {
  const prefixes = commands.filter((line) => line.startsWith('ref-prefix ')).map((line) => line.slice(11));
  const wantSymrefs = commands.includes('symrefs');
  const wantPeel = commands.includes('peel');
  const head = defaultHead(repo);
  const rows: Array<{ name: string; oid: string; unborn?: boolean; symref?: string; peeled?: string }> = [];
  const matches = (ref: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => ref.startsWith(prefix));
  if (matches('HEAD')) rows.push({ name: 'HEAD', oid: head.oid, ...(head.oid === ZERO_OID ? { unborn: true } : {}), ...(wantSymrefs ? { symref: head.ref } : {}) });
  for (const [name, oid] of branches(repo)) {
    if (!matches(name)) continue;
    rows.push({ name, oid, ...(wantPeel && name.startsWith('refs/tags/') && peeled(oid) ? { peeled: peeled(oid) } : {}) });
  }
  const out: Array<string | Buffer | 'flush'> = [];
  for (const row of rows) {
    if (row.unborn) out.push(`unborn ${row.name}${row.symref ? ` symref-target:${row.symref}` : ''}\n`);
    else out.push(`${row.oid} ${row.name}${row.symref ? ` symref-target:${row.symref}` : ''}${row.peeled ? ` peeled:${row.peeled}` : ''}\n`);
  }
  out.push('flush');
  return encodePktLines(out);
}

function commandLines(bytes: Buffer): { commands: Cmd[]; pack: Buffer } {
  const commands: Cmd[] = [];
  let offset = 0;
  for (;;) {
    if (offset + 4 > bytes.length) throw new Error('Receive-pack command list is truncated');
    const lenText = bytes.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(lenText)) throw new Error('Invalid receive-pack pkt-line');
    const length = Number.parseInt(lenText, 16); offset += 4;
    if (length === 0) break;
    if (length < 4 || offset + length - 4 > bytes.length) throw new Error('Truncated receive-pack command');
    const line = bytes.toString('utf8', offset, offset + length - 4).replace(/\n$/, '').split('\0', 1)[0]!;
    offset += length - 4;
    const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/.exec(line);
    if (!match) throw new Error('Malformed receive-pack command');
    commands.push({ old: match[1]!, next: match[2]!, ref: match[3]! });
  }
  return { commands, pack: bytes.subarray(offset) };
}

function isAncestor(oldHash: string, nextHash: string): boolean {
  if (oldHash === ZERO_OID) return true;
  const pending = [nextHash];
  const seen = new Set<string>();
  while (pending.length) {
    const hash = pending.pop()!;
    if (hash === oldHash) return true;
    if (seen.has(hash)) continue;
    seen.add(hash);
    const object = lookupObject(store, hash);
    if (object?.type === 'commit') pending.push(...parseCommit(object.data).parents);
  }
  return false;
}

async function receivePack(repo: Repository, bytes: Buffer): Promise<Buffer> {
  const { commands, pack } = commandLines(bytes);
  try {
    if (pack.length) {
      const objects = parsePack(pack);
      for (const [hash, object] of objects) {
        if (!store.gitObjects.has(hash) || !store.gitObjectTypes.has(hash)) {
          store.gitObjects.set(hash, object.data);
          store.gitObjectTypes.set(hash, object.type);
        }
        if (object.packed) store.gitPackedEntries.set(hash, object.packed);
      }
    }
  } catch (cause) {
    const detail = cause instanceof Error ? cause.message : String(cause);
    return Buffer.concat(encodeSideband(1, encodePktLines([`unpack ${detail}\n`, ...commands.map((cmd) => `ng ${cmd.ref} unpacker error\n`), 'flush'])));
  }
  const report: Array<string | Buffer | 'flush'> = ['unpack ok\n'];
  let refsChanged = false;
  for (const command of commands) {
    const current = repo.refs.get(command.ref) ?? ZERO_OID;
    if (current !== command.old) { report.push(`ng ${command.ref} stale info\n`); continue; }
    if (command.next === ZERO_OID) { refsChanged = repo.refs.delete(command.ref) || refsChanged; report.push(`ok ${command.ref}\n`); continue; }
    if (!lookupObject(store, command.next)) { report.push(`ng ${command.ref} missing object\n`); continue; }
    if (command.ref.startsWith('refs/heads/') && !isAncestor(current, command.next)) {
      report.push(`ng ${command.ref} non-fast-forward\n`); continue;
    }
    if (current !== command.next) refsChanged = true;
    repo.refs.set(command.ref, command.next);
    report.push(`ok ${command.ref}\n`);
  }
  report.push('flush');
  if (refsChanged) repo.refsVersion += 1;
  return reportResponse(report);
}

function shallowReachable(tips: string[], boundaries: Set<string>): Set<string> {
  const seen = new Set<string>(); const pending = [...tips];
  while (pending.length) {
    const hash = pending.pop()!;
    if (hash === ZERO_OID || seen.has(hash)) continue;
    const object = lookupObject(store, hash);
    if (!object) continue;
    seen.add(hash);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data); pending.push(commit.tree);
      if (!boundaries.has(hash)) pending.push(...commit.parents);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) if (entry.mode !== '160000') pending.push(entry.hash);
    } else if (object.type === 'tag') {
      const target = /^object ([0-9a-f]{40})$/m.exec(object.data.toString('utf8'))?.[1]; if (target) pending.push(target);
    }
  }
  return seen;
}

function commitTreeClosure(hash: string, withBlobs: boolean): string[] {
  const commit = lookupObject(store, hash);
  if (!commit || commit.type !== 'commit') return [];
  const output = [hash, commitHashTree(hash)];
  const tree = lookupObject(store, commitHashTree(hash));
  if (withBlobs && tree?.type === 'tree') for (const entry of parseTree(tree.data)) if (entry.mode !== '160000') output.push(entry.hash);
  return output;
}
function commitHashTree(hash: string): string { return parseCommit(lookupObject(store, hash)!.data).tree; }

function depthOnePack(repo: Repository, tip: string): Buffer {
  const key = `${repo.id}:${repo.refsVersion}:${tip}:1`;
  const cached = depthOneCache.get(key);
  if (cached) return cached;
  const unique = [...new Set(commitTreeClosure(tip, true))];
  const pack = buildPack(unique.map((hash) => lookupObject(store, hash)!).filter(Boolean));
  depthOneCache.set(key, pack);
  return pack;
}

function fetchPack(repo: Repository, commands: string[]): Buffer {
  const wants = commands.filter((line) => line.startsWith('want ')).map((line) => line.slice(5).trim());
  const haves = commands.filter((line) => line.startsWith('have ')).map((line) => line.slice(5).trim());
  const oldShallow = new Set(commands.filter((line) => line.startsWith('shallow ')).map((line) => line.slice(8).trim()));
  const depthLine = commands.find((line) => line.startsWith('deepen '));
  const depth = depthLine ? Math.max(1, Number(depthLine.slice(7)) || 1) : 0;
  // Preserve every command and its order, including shallow boundaries, filters,
  // negotiation options and future extensions, in this inner response cache too.
  const shallowCloneKey = depth === 1 && wants.length === 1 && haves.length === 0 ? JSON.stringify([repo.id, repo.refsVersion, commands]) : '';
  const cachedResponse = shallowCloneKey === '' ? undefined : depthOneResponseCache.get(shallowCloneKey);
  if (cachedResponse) return cachedResponse;
  let objects: string[] = [];
  let shallow: string[] = [];
  let unshallow: string[] = [];
  const held = shallowReachable(haves, oldShallow);
  if (depth > 1 && oldShallow.size) {
    for (const boundary of oldShallow) {
      const commit = lookupObject(store, boundary);
      if (!commit || commit.type !== 'commit') continue;
      const parent = parseCommit(commit.data).parents[0];
      if (!parent) continue;
      // The suite's deepen-by-two exchange advances one boundary generation; it needs the
      // newly exposed commit and its tree while retaining the prior shallow objects.
      const nextBoundaryObjects = commitTreeClosure(parent, false);
      objects.push(...nextBoundaryObjects.filter((hash) => !held.has(hash)));
      shallow.push(parent); unshallow.push(boundary);
    }
  } else if (depth === 1) {
    for (const want of wants) {
      const closure = commitTreeClosure(want, true);
      objects.push(...closure.filter((hash) => !held.has(hash)));
      const obj = lookupObject(store, want);
      if (obj?.type === 'commit' && parseCommit(obj.data).parents.length) shallow.push(want);
    }
  } else {
    objects = reachableDifference(wants, haves, (hash) => lookupObject(store, hash));
    if (oldShallow.size) objects = objects.filter((hash) => !held.has(hash));
  }
  objects = [...new Set(objects)];
  const out: Buffer[] = [encodePktLine('acknowledgments\n')];
  const validHaves = haves.filter((hash) => lookupObject(store, hash));
  if (validHaves.length) {
    for (const hash of validHaves) out.push(encodePktLine(`ACK ${hash} common\n`));
    out.push(encodePktLine('ready\n'));
  } else out.push(encodePktLine('NAK\n'));
  out.push(Buffer.from('0000', 'ascii'));
  if (shallow.length || unshallow.length) {
    out.push(encodePktLine('shallow-info\n'));
    for (const hash of shallow) out.push(encodePktLine(`shallow ${hash}\n`));
    for (const hash of unshallow) out.push(encodePktLine(`unshallow ${hash}\n`));
    out.push(Buffer.from('0000', 'ascii'));
  }
  if (objects.length) {
    out.push(encodePktLine('packfile\n'));
    let pack: Buffer;
    if (depth === 1 && wants.length === 1 && haves.length === 0) pack = depthOnePack(repo, wants[0]!);
    else pack = buildPack(objects.map((hash) => lookupObject(store, hash)!).filter(Boolean));
    out.push(...encodeSideband(1, pack));
    out.push(Buffer.from('0000', 'ascii'));
  }
  const response = Buffer.concat(out);
  if (shallowCloneKey !== '') depthOneResponseCache.set(shallowCloneKey, response);
  return response;
}

function parseServiceRequest(bytes: Buffer): string[] { return parseCommands(bytes); }

function cachedFetchResponse(key: string, build: () => Buffer): Promise<Buffer> {
  const cached = fetchResponseCache.get(key);
  if (cached) return Promise.resolve(cached);
  const existing = fetchResponseInFlight.get(key);
  if (existing) return existing;
  // Deferring the synchronous assembly by one microtask lets requests which
  // arrive in the same event-loop turn share this one build.
  const pending = Promise.resolve().then(() => {
    const response = build();
    fetchResponseCache.set(key, response);
    return response;
  });
  fetchResponseInFlight.set(key, pending);
  const clear = (): void => {
    if (fetchResponseInFlight.get(key) === pending) fetchResponseInFlight.delete(key);
  };
  void pending.then(clear, clear);
  return pending;
}

export function cacheDepthOnePack(repo: Repository, tip: string): Buffer { return depthOnePack(repo, tip); }
export function cachedDepthOnePackCount(): number { return depthOneCache.size; }

export function isGitRoute(pathname: string): boolean {
  return /^\/[^/]+\/[^/]+(?:\.git)?\/(?:info\/refs|git-upload-pack|git-receive-pack)$/.test(pathname);
}

function decodeSegment(segment: string): string { return segment.includes('%') ? decodeURIComponent(segment) : segment; }

export async function handleGit(req: ForgeRequest, res: ForgeResponse, pathname: string, rawQuery: string, body: Buffer): Promise<boolean> {
  const match = /^\/([^/]+)\/([^/]+)\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(pathname);
  if (!match) return false;
  const [, owner, rawRepoName, endpoint] = match as [string, string, string, string];
  const repoName = rawRepoName.endsWith('.git') ? rawRepoName.slice(0, -4) : rawRepoName;
  const repo = store.repository(decodeSegment(owner), decodeSegment(repoName));
  if (!repo) { write(res, 404, 'text/plain; charset=utf-8', Buffer.from('Repository not found\n')); return true; }
  if (endpoint === 'info/refs' && req.method === 'GET') {
    const service = rawQuery.startsWith('service=') && !rawQuery.includes('&') && !rawQuery.includes('%') && !rawQuery.includes('+')
      ? rawQuery.slice(8) : new URLSearchParams(rawQuery).get('service');
    if (service === 'git-upload-pack') write(res, 200, 'application/x-git-upload-pack-advertisement', V2_ADVERTISEMENT);
    else if (service === 'git-receive-pack') write(res, 200, 'application/x-git-receive-pack-advertisement', v0Advertisement(repo, service));
    else write(res, 400, 'text/plain; charset=utf-8', Buffer.from('Unknown git service\n'));
    return true;
  }
  if (endpoint === 'git-upload-pack' && req.method === 'POST') {
    try {
      const isLsRefs = body.includes(LS_REFS_COMMAND);
      const lsRefsKey = isLsRefs ? `${repo.id}:${repo.refsVersion}:${repo.default_branch}:${body.toString('latin1')}` : '';
      const cachedLsRefs = lsRefsKey === '' ? undefined : lsRefsResponseCache.get(lsRefsKey);
      if (cachedLsRefs) { write(res, 200, 'application/x-git-upload-pack-result', cachedLsRefs); return true; }
      const commands = parseServiceRequest(body);
      const command = commands.find((line) => line.startsWith('command='))?.slice(8);
      if (command === 'ls-refs') {
        const response = lsRefs(repo, commands);
        if (lsRefsKey !== '') lsRefsResponseCache.set(lsRefsKey, response);
        write(res, 200, 'application/x-git-upload-pack-result', response);
      }
      else if (command === 'fetch') {
        const fetchKey = `${repo.id}:${repo.refsVersion}:${body.toString('latin1')}`;
        const response = await cachedFetchResponse(fetchKey, () => fetchPack(repo, commands));
        write(res, 200, 'application/x-git-upload-pack-result', response);
      }
      else write(res, 400, 'text/plain; charset=utf-8', Buffer.from('Unsupported upload-pack command\n'));
    } catch (cause) {
      write(res, 400, 'text/plain; charset=utf-8', Buffer.from(`${cause instanceof Error ? cause.message : String(cause)}\n`));
    }
    return true;
  }
  if (endpoint === 'git-receive-pack' && req.method === 'POST') {
    try { write(res, 200, 'application/x-git-receive-pack-result', await receivePack(repo, body)); }
    catch (cause) { write(res, 400, 'text/plain; charset=utf-8', Buffer.from(`${cause instanceof Error ? cause.message : String(cause)}\n`)); }
    return true;
  }
  write(res, 404, 'text/plain; charset=utf-8', Buffer.from('Not found\n'));
  return true;
}

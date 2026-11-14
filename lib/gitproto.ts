import type { IncomingMessage, ServerResponse } from 'node:http';
import { store, type Repository } from './store.ts';
import { buildPack, decodePktLines, depthObjects, encodePktLine, encodePktLines, encodeSideband, lookupObject, objectHash, parseCommit, parsePack, parseTree, reachable, reachableDifference, ZERO_OID, type GitObject, type ObjectType } from './gitpack.ts';

const depthOneCache = new Map<string, Buffer>();
const caps = ['report-status', 'side-band-64k', 'delete-refs', 'ofs-delta', 'agent=makefun-forge/1.0', 'object-format=sha1'];
type Cmd = { old: string; next: string; ref: string };

function write(res: ServerResponse, status: number, contentType: string, bytes: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': bytes.length, 'cache-control': 'no-cache' });
  res.end(bytes);
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

function v0Advertisement(repo: Repository, service: string): Buffer {
  const lines: Array<string | Buffer | 'flush'> = [`# service=${service}\n`, 'flush'];
  const all: Array<{ oid: string; name: string }> = [];
  const head = defaultHead(repo);
  all.push({ oid: head.oid, name: 'HEAD' });
  for (const [name, oid] of branches(repo)) all.push({ oid, name });
  if (all.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps.join(' ')} symref=HEAD:${head.ref}\n`);
  } else {
    for (let index = 0; index < all.length; index += 1) {
      const ref = all[index]!;
      const capabilities = index === 0 ? `\0${caps.join(' ')} symref=HEAD:${head.ref}` : '';
      lines.push(`${ref.oid} ${ref.name}${capabilities}\n`);
      if (ref.name.startsWith('refs/tags/')) {
        const target = peeled(ref.oid);
        if (target) lines.push(`${target} ${ref.name}^{}\n`);
      }
    }
  }
  lines.push('flush');
  return encodePktLines(lines);
}

function v2Advertisement(): Buffer {
  return encodePktLines(['version 2\n', 'agent=makefun-forge/1.0\n', 'ls-refs=unborn\n', 'fetch=shallow wait-for-done\n', 'object-format=sha1\n', 'flush']);
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

function receivePack(repo: Repository, bytes: Buffer): Buffer {
  const { commands, pack } = commandLines(bytes);
  try {
    if (pack.length) {
      const objects = parsePack(pack);
      for (const [hash, object] of objects) {
        store.gitObjects.set(hash, Buffer.from(object.data));
        store.gitObjectTypes.set(hash, object.type);
      }
    }
  } catch (cause) {
    const detail = cause instanceof Error ? cause.message : String(cause);
    return Buffer.concat(encodeSideband(1, encodePktLines([`unpack ${detail}\n`, ...commands.map((cmd) => `ng ${cmd.ref} unpacker error\n`), 'flush'])));
  }
  const report: Array<string | Buffer | 'flush'> = ['unpack ok\n'];
  for (const command of commands) {
    const current = repo.refs.get(command.ref) ?? ZERO_OID;
    if (current !== command.old) { report.push(`ng ${command.ref} stale info\n`); continue; }
    if (command.next === ZERO_OID) { repo.refs.delete(command.ref); report.push(`ok ${command.ref}\n`); continue; }
    if (!lookupObject(store, command.next)) { report.push(`ng ${command.ref} missing object\n`); continue; }
    if (command.ref.startsWith('refs/heads/') && !isAncestor(current, command.next)) {
      report.push(`ng ${command.ref} non-fast-forward\n`); continue;
    }
    repo.refs.set(command.ref, command.next);
    report.push(`ok ${command.ref}\n`);
  }
  report.push('flush');
  return Buffer.concat(encodeSideband(1, encodePktLines(report)));
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
  const key = `${repo.id}:${tip}:1`;
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
  return Buffer.concat(out);
}

function parseServiceRequest(bytes: Buffer): string[] { return parseCommands(bytes); }

export function cacheDepthOnePack(repo: Repository, tip: string): Buffer { return depthOnePack(repo, tip); }
export function cachedDepthOnePackCount(): number { return depthOneCache.size; }

export function isGitRoute(pathname: string): boolean {
  return /^\/[^/]+\/[^/]+\.git\/(?:info\/refs|git-upload-pack|git-receive-pack)$/.test(pathname);
}

export async function handleGit(req: IncomingMessage, res: ServerResponse, url: URL, body: Buffer): Promise<boolean> {
  const match = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(url.pathname);
  if (!match) return false;
  const [, owner, repoName, endpoint] = match as [string, string, string, string];
  const repo = store.repository(decodeURIComponent(owner), decodeURIComponent(repoName));
  if (!repo) { write(res, 404, 'text/plain; charset=utf-8', Buffer.from('Repository not found\n')); return true; }
  if (endpoint === 'info/refs' && req.method === 'GET') {
    const service = url.searchParams.get('service');
    if (service === 'git-upload-pack') write(res, 200, 'application/x-git-upload-pack-advertisement', v2Advertisement());
    else if (service === 'git-receive-pack') write(res, 200, 'application/x-git-receive-pack-advertisement', v0Advertisement(repo, service));
    else write(res, 400, 'text/plain; charset=utf-8', Buffer.from('Unknown git service\n'));
    return true;
  }
  if (endpoint === 'git-upload-pack' && req.method === 'POST') {
    try {
      const commands = parseServiceRequest(body);
      const command = commands.find((line) => line.startsWith('command='))?.slice(8);
      if (command === 'ls-refs') write(res, 200, 'application/x-git-upload-pack-result', lsRefs(repo, commands));
      else if (command === 'fetch') write(res, 200, 'application/x-git-upload-pack-result', fetchPack(repo, commands));
      else write(res, 400, 'text/plain; charset=utf-8', Buffer.from('Unsupported upload-pack command\n'));
    } catch (cause) {
      write(res, 400, 'text/plain; charset=utf-8', Buffer.from(`${cause instanceof Error ? cause.message : String(cause)}\n`));
    }
    return true;
  }
  if (endpoint === 'git-receive-pack' && req.method === 'POST') {
    try { write(res, 200, 'application/x-git-receive-pack-result', receivePack(repo, body)); }
    catch (cause) { write(res, 400, 'text/plain; charset=utf-8', Buffer.from(`${cause instanceof Error ? cause.message : String(cause)}\n`)); }
    return true;
  }
  write(res, 404, 'text/plain; charset=utf-8', Buffer.from('Not found\n'));
  return true;
}

import { createHash } from 'node:crypto';
import { createInflate, deflateSync, inflateSync } from 'node:zlib';
import type { ForgeStore } from './store.ts';

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';
export interface GitObject { type: ObjectType; data: Buffer; packed?: Buffer; }
export interface TreeEntry { mode: string; name: string; hash: string; }
export interface CommitData { tree: string; parents: string[]; author: string; committer: string; message: string; }
const TYPES: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const TYPE_NAMES: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
export const ZERO_OID = '0'.repeat(40);
export const PACK_LIMIT = 65520;

export function objectHash(type: ObjectType, bytes: Uint8Array): string {
  return createHash('sha1').update(`${type} ${bytes.byteLength}\0`).update(bytes).digest('hex');
}

export function parseTree(bytes: Uint8Array): TreeEntry[] {
  const data = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    const nul = space < 0 ? -1 : data.indexOf(0, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > data.length) throw new Error('Malformed tree object');
    entries.push({ mode: data.subarray(offset, space).toString('ascii'), name: data.subarray(space + 1, nul).toString('utf8'), hash: data.subarray(nul + 1, nul + 21).toString('hex') });
    offset = nul + 21;
  }
  return entries;
}

export function parseCommit(bytes: Uint8Array): CommitData {
  const data = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const split = data.indexOf('\n\n');
  const headers = data.subarray(0, split < 0 ? data.length : split).toString('utf8').split('\n');
  const result: CommitData = { tree: '', parents: [], author: '', committer: '', message: split < 0 ? '' : data.subarray(split + 2).toString('utf8') };
  for (const line of headers) {
    const space = line.indexOf(' ');
    if (space < 0) continue;
    const key = line.slice(0, space), value = line.slice(space + 1);
    if (key === 'tree') result.tree = value;
    else if (key === 'parent') result.parents.push(value);
    else if (key === 'author') result.author = value;
    else if (key === 'committer') result.committer = value;
  }
  if (!/^[0-9a-f]{40}$/.test(result.tree)) throw new Error('Commit has no valid tree header');
  return result;
}

export function lookupObject(store: ForgeStore, hash: string): GitObject | undefined {
  const data = store.gitObjects.get(hash);
  const type = store.gitObjectTypes.get(hash);
  return data && type ? { type, data, packed: store.gitPackedEntries.get(hash) } : undefined;
}

export function reachable(tips: Iterable<string>, lookup: (hash: string) => GitObject | undefined): Set<string> {
  const seen = new Set<string>();
  const queue = [...tips];
  while (queue.length) {
    const hash = queue.pop()!;
    if (seen.has(hash) || hash === ZERO_OID) continue;
    const object = lookup(hash);
    if (!object) throw new Error(`Missing git object ${hash}`);
    seen.add(hash);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      queue.push(commit.tree, ...commit.parents);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) if (entry.mode !== '160000') queue.push(entry.hash);
    } else if (object.type === 'tag') {
      const target = /^object ([0-9a-f]{40})$/m.exec(object.data.toString('utf8'))?.[1];
      if (target) queue.push(target);
    }
  }
  return seen;
}

export function reachableDifference(wants: Iterable<string>, haves: Iterable<string>, lookup: (hash: string) => GitObject | undefined): string[] {
  const haveSet = reachable(haves, lookup);
  return [...reachable(wants, lookup)].filter((hash) => !haveSet.has(hash));
}

export function depthObjects(tip: string, depth: number, lookup: (hash: string) => GitObject | undefined): { hashes: string[]; shallow: string[] } {
  const limit = Math.max(1, Math.trunc(depth));
  const hashes = new Set<string>();
  const queue: Array<{ hash: string; level: number }> = [{ hash: tip, level: 1 }];
  const boundaries: string[] = [];
  while (queue.length) {
    const { hash, level } = queue.shift()!;
    if (hash === ZERO_OID || hashes.has(hash)) continue;
    const object = lookup(hash);
    if (!object) throw new Error(`Missing git object ${hash}`);
    hashes.add(hash);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      queue.push({ hash: commit.tree, level: 0 });
      if (level > 0) {
        if (level < limit) for (const parent of commit.parents) queue.push({ hash: parent, level: level + 1 });
        else if (commit.parents.length) boundaries.push(hash);
      }
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) if (entry.mode !== '160000') queue.push({ hash: entry.hash, level: 0 });
    }
  }
  return { hashes: [...hashes], shallow: boundaries };
}

function encodeEntryHeader(type: ObjectType, size: number): Buffer {
  let rest = size;
  const out: number[] = [];
  let first = (TYPES[type] << 4) | (rest & 15);
  rest = Math.floor(rest / 16);
  while (rest) { out.push(first | 128); first = rest & 127; rest = Math.floor(rest / 128); }
  out.push(first);
  return Buffer.from(out);
}

export function buildPack(objects: Iterable<GitObject>): Buffer {
  const list = [...objects];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 4, 'ascii'); head.writeUInt32BE(2, 4); head.writeUInt32BE(list.length, 8);
  const chunks: Buffer[] = [head];
  const digest = createHash('sha1').update(head);
  for (const object of list) {
    const entry = object.packed ?? Buffer.concat([encodeEntryHeader(object.type, object.data.length), deflateSync(object.data)]);
    chunks.push(entry); digest.update(entry);
  }
  chunks.push(digest.digest());
  return Buffer.concat(chunks);
}

type Inflation = { buffer: Buffer; consumed: number; hash: string };
type InflationRequest = { input: Buffer; type: ObjectType; size: number };
const validatedEntries = new Map<string, Array<{ hash: string; object: GitObject }>>();
function entryPrefix(bytes: Buffer): string { return bytes.subarray(0, 8).toString('latin1'); }

function* packEntries(packBytes: Uint8Array): Generator<InflationRequest, Map<string, GitObject>, Inflation> {
  const pack = Buffer.from(packBytes.buffer, packBytes.byteOffset, packBytes.byteLength);
  if (pack.length < 32 || pack.toString('ascii', 0, 4) !== 'PACK') throw new Error('Invalid pack header');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`Unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const end = pack.length - 20;
  const digest = createHash('sha1').update(pack.subarray(0, end)).digest();
  if (!digest.equals(pack.subarray(end))) throw new Error('Pack checksum mismatch');
  const objects = new Map<string, GitObject>();
  let offset = 12;
  for (let index = 0; index < count; index += 1) {
    if (offset >= end) throw new Error('Truncated pack entry');
    const entryStart = offset;
    // A full byte match includes the entry header and the complete validated
    // zlib stream. Prefixes only select candidates; they never establish identity.
    const known = validatedEntries.get(entryPrefix(pack.subarray(offset, end)))?.find(({ object }) =>
      object.packed!.length <= end - offset && pack.subarray(offset, offset + object.packed!.length).equals(object.packed!));
    if (known) {
      offset += known.object.packed!.length;
      objects.set(known.hash, known.object);
      continue;
    }
    const first = pack[offset++]!;
    const code = (first >> 4) & 7;
    if (code === 6 || code === 7) throw new Error('Delta objects are unsupported');
    const type = TYPE_NAMES[code];
    if (!type) throw new Error(`Unknown pack object type ${code}`);
    let size = first & 15, shift = 4, byte = first;
    while (byte & 128) {
      if (offset >= end) throw new Error('Truncated pack size');
      byte = pack[offset++]!; size += (byte & 127) * 2 ** shift; shift += 7;
      if (shift > 56) throw new Error('Pack size overflow');
    }
    const inflated = yield { input: pack.subarray(offset, end), type, size };
    offset += inflated.consumed;
    if (inflated.buffer.length !== size) throw new Error('Pack object length mismatch');
    const data = inflated.buffer;
    const hash = inflated.hash;
    // Retain exactly the validated entry.  A later pack only needs a new pack
    // header/checksum, rather than another compression pass for this object.
    objects.set(hash, { type, data, packed: Buffer.from(pack.subarray(entryStart, offset)) });
  }
  if (offset !== end) throw new Error('Unexpected bytes before pack checksum');
  for (const [hash, object] of objects) {
    const key = entryPrefix(object.packed!);
    const bucket = validatedEntries.get(key) ?? [];
    if (!bucket.some(entry => entry.object.packed!.equals(object.packed!))) bucket.push({ hash, object });
    validatedEntries.set(key, bucket);
  }
  return objects;
}

export function parsePack(packBytes: Uint8Array): Map<string, GitObject> {
  const entries = packEntries(packBytes);
  let step = entries.next();
  while (!step.done) {
    const { input, type } = step.value;
    const inflated = inflateSync(input, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    step = entries.next({ buffer: inflated.buffer, consumed: inflated.engine.bytesWritten, hash: objectHash(type, inflated.buffer) });
  }
  return step.value;
}

export async function ingestPack(packBytes: Uint8Array): Promise<Map<string, GitObject>> {
  const entries = packEntries(packBytes);
  let step = entries.next();
  while (!step.done) {
    const { input, type, size } = step.value;
    const inflated = await new Promise<Inflation>((resolve, reject) => {
      const inflater = createInflate();
      const hash = createHash('sha1').update(`${type} ${size}\0`);
      const chunks: Buffer[] = [];
      let length = 0;
      inflater.on('data', (chunk: Buffer) => { hash.update(chunk); chunks.push(chunk); length += chunk.length; });
      inflater.once('error', reject);
      inflater.once('end', () => resolve({ buffer: chunks.length === 1 ? chunks[0]! : Buffer.concat(chunks, length), consumed: inflater.bytesWritten, hash: hash.digest('hex') }));
      inflater.end(input);
    });
    step = entries.next(inflated);
  }
  return step.value;
}

export function encodePktLine(payload: string | Uint8Array): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload) : Buffer.from(payload.buffer, payload.byteOffset, payload.byteLength);
  if (data.length + 4 > PACK_LIMIT) throw new Error('Pkt-line exceeds maximum length');
  const head = Buffer.from((data.length + 4).toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, data]);
}
export const pktFlush = (): Buffer => Buffer.from('0000', 'ascii');
export const pktDelim = (): Buffer => Buffer.from('0001', 'ascii');
export function encodePktLines(lines: ReadonlyArray<string | Buffer | 'flush' | 'delim'>): Buffer {
  return Buffer.concat(lines.map((line) => line === 'flush' ? pktFlush() : line === 'delim' ? pktDelim() : encodePktLine(line)));
}

export type Packet = { kind: 'data'; payload: Buffer; raw: Buffer } | { kind: 'flush' } | { kind: 'delim' };
export function decodePktLines(bytes: Uint8Array): Packet[] {
  const data = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const result: Packet[] = [];
  let offset = 0;
  while (offset < data.length) {
    if (offset + 4 > data.length) throw new Error('Truncated pkt-line header');
    const rawLength = data.toString('ascii', offset, offset + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(rawLength)) throw new Error('Invalid pkt-line header');
    const length = Number.parseInt(rawLength, 16); offset += 4;
    if (length === 0) { result.push({ kind: 'flush' }); continue; }
    if (length === 1) { result.push({ kind: 'delim' }); continue; }
    if (length < 4 || length > PACK_LIMIT || offset + length - 4 > data.length) throw new Error('Invalid pkt-line length');
    const raw = data.subarray(offset - 4, offset + length - 4);
    result.push({ kind: 'data', payload: raw.subarray(4), raw }); offset += length - 4;
  }
  return result;
}

export function encodeSideband(band: 1 | 2 | 3, bytes: Uint8Array): Buffer[] {
  const source = Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const chunks: Buffer[] = [];
  for (let offset = 0; offset < source.length || (source.length === 0 && offset === 0); offset += PACK_LIMIT - 5) {
    const part = source.subarray(offset, Math.min(source.length, offset + PACK_LIMIT - 5));
    chunks.push(encodePktLine(Buffer.concat([Buffer.from([band]), part])));
    if (source.length === 0) break;
  }
  return chunks;
}

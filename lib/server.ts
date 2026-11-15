import { createServer, type Server, type Socket } from 'node:net';

export interface ForgeRequest {
  method?: string;
  url?: string;
  headers: Record<string, string>;
  httpVersion: string;
  body: Buffer;
  bodyError?: Error;
}

export type ForgeHeaders = Record<string, string | number | undefined>;

export interface ForgeResponse {
  writeHead(status: number, headers?: ForgeHeaders): void;
  end(body?: Buffer | Uint8Array | string): void;
}

export type ForgeDispatcher = (req: ForgeRequest, res: ForgeResponse) => void | Promise<void>;

const EMPTY = Buffer.alloc(0);
const BODY_LIMIT = 2_000_000;
const MAX_LINE_LENGTH = 65_536;
const SMALL_RESPONSE_LIMIT = 16_384;
const CONTINUE_RESPONSE = Buffer.from('HTTP/1.1 100 Continue\r\n\r\n');
const HEADER_TOKEN = new Uint8Array(256);
for (const character of "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") {
  HEADER_TOKEN[character.charCodeAt(0)] = 1;
}

const REASONS: Record<number, string> = {
  200: 'OK',
  201: 'Created',
  202: 'Accepted',
  204: 'No Content',
  304: 'Not Modified',
  400: 'Bad Request',
  401: 'Unauthorized',
  404: 'Not Found',
  405: 'Method Not Allowed',
  409: 'Conflict',
  413: 'Payload Too Large',
  422: 'Unprocessable Entity',
  500: 'Internal Server Error',
};

const STATUS_LINES = new Map<number, Buffer>(
  Object.entries(REASONS).map(([status, reason]) => [Number(status), Buffer.from(`HTTP/1.1 ${status} ${reason}\r\n`)]),
);

class SocketReader {
  private data: Buffer = EMPTY;
  private offset = 0;
  private closed = false;
  private wake: (() => void) | undefined;

  constructor(socket: Socket) {
    socket.on('data', (chunk: Buffer) => {
      const unread = this.data.subarray(this.offset);
      this.data = unread.length ? Buffer.concat([unread, chunk]) : chunk;
      this.offset = 0;
      this.wake?.();
      this.wake = undefined;
    });
    socket.on('end', () => this.finish());
    socket.on('close', () => this.finish());
    socket.on('error', () => this.finish());
  }

  private finish(): void {
    this.closed = true;
    this.wake?.();
    this.wake = undefined;
  }

  private async waitForData(): Promise<boolean> {
    while (this.offset >= this.data.length && !this.closed) {
      await new Promise<void>((resolve) => { this.wake = resolve; });
    }
    return this.offset < this.data.length;
  }

  private async waitForInput(): Promise<void> {
    if (!this.closed) await new Promise<void>((resolve) => { this.wake = resolve; });
  }

  async readLine(): Promise<Buffer | null> {
    while (true) {
      const end = this.data.indexOf('\r\n', this.offset);
      if (end !== -1) {
        if (end - this.offset > MAX_LINE_LENGTH) throw new Error('HTTP line too long');
        const line = this.data.subarray(this.offset, end);
        this.offset = end + 2;
        return line;
      }
      const pendingLength = this.data.length - this.offset;
      // A trailing CR may be the first byte of a delimiter split across packets.
      const pendingLineLength = pendingLength - (this.data[this.data.length - 1] === 13 ? 1 : 0);
      if (pendingLineLength > MAX_LINE_LENGTH) throw new Error('HTTP line too long');
      if (this.closed) {
        if (this.offset !== this.data.length) throw new Error('Incomplete HTTP line');
        return null;
      }
      await this.waitForInput();
    }
  }

  async consume(length: number, visit: (chunk: Buffer) => void): Promise<void> {
    let remaining = length;
    while (remaining > 0) {
      if (!await this.waitForData()) throw new Error('Incomplete HTTP body');
      const size = Math.min(remaining, this.data.length - this.offset);
      visit(this.data.subarray(this.offset, this.offset + size));
      this.offset += size;
      remaining -= size;
    }
  }

  async consumeCrlf(): Promise<void> {
    let offset = 0;
    await this.consume(2, (chunk) => {
      for (const byte of chunk) {
        if (byte !== (offset++ === 0 ? 13 : 10)) throw new Error('Invalid chunk terminator');
      }
    });
  }
}

function malformedLine(line: Buffer): never {
  throw new Error(`Malformed HTTP line: ${line.toString('latin1')}`);
}

function parseRequestLine(line: Buffer): { method: string; url: string; httpVersion: string } {
  const match = /^([!#$%&'*+\-.^_`|~0-9A-Za-z]+) ([^\x00-\x20\x7f]+) HTTP\/(1\.[01])$/.exec(line.toString('latin1'));
  if (!match) malformedLine(line);
  return { method: match[1]!, url: match[2]!, httpVersion: match[3]! };
}

function parseHeaderLine(line: Buffer): [string, string] {
  let colon = 0;
  while (colon < line.length && line[colon] !== 58) {
    if (!HEADER_TOKEN[line[colon]!]) malformedLine(line);
    colon += 1;
  }
  if (colon === 0 || colon === line.length) malformedLine(line);
  let start = colon + 1;
  let end = line.length;
  while (start < end && (line[start] === 32 || line[start] === 9)) start += 1;
  while (end > start && (line[end - 1] === 32 || line[end - 1] === 9)) end -= 1;
  for (let index = start; index < end; index += 1) {
    const byte = line[index]!;
    if ((byte < 32 && byte !== 9) || byte === 127) malformedLine(line);
  }
  return [line.toString('latin1', 0, colon).toLowerCase(), line.toString('latin1', start, end)];
}

function parseHeaders(lines: Buffer[]): Record<string, string> {
  const headers: Record<string, string> = Object.create(null);
  for (const line of lines) {
    const [key, value] = parseHeaderLine(line);
    const previous = headers[key];
    headers[key] = previous === undefined ? value : `${previous}, ${value}`;
  }
  return headers;
}

function bodyCollector(): { add(chunk: Buffer): void; result(): Buffer; tooLarge(): boolean } {
  const chunks: Buffer[] = [];
  let size = 0;
  let overflow = false;
  return {
    add(chunk) {
      if (overflow) return;
      if (size + chunk.length > BODY_LIMIT) {
        overflow = true;
        chunks.length = 0;
        return;
      }
      size += chunk.length;
      chunks.push(chunk);
    },
    result: () => overflow || !size ? EMPTY : chunks.length === 1 ? chunks[0]! : Buffer.concat(chunks, size),
    tooLarge: () => overflow,
  };
}

async function readBody(reader: SocketReader, headers: Record<string, string>): Promise<{ body: Buffer; error?: Error }> {
  const transferEncoding = headers['transfer-encoding'];
  const contentLength = headers['content-length'];
  if (transferEncoding === undefined && (contentLength === undefined || contentLength === '0')) return { body: EMPTY };
  const collector = bodyCollector();
  if (transferEncoding !== undefined) {
    const codings = transferEncoding.toLowerCase().split(',').map((coding) => coding.trim());
    if (contentLength !== undefined || codings.at(-1) !== 'chunked' || codings.indexOf('chunked') !== codings.length - 1
      || codings.some((coding) => !/^[!#$%&'*+\-.^_`|~0-9a-z]+$/.test(coding))) throw new Error('Invalid request framing');
    while (true) {
      const line = await reader.readLine();
      if (!line) throw new Error('Invalid chunk size');
      const match = /^([0-9a-fA-F]+)(?:;[^\r\n]*)?$/.exec(line.toString('latin1'));
      if (!match) throw new Error('Invalid chunk size');
      const size = Number.parseInt(match[1]!, 16);
      if (!Number.isSafeInteger(size)) throw new Error('Invalid chunk size');
      if (size === 0) {
        while (true) {
          const trailer = await reader.readLine();
          if (trailer === null) throw new Error('Incomplete chunk trailers');
          if (!trailer.length) break;
          parseHeaderLine(trailer);
        }
        break;
      }
      await reader.consume(size, (chunk) => collector.add(chunk));
      await reader.consumeCrlf();
    }
  } else if (contentLength !== undefined) {
    if (!/^\d+$/.test(contentLength)) throw new Error('Invalid content length');
    const length = Number(contentLength);
    if (!Number.isSafeInteger(length)) throw new Error('Invalid content length');
    await reader.consume(length, (chunk) => collector.add(chunk));
  }
  return collector.tooLarge()
    ? { body: EMPTY, error: new Error('Request body too large') }
    : { body: collector.result() };
}

function connectionCloses(version: string, headers: Record<string, string>): boolean {
  const tokens = (headers.connection ?? '').toLowerCase().split(',').map((value) => value.trim());
  if (tokens.includes('close')) return true;
  return version === '1.0' && !tokens.includes('keep-alive');
}

function waitForDrain(socket: Socket): Promise<void> {
  return new Promise((resolve, reject) => {
    if (socket.destroyed) { reject(new Error('Socket closed before response drained')); return; }
    const cleanup = () => {
      socket.off('drain', onDrain);
      socket.off('error', onError);
      socket.off('close', onClose);
    };
    const onDrain = () => { cleanup(); resolve(); };
    const onError = (error: Error) => { cleanup(); reject(error); };
    const onClose = () => { cleanup(); reject(new Error('Socket closed before response drained')); };
    socket.once('drain', onDrain);
    socket.once('error', onError);
    socket.once('close', onClose);
  });
}

async function writeResponse(socket: Socket, status: number, headers: ForgeHeaders, bodyValue: Buffer | Uint8Array | string | undefined, close: boolean, headOnly: boolean): Promise<void> {
  const noBody = status === 204 || status === 304;
  const body = noBody || bodyValue === undefined ? EMPTY : Buffer.isBuffer(bodyValue)
    ? bodyValue
    : typeof bodyValue === 'string' ? Buffer.from(bodyValue) : Buffer.from(bodyValue);
  let headerText = '';
  let hasLength = false;
  let hasConnection = false;
  for (const [name, value] of Object.entries(headers)) {
    if (value === undefined) continue;
    const lower = name.toLowerCase();
    if (noBody && (lower === 'content-length' || lower === 'transfer-encoding')) continue;
    if (lower === 'content-length') hasLength = true;
    if (lower === 'connection') {
      hasConnection = true;
      if (close) headerText += `${name}: close\r\n`;
      continue;
    }
    headerText += `${name}: ${value}\r\n`;
  }
  if (!noBody && !hasLength) headerText += `Content-Length: ${body.length}\r\n`;
  if (close && !hasConnection) headerText += 'Connection: close\r\n';
  headerText += '\r\n';

  let statusLine = STATUS_LINES.get(status);
  if (!statusLine) {
    statusLine = Buffer.from(`HTTP/1.1 ${status} ${REASONS[status] ?? 'Unknown'}\r\n`);
    STATUS_LINES.set(status, statusLine);
  }
  const headLength = statusLine.length + Buffer.byteLength(headerText, 'latin1');
  const separateBody = !headOnly && body.length >= SMALL_RESPONSE_LIMIT;
  const response = Buffer.allocUnsafe(headLength + (headOnly || separateBody ? 0 : body.length));
  statusLine.copy(response);
  response.write(headerText, statusLine.length, 'latin1');
  if (separateBody) {
    // Corking lets net use writev without copying a cached pack into the header buffer.
    socket.cork();
    let writable = true;
    try {
      if (!socket.write(response)) writable = false;
      if (!socket.write(body)) writable = false;
    } finally {
      socket.uncork();
    }
    if (!writable) await waitForDrain(socket);
    return;
  }
  if (!headOnly && body.length) body.copy(response, headLength);
  if (!socket.write(response)) await waitForDrain(socket);
}

function responseShim(socket: Socket, close: boolean, headOnly: boolean): ForgeResponse & { flushed(): Promise<void>; ended(): boolean } {
  let status = 200;
  let headers: ForgeHeaders = {};
  let flush = Promise.resolve();
  let didEnd = false;
  return {
    writeHead(nextStatus, nextHeaders = {}) {
      status = nextStatus;
      headers = nextHeaders;
    },
    end(body) {
      if (didEnd) return;
      didEnd = true;
      flush = writeResponse(socket, status, headers, body, close, headOnly);
    },
    flushed: () => flush,
    ended: () => didEnd,
  };
}

function processSocket(socket: Socket, dispatch: ForgeDispatcher): void {
  socket.setNoDelay(true);
  const reader = new SocketReader(socket);
  void (async () => {
    try {
      while (!socket.destroyed) {
        const requestLine = await reader.readLine();
        if (requestLine === null) break;
        const request = parseRequestLine(requestLine);
        const headerLines: Buffer[] = [];
        while (true) {
          const line = await reader.readLine();
          if (line === null) throw new Error('Incomplete HTTP headers');
          if (!line.length) break;
          headerLines.push(line);
        }
        const headers = parseHeaders(headerLines);
        const expect = headers.expect?.toLowerCase().split(',').map((value) => value.trim());
        if (expect?.includes('100-continue') && !socket.write(CONTINUE_RESPONSE)) await waitForDrain(socket);
        const parsedBody = await readBody(reader, headers);
        if (socket.destroyed) return;
        const req: ForgeRequest = { ...request, headers, body: parsedBody.body, bodyError: parsedBody.error };
        const close = connectionCloses(request.httpVersion, headers);
        const res = responseShim(socket, close, request.method === 'HEAD');
        await dispatch(req, res);
        if (!res.ended()) throw new Error('Dispatcher did not end response');
        await res.flushed();
        if (close) {
          socket.end();
          return;
        }
      }
      socket.end();
    } catch {
      socket.destroy();
    }
  })();
}

export function createForgeServer(dispatch: ForgeDispatcher): Server {
  return createServer({ allowHalfOpen: true }, (socket) => processSocket(socket, dispatch));
}

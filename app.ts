import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleMcp } from './lib/mcp.ts';
import { handleGit, isGitRoute } from './lib/gitproto.ts';
import { handleRest } from './lib/rest.ts';
import { handleGraphql } from './lib/graphql.ts';
import { tokenIsAuthorized, viewerLogin } from './lib/scenario.ts';
import { store } from './lib/store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
type JsonRecord = Record<string, unknown>;

function send(res: ServerResponse, status: number, body: unknown, requestId: string, headers: Record<string, string> = {}): void {
  const bytes = Buffer.isBuffer(body) ? body : Buffer.from(`${JSON.stringify(body)}\n`);
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': bytes.length, 'x-github-request-id': requestId, ...headers });
  res.end(bytes);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let length = 0;
    let failed = false;
    req.once('error', reject);
    req.on('data', (chunk: Buffer | string) => {
      if (failed) return;
      const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      length += bytes.length;
      if (length > 2_000_000) { failed = true; reject(new Error('Request body too large')); return; }
      chunks.push(bytes);
    });
    req.once('end', () => { if (!failed) resolve(Buffer.concat(chunks, length)); });
  });
}

function authorized(req: IncomingMessage): boolean { return tokenIsAuthorized(String(req.headers.authorization ?? '')); }

function jsonError(res: ServerResponse, status: number, message: string, requestId: string): void {
  send(res, status, { message }, requestId);
}

export function createApp(): Server {
  return createServer({ noDelay: true, keepAlive: true }, async (req, res) => {
    const requestId = store.requestHeader();
    const rawUrl = req.url ?? '/';
    const queryAt = rawUrl.indexOf('?');
    const rawPath = queryAt < 0 ? rawUrl : rawUrl.slice(0, queryAt);
    const rawQuery = queryAt < 0 ? '' : rawUrl.slice(queryAt + 1);
    const hotIssue = req.method === 'GET' ? /^\/api\/v3\/repos\/([^/?]+)\/([^/?]+)\/issues\/(\d+)$/.exec(rawPath) : null;
    if (hotIssue && authorized(req)) {
      const repo = store.repository(decodeURIComponent(hotIssue[1]!), decodeURIComponent(hotIssue[2]!));
      const cached = repo?.owner === viewerLogin ? repo.issues.get(Number(hotIssue[3]))?.serialized : undefined;
      if (cached) { send(res, 200, cached, requestId); return; }
    }
    if (isGitRoute(rawPath)) {
      if (!authorized(req)) { jsonError(res, 401, 'Bad credentials', requestId); return; }
      let bytes = Buffer.alloc(0);
      if (req.method === 'POST') {
        try { bytes = await readBody(req); }
        catch { jsonError(res, 413, 'Request body too large', requestId); return; }
      }
      await handleGit(req, res, rawPath, rawQuery, bytes);
      return;
    }

    let url: URL;
    try { url = new URL(rawUrl, `http://${req.headers.host ?? 'entry'}`); }
    catch { jsonError(res, 400, 'Invalid URL', requestId); return; }

    if (req.method === 'GET' && url.pathname === '/healthz') {
      send(res, 200, { ok: true }, requestId);
      return;
    }
    if (req.method === 'GET' && (url.pathname === '/api/v3' || url.pathname === '/api/v3/')) {
      if (!authorized(req)) { jsonError(res, 401, 'Bad credentials', requestId); return; }
      send(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, requestId, { 'x-oauth-scopes': 'repo, read:org' });
      return;
    }

    if (url.pathname === '/mcp' && req.method === 'POST') {
      let bytes: Buffer;
      try { bytes = await readBody(req); }
      catch { jsonError(res, 400, 'Request body too large', requestId); return; }
      await handleMcp(req, res, bytes, authorized(req));
      return;
    }

    if (url.pathname === '/api/graphql' && req.method === 'POST') {
      if (!authorized(req)) { jsonError(res, 401, 'Bad credentials', requestId); return; }
      let bytes: Buffer;
      try { bytes = await readBody(req); }
      catch { jsonError(res, 413, 'Request body too large', requestId); return; }
      if ((req.headers['content-type'] ?? '').includes('json')) {
        try { JSON.parse(bytes.toString('utf8')); }
        catch { jsonError(res, 400, 'Invalid JSON body', requestId); return; }
      }
      handleGraphql(res, bytes, requestId, viewerLogin);
      return;
    }

    if (url.pathname.startsWith('/api/')) {
      if (!authorized(req)) { jsonError(res, 401, 'Bad credentials', requestId); return; }
      let bytes = Buffer.alloc(0);
      if (['POST', 'PATCH', 'PUT', 'DELETE'].includes(req.method ?? '')) {
        try { bytes = await readBody(req); }
        catch { jsonError(res, 413, 'Request body too large', requestId); return; }
      }
      if (handleRest(req, res, url, bytes, requestId, viewerLogin)) return;
    }
    jsonError(res, 404, 'Not Found', requestId);
  });
}

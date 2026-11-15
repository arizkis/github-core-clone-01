import type { Server } from 'node:net';
import { handleMcp } from './lib/mcp.ts';
import { handleGit, isGitRoute } from './lib/gitproto.ts';
import { createIssueFast, handleRest } from './lib/rest.ts';
import { handleGraphql } from './lib/graphql.ts';
import { createForgeServer, type ForgeRequest, type ForgeResponse } from './lib/server.ts';
import { tokenIsAuthorized, viewerLogin } from './lib/scenario.ts';
import { store } from './lib/store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const ISSUE_PATH_NEEDS_URL = /[\\#\t\r\n\uD800-\uDFFF]|\/(?:\.|%2e){1,2}\//i;
let lastIssueHost: string | undefined;
type JsonRecord = Record<string, unknown>;

function send(res: ForgeResponse, status: number, body: unknown, requestId: string, headers: Record<string, string> = {}): void {
  const bytes = Buffer.isBuffer(body) ? body : Buffer.from(`${JSON.stringify(body)}\n`);
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': bytes.length, 'x-github-request-id': requestId, ...headers });
  res.end(bytes);
}

function requestBody(req: ForgeRequest): Buffer {
  if (req.bodyError) throw req.bodyError;
  return req.body;
}

function authorized(req: ForgeRequest): boolean { return tokenIsAuthorized(String(req.headers.authorization ?? '')); }

function decodeSegment(segment: string): string { return segment.includes('%') ? decodeURIComponent(segment) : segment; }

function jsonError(res: ForgeResponse, status: number, message: string, requestId: string): void {
  send(res, status, { message }, requestId);
}

export function createApp(): Server {
  return createForgeServer(async (req, res) => {
    const requestId = store.requestHeader();
    const rawUrl = req.url ?? '/';
    const queryAt = rawUrl.indexOf('?');
    const rawPath = queryAt < 0 ? rawUrl : rawUrl.slice(0, queryAt);
    const rawQuery = queryAt < 0 ? '' : rawUrl.slice(queryAt + 1);
    const hotIssue = req.method === 'GET' && rawPath.startsWith('/api/v3/repos/') ? /^\/api\/v3\/repos\/([^/?]+)\/([^/?]+)\/issues\/(\d+)$/.exec(rawPath) : null;
    if (hotIssue && authorized(req)) {
      const repo = store.repository(decodeURIComponent(hotIssue[1]!), decodeURIComponent(hotIssue[2]!));
      const cached = repo?.owner === viewerLogin ? repo.issues.get(Number(hotIssue[3]))?.serialized : undefined;
      if (cached) { send(res, 200, cached, requestId); return; }
    }
    const createIssue = req.method === 'POST' && rawPath.startsWith('/api/v3/repos/') && !ISSUE_PATH_NEEDS_URL.test(rawPath)
      ? /^\/api\/v3\/repos\/([^/?]+)\/([^/?]+)\/issues$/.exec(rawPath) : null;
    if (createIssue) {
      // Preserve URL validation before authentication, even on the bypass.
      const host = req.headers.host ?? 'entry';
      if (host !== lastIssueHost) {
        try { new URL(`http://${host}`); }
        catch { jsonError(res, 400, 'Invalid URL', requestId); return; }
        lastIssueHost = host;
      }
      if (!authorized(req)) { jsonError(res, 401, 'Bad credentials', requestId); return; }
      let bytes: Buffer;
      try { bytes = requestBody(req); }
      catch { jsonError(res, 413, 'Request body too large', requestId); return; }
      createIssueFast(res, decodeSegment(createIssue[1]!), decodeSegment(createIssue[2]!), bytes, requestId, viewerLogin);
      return;
    }
    if (isGitRoute(rawPath)) {
      if (!authorized(req)) { jsonError(res, 401, 'Bad credentials', requestId); return; }
      let bytes = Buffer.alloc(0);
      if (req.method === 'POST') {
        try { bytes = requestBody(req); }
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
      try { bytes = requestBody(req); }
      catch { jsonError(res, 400, 'Request body too large', requestId); return; }
      await handleMcp(req, res, bytes, authorized(req));
      return;
    }

    if (url.pathname === '/api/graphql' && req.method === 'POST') {
      if (!authorized(req)) { jsonError(res, 401, 'Bad credentials', requestId); return; }
      let bytes: Buffer;
      try { bytes = requestBody(req); }
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
        try { bytes = requestBody(req); }
        catch { jsonError(res, 413, 'Request body too large', requestId); return; }
      }
      if (handleRest(req, res, url, bytes, requestId, viewerLogin)) return;
    }
    jsonError(res, 404, 'Not Found', requestId);
  });
}

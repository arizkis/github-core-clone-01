import type { ForgeRequest, ForgeResponse } from './server.ts';
import { store, type PullRequest, type Repository } from './store.ts';
import { viewerLogin } from './scenario.ts';

const REVISION = '2026-07-28';
const LEGACY = '2025-11-25';
const tools = Object.freeze([
  { name: 'create_repo', description: 'Create a repository for the authenticated user.', inputSchema: { type: 'object', properties: { name: { type: 'string' }, owner: { type: 'string' }, description: { type: 'string' }, private: { type: 'boolean' } }, required: ['name'], additionalProperties: false } },
  { name: 'get_repo', description: 'Read a repository by owner and name.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' } }, required: ['owner', 'repo'], additionalProperties: false } },
  { name: 'create_issue', description: 'Create an issue in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' } }, required: ['owner', 'repo', 'title'], additionalProperties: false } },
  { name: 'list_issues', description: 'List issues in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, state: { type: 'string', enum: ['open', 'closed', 'all'] } }, required: ['owner', 'repo'], additionalProperties: false } },
  { name: 'create_pull_request', description: 'Open a pull request from one branch into another.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' }, head: { type: 'string' }, base: { type: 'string' } }, required: ['owner', 'repo', 'title', 'head', 'base'], additionalProperties: false } },
  { name: 'merge_pull_request', description: 'Merge a pull request.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, pull_number: { type: 'integer' } }, required: ['owner', 'repo', 'pull_number'], additionalProperties: false } },
]);
const toolsBytes = Buffer.from(JSON.stringify({ tools }));

type RpcRequest = { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown; _meta?: unknown };
type JsonObject = Record<string, unknown>;

function isObject(value: unknown): value is JsonObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function reply(res: ForgeResponse, status: number, bytes: Buffer): void {
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': bytes.length });
  res.end(bytes);
}

function rpcError(id: unknown, code: number, message: string): Buffer {
  return Buffer.from(JSON.stringify({ jsonrpc: '2.0', id: id ?? null, error: { code, message } }));
}

function result(id: unknown, value: unknown): Buffer {
  return Buffer.from(JSON.stringify({ jsonrpc: '2.0', id: id ?? null, result: value }));
}

function content(value: unknown, isError = false): JsonObject {
  const text = JSON.stringify(value);
  return { content: [{ type: 'text', text }], structuredContent: value, ...(isError ? { isError: true } : {}) };
}

function ownerFrom(req: ForgeRequest): string {
  void req;
  return viewerLogin;
}

function repoFrom(args: JsonObject): Repository | undefined {
  const full = typeof args.full_name === 'string' ? args.full_name.split('/') : [];
  const owner = typeof args.owner === 'string' ? args.owner : full[0];
  const name = typeof args.repo === 'string' ? args.repo : typeof args.name === 'string' ? args.name : full[1];
  return owner && name ? store.repository(owner, name) : undefined;
}

function requiredArgs(toolName: string, args: JsonObject): string[] {
  const found = tools.find((tool) => tool.name === toolName);
  if (!found) return [];
  return (found.inputSchema.required as string[]).filter((name) => args[name] === undefined || args[name] === null || args[name] === '');
}

function callTool(name: string, args: JsonObject, req: ForgeRequest): { value: unknown; error?: boolean } {
  const missing = requiredArgs(name, args);
  if (missing.length) return { value: { message: `Missing required arguments: ${missing.join(', ')}` }, error: true };
  if (name === 'create_repo') {
    const owner = typeof args.owner === 'string' ? args.owner : ownerFrom(req);
    const repo = store.createRepository(owner, String(args.name), String(args.description ?? ''), args.private === true);
    return { value: publicRepo(repo) };
  }
  const repo = repoFrom(args);
  if (!repo) return { value: { message: 'Not Found' }, error: true };
  if (name === 'get_repo') return { value: publicRepo(repo) };
  if (name === 'create_issue') {
    const issue = store.createIssue(repo, String(args.title), String(args.body ?? ''));
    return { value: publicIssue(issue) };
  }
  if (name === 'list_issues') {
    const state = typeof args.state === 'string' ? args.state.toLowerCase() : 'open';
    return { value: [...repo.issues.values()].filter((issue) => state === 'all' || issue.state === state).map(publicIssue) };
  }
  if (name === 'create_pull_request') {
    const pull = store.createPull(repo, String(args.title), String(args.body ?? ''), String(args.head), String(args.base));
    return { value: publicPull(pull) };
  }
  if (name === 'merge_pull_request') {
    const pull = repo.pulls.get(Number(args.pull_number));
    if (!pull) return { value: { message: 'Pull request not found' }, error: true };
    pull.state = 'closed';
    pull.merged = true;
    pull.merged_at = pull.updated_at;
    store.invalidate(repo, (key) => key === `pull:${pull.number}` || key.startsWith('pulls:'));
    return { value: { merged: true, sha: pull.head.sha || pull.base.sha || '0'.repeat(40), message: 'Pull request successfully merged' } };
  }
  return { value: { message: `Unknown tool: ${name}` }, error: true };
}

export function publicRepo(repo: Repository): JsonObject {
  return { id: repo.id, node_id: repo.node_id, name: repo.name, full_name: repo.full_name, owner: { login: repo.owner }, private: repo.private, description: repo.description, default_branch: repo.default_branch, created_at: repo.created_at, updated_at: repo.updated_at };
}

export function publicIssue(issue: { id: number; node_id: string; number: number; title: string; body: string; state: string; labels: unknown[]; created_at: string; updated_at: string; comments: number }): JsonObject {
  return { id: issue.id, node_id: issue.node_id, number: issue.number, title: issue.title, body: issue.body, state: issue.state, labels: issue.labels, created_at: issue.created_at, updated_at: issue.updated_at, comments: issue.comments };
}

export function publicPull(pull: PullRequest): JsonObject {
  return { id: pull.id, node_id: pull.node_id, number: pull.number, title: pull.title, body: pull.body, state: pull.state, merged: pull.merged, head: { ref: pull.head.ref, sha: pull.head.sha }, base: { ref: pull.base.ref, sha: pull.base.sha }, created_at: pull.created_at, updated_at: pull.updated_at, merged_at: pull.merged_at };
}

export async function handleMcp(req: ForgeRequest, res: ForgeResponse, body: Buffer, authorized: boolean): Promise<void> {
  if (!authorized) {
    reply(res, 401, Buffer.from(JSON.stringify({ message: 'Bad credentials' })));
    return;
  }
  let input: unknown;
  try { input = JSON.parse(body.toString('utf8')); }
  catch { reply(res, 400, rpcError(null, -32700, 'Parse error')); return; }
  if (!isObject(input) || input.jsonrpc !== '2.0' || typeof input.method !== 'string') {
    reply(res, 400, rpcError(isObject(input) ? input.id : null, -32600, 'Invalid request'));
    return;
  }
  const rpc = input as RpcRequest;
  const rev = isObject(rpc._meta) && typeof rpc._meta.protocolVersion === 'string' ? rpc._meta.protocolVersion : '';
  const id = rpc.id;
  const method = rpc.method;
  if (method === 'notifications/initialized') { res.writeHead(202); res.end(); return; }
  if (method === 'server/discover') {
    reply(res, 200, result(id, { _meta: { 'io.modelcontextprotocol/serverInfo': { name: 'github-core-clone', version: '1.0.0' } }, capabilities: { tools: {} }, supportedVersions: [REVISION, LEGACY] }));
    return;
  }
  if (method === 'initialize') {
    reply(res, 200, result(id, { protocolVersion: LEGACY, serverInfo: { name: 'github-core-clone', version: '1.0.0' }, capabilities: { tools: {} } }));
    return;
  }
  if (method === 'ping') { reply(res, 200, result(id, {})); return; }
  if (method === 'tools/list') {
    reply(res, 200, Buffer.from(JSON.stringify({ jsonrpc: '2.0', id: id ?? null, result: { tools } })));
    return;
  }
  if (method === 'tools/call') {
    if (!isObject(rpc.params) || typeof rpc.params.name !== 'string') { reply(res, 200, rpcError(id, -32602, 'Tool name is required')); return; }
    const name = rpc.params.name;
    if (!tools.some((tool) => tool.name === name)) { reply(res, 200, rpcError(id, -32601, `Unknown tool: ${name}`)); return; }
    const args = isObject(rpc.params.arguments) ? rpc.params.arguments : {};
    const called = callTool(name, args, req);
    reply(res, 200, result(id, content(called.value, called.error)));
    return;
  }
  reply(res, 200, rpcError(id, -32601, `Method not found: ${method}${rev ? ` (${rev})` : ''}`));
}

export const MCP_TOOLS = tools;
export const MCP_TOOLS_BYTES = toolsBytes;

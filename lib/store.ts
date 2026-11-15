import { createHash } from 'node:crypto';

export const FIXED_TIME = '2025-01-01T00:00:00.000Z';

export interface Repository {
  id: number;
  node_id: string;
  owner: string;
  name: string;
  full_name: string;
  description: string;
  private: boolean;
  default_branch: string;
  created_at: string;
  updated_at: string;
  issues: Map<number, Issue>;
  pulls: Map<number, PullRequest>;
  labels: Map<string, Label>;
  comments: Map<number, Comment>;
  refs: Map<string, string>;
  refsVersion: number;
  cache: Map<string, Buffer>;
  nextIssueNumber: number;
  nextPullNumber: number;
}

export interface Issue {
  id: number;
  node_id: string;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  created_at: string;
  updated_at: string;
  comments: number;
  repository: Repository;
  serialized: Buffer;
  serializedResponse: Buffer;
}

const ISSUE_TIMES = `,"created_at":"${FIXED_TIME}","updated_at":"${FIXED_TIME}"}`;
export function refreshIssue(issue: Issue): void {
  const labels = issue.labels.length ? JSON.stringify(issue.labels.map(l => ({ id:l.id,name:l.name,color:l.color }))) : '[]';
  const text = `{"id":${issue.id},"node_id":"${issue.node_id}","number":${issue.number},"title":${JSON.stringify(issue.title)},"body":${JSON.stringify(issue.body)},"state":"${issue.state}","labels":${labels},"comments":${issue.comments}` +
    (issue.created_at === FIXED_TIME && issue.updated_at === FIXED_TIME ? ISSUE_TIMES : `,"created_at":${JSON.stringify(issue.created_at)},"updated_at":${JSON.stringify(issue.updated_at)}}`);
  issue.serializedResponse = Buffer.from(text + '\n');
  issue.serialized = issue.serializedResponse.subarray(0, issue.serializedResponse.length - 1);
}

export interface PullRequest {
  id: number;
  node_id: string;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  created_at: string;
  updated_at: string;
  merged_at: string | null;
  mergeable?: boolean;
  merge_commit_sha?: string | null;
  repository: Repository;
}

export interface Label { id: number; name: string; color: string; }
export interface Comment { id: number; body: string; created_at: string; issue_number?: number; pull_number?: number; }

const globalId = (type: string, id: number): string => Buffer.from(`${type}:${id}`).toString('base64');

export class ForgeStore {
  readonly repositories = new Map<string, Repository>();
  readonly gitObjects = new Map<string, Buffer>();
  readonly gitObjectTypes = new Map<string, 'commit' | 'tree' | 'blob' | 'tag'>();
  // The validated on-wire entry (pack header plus deflated object body).  Objects
  // received through receive-pack retain this so upload-pack can reuse it.
  readonly gitPackedEntries = new Map<string, Buffer>();
  requestId = 0;
  nextId = 1;

  requestHeader(): string {
    this.requestId += 1;
    return `MF${String(this.requestId).padStart(10, '0')}`;
  }

  repository(owner: string, name: string): Repository | undefined {
    return this.repositories.get(`${owner.toLowerCase()}/${name.toLowerCase()}`);
  }

  createRepository(owner: string, name: string, description = '', isPrivate = false): Repository {
    const key = `${owner.toLowerCase()}/${name.toLowerCase()}`;
    const current = this.repositories.get(key);
    if (current) return current;
    const id = this.nextId++;
    const repo: Repository = {
      id, node_id: globalId('Repository', id), owner, name, full_name: `${owner}/${name}`,
      description, private: isPrivate, default_branch: 'main', created_at: FIXED_TIME,
      updated_at: FIXED_TIME, issues: new Map(), pulls: new Map(), labels: new Map(),
      comments: new Map(), refs: new Map(), refsVersion: 0, cache: new Map(), nextIssueNumber: 1, nextPullNumber: 1,
    };
    this.repositories.set(key, repo);
    return repo;
  }

  createIssue(repo: Repository, title: string, body = '', labels: Label[] = []): Issue {
    const id = this.nextId++;
    const issue: Issue = {
      id, node_id: globalId('Issue', id), number: repo.nextIssueNumber++,
      title, body, state: 'open', labels, created_at: FIXED_TIME, updated_at: FIXED_TIME,
      comments: 0, repository: repo,
      serialized: undefined!, serializedResponse: undefined!,
    };
    repo.issues.set(issue.number, issue);
    this.invalidate(repo, (key) => key === `issue:${issue.number}` || key.startsWith('issues:'));
    refreshIssue(issue);
    return issue;
  }

  createPull(repo: Repository, title: string, body: string, head: string, base: string): PullRequest {
    const id = this.nextId++;
    const pull: PullRequest = {
      id, node_id: globalId('PullRequest', id), number: repo.nextPullNumber++, title, body,
      state: 'open', merged: false,
      head: { ref: head, sha: repo.refs.get(`refs/heads/${head}`) ?? '' },
      base: { ref: base, sha: repo.refs.get(`refs/heads/${base}`) ?? '' },
      created_at: FIXED_TIME, updated_at: FIXED_TIME, merged_at: null, repository: repo,
    };
    repo.pulls.set(pull.number, pull);
    this.invalidate(repo, (key) => key.startsWith('pulls:'));
    return pull;
  }

  invalidate(repo: Repository, matches: (key: string) => boolean): void {
    for (const key of repo.cache.keys()) if (matches(key)) repo.cache.delete(key);
  }

  cached(repo: Repository, key: string, value: unknown): Buffer {
    let bytes = repo.cache.get(key);
    if (!bytes) {
      bytes = Buffer.from(JSON.stringify(value));
      repo.cache.set(key, bytes);
    }
    return bytes;
  }

  putObject(bytes: Buffer): string {
    return this.putGitObject('blob', bytes);
  }

  putGitObject(type: 'commit' | 'tree' | 'blob' | 'tag', bytes: Buffer): string {
    const header = Buffer.from(`${type} ${bytes.length}\0`);
    const hash = createHash('sha1').update(header).update(bytes).digest('hex');
    if (!this.gitObjects.has(hash)) this.gitObjects.set(hash, Buffer.from(bytes));
    this.gitObjectTypes.set(hash, type);
    return hash;
  }
}

export const store = new ForgeStore();

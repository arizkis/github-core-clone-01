import type { ForgeRequest, ForgeResponse } from './server.ts';
import { store, refreshIssue, type Repository, type Issue, type PullRequest, type Label } from './store.ts';
import { objectHash, parseCommit, parseTree, type GitObject } from './gitpack.ts';

type Body = Record<string, unknown>;
const ZERO = '0'.repeat(40);
const JSON_TYPE = 'application/json; charset=utf-8';
function json(res: ForgeResponse, status: number, value: unknown, requestId: string, headers: Record<string, string> = {}): void {
  const bytes = Buffer.isBuffer(value) ? value : Buffer.from(`${JSON.stringify(value)}\n`);
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': bytes.length, 'x-github-request-id': requestId, ...headers }); res.end(bytes);
}
function fail(res: ForgeResponse, status: number, message: string, id: string): void { json(res, status, { message }, id); }
function rec(value: unknown): Body { return value && typeof value === 'object' && !Array.isArray(value) ? value as Body : {}; }
function idFor(type: string, id: number): string { return Buffer.from(`${type}:${id}`).toString('base64'); }
function ref(repo: Repository, branch: string): string { return repo.refs.get(branch.startsWith('refs/') ? branch : `refs/heads/${branch}`) ?? ZERO; }
function lookup(hash: string): GitObject | undefined { const data = store.gitObjects.get(hash), type = store.gitObjectTypes.get(hash); return data && type ? { data, type } : undefined; }
function put(type: 'blob'|'tree'|'commit', data: Buffer): string { const hash = objectHash(type, data); store.gitObjects.set(hash, Buffer.from(data)); store.gitObjectTypes.set(hash, type); return hash; }
function commitTree(hash: string): string | undefined { const o = lookup(hash); return o?.type === 'commit' ? parseCommit(o.data).tree : undefined; }
function flatten(treeHash: string, prefix = '', out = new Map<string, string>()): Map<string, string> {
  const tree = lookup(treeHash); if (tree?.type !== 'tree') return out;
  for (const entry of parseTree(tree.data)) {
    const path = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.mode === '40000' || entry.mode === '040000') flatten(entry.hash, path, out);
    else if (entry.mode !== '160000') out.set(path, entry.hash);
  }
  return out;
}
function writeTree(files: Map<string, string>): string {
  type Node = { files: Map<string,string>; dirs: Map<string,Node> };
  const root: Node = { files: new Map(), dirs: new Map() };
  for (const [path, hash] of files) { const bits = path.split('/').filter(Boolean); let node = root; for (const part of bits.slice(0,-1)) { let next = node.dirs.get(part); if (!next) { next={files:new Map(),dirs:new Map()}; node.dirs.set(part,next); } node=next; } if (bits.length) node.files.set(bits.at(-1)!,hash); }
  const build = (node: Node): string => {
    const rows: Array<{mode:string;name:string;hash:string}> = [];
    for (const [name,hash] of node.files) rows.push({mode:'100644',name,hash});
    for (const [name,child] of node.dirs) rows.push({mode:'40000',name,hash:build(child)});
    rows.sort((a,b) => { const ak=a.name+(a.mode==='40000'?'/':''); const bk=b.name+(b.mode==='40000'?'/':''); return ak<bk?-1:ak>bk?1:0; });
    const chunks: Buffer[]=[]; for (const row of rows) chunks.push(Buffer.from(`${row.mode} ${row.name}\0`),Buffer.from(row.hash,'hex'));
    return put('tree',Buffer.concat(chunks));
  };
  return build(root);
}
function commit(tree: string, parent: string | null, message: string): string {
  const stamp = '1735689600 +0000';
  const text = `tree ${tree}\n${parent ? `parent ${parent}\n` : ''}author Makefun Forge <forge@example.invalid> ${stamp}\ncommitter Makefun Forge <forge@example.invalid> ${stamp}\n\n${message}\n`;
  return put('commit',Buffer.from(text));
}
function updatePath(repo: Repository, branch: string, path: string, content: Buffer, message: string, remove = false): {commit:string;blob?:string;old?:string} {
  const branchRef = `refs/heads/${branch}`; const parent = repo.refs.get(branchRef) ?? null;
  const files = parent ? flatten(commitTree(parent) ?? '') : new Map<string,string>(); const old=files.get(path);
  let blob: string | undefined;
  if (remove) files.delete(path); else { blob=put('blob',content); files.set(path,blob); }
  const tree=writeTree(files), sha=commit(tree,parent,message); repo.refs.set(branchRef,sha); repo.refsVersion += 1;
  repo.cache.delete('repository'); store.invalidate(repo,key=>key.startsWith('branches:')||key.startsWith('commits:')||key==='repository'||key.startsWith(`content:${branch}:`));
  return {commit:sha,blob,old};
}
function pubRepo(r: Repository): unknown { return { id:r.id,node_id:r.node_id,name:r.name,full_name:r.full_name,owner:{login:r.owner},private:r.private,description:r.description,default_branch:r.default_branch,created_at:r.created_at,updated_at:r.updated_at,has_issues:true }; }
function pubIssue(i: Issue): unknown { return { id:i.id,node_id:i.node_id,number:i.number,title:i.title,body:i.body,state:i.state,labels:i.labels.map(l=>({id:l.id,name:l.name,color:l.color})),comments:i.comments,created_at:i.created_at,updated_at:i.updated_at }; }
function pubPull(p: PullRequest): unknown { return { id:p.id,node_id:p.node_id,number:p.number,title:p.title,body:p.body,state:p.state,merged:p.merged,head:{...p.head},base:{...p.base},created_at:p.created_at,updated_at:p.updated_at,merged_at:p.merged_at,mergeable:pullMergeable(p),merge_commit_sha:p.merge_commit_sha??null }; }
function cache(repo: Repository,key: string,value: unknown): Buffer { return store.cached(repo,key,value); }
function link(url: URL,page: number,per: number,total: number): Record<string,string> { if(page*per>=total)return{};const query=new URLSearchParams(url.searchParams);query.set('per_page',String(per));query.set('page',String(page+1));return{link:`<${url.origin}${url.pathname}?${query.toString()}>; rel="next"`}; }

export function createIssueFast(res: ForgeResponse, ownerSegment: string, nameSegment: string, bytes: Buffer, requestId: string, viewer: string): void {
  try {
    const repo = store.repository(ownerSegment, nameSegment);
    if (!repo || repo.owner !== viewer) { fail(res, 404, 'Not Found', requestId); return; }
    const b = rec(bytes.length ? JSON.parse(bytes.toString('utf8')) : {});
    const names = Array.isArray(b.labels) ? b.labels.map(String) : [];
    const labels: Label[] = [];
    for (const name of names) {
      const label = repo.labels.get(name.toLowerCase());
      if (!label) { fail(res, 422, `Label ${name} does not exist`, requestId); return; }
      labels.push(label);
    }
    const issue = store.createIssue(repo, String(b.title ?? ''), String(b.body ?? ''), labels);
    json(res, 201, issue.serializedResponse, requestId);
  } catch (cause) {
    fail(res, 400, cause instanceof Error ? cause.message : 'Invalid request', requestId);
  }
}

export function handleRest(req: ForgeRequest,res: ForgeResponse,url: URL,bytes: Buffer,requestId: string,viewer: string): boolean {
  if (!url.pathname.startsWith('/api/v3/')) return false;
  const method=req.method??'GET', path=url.pathname.slice('/api/v3/'.length).split('/').map(decodeURIComponent), body=():Body=>rec(bytes.length?JSON.parse(bytes.toString('utf8')):{});
  try {
    if (method==='GET' && path[0]==='user' && path.length===1) { json(res,200,{login:viewer,id:1000,node_id:Buffer.from('User:1000').toString('base64')},requestId); return true; }
    if (method==='GET' && path[0]==='meta') { json(res,200,{installed_version:'3.0.0'},requestId); return true; }
    if (path[0]==='user'&&path[1]==='repos') {
      if(method==='POST') { const b=body(); if(typeof b.name!=='string'||!b.name) return (fail(res,422,'Repository name is required',requestId),true); const r=store.createRepository(viewer,b.name,String(b.description??''),b.private===true); json(res,201,pubRepo(r),requestId); return true; }
      if(method==='GET') { const rows=[...store.repositories.values()].filter(r=>r.owner===viewer); const page=Math.max(1,Number(url.searchParams.get('page'))||1), per=Math.max(1,Math.min(100,Number(url.searchParams.get('per_page'))||30)); json(res,200,rows.slice((page-1)*per,page*per).map(pubRepo),requestId,link(url,page,per,rows.length)); return true; }
    }
    if(path[0]!=='repos'||path.length<3) return (fail(res,404,'Not Found',requestId),true);
    const repo=store.repository(path[1]!,path[2]!); if(!repo||repo.owner!==viewer) return (fail(res,404,'Not Found',requestId),true);
    const tail=path.slice(3);
    if(!tail.length) {
      if(method==='GET') { json(res,200,cache(repo,'repository',pubRepo(repo)),requestId); return true; }
      if(method==='PATCH') { const b=body(); if(typeof b.description==='string')repo.description=b.description; repo.cache.delete('repository'); json(res,200,pubRepo(repo),requestId); return true; }
      if(method==='DELETE') { store.repositories.delete(`${repo.owner.toLowerCase()}/${repo.name.toLowerCase()}`); res.writeHead(204,{'content-type':JSON_TYPE,'x-github-request-id':requestId});res.end();return true; }
    }
    if(tail[0]==='git'&&tail[1]==='refs'&&method==='POST') { const b=body(),name=String(b.ref??''),sha=String(b.sha??''); if(!/^refs\/(heads|tags)\//.test(name)||!lookup(sha)) return (fail(res,422,'Reference target is invalid',requestId),true); if(repo.refs.has(name))return(fail(res,422,'Reference already exists',requestId),true);repo.refs.set(name,sha);repo.refsVersion += 1;store.invalidate(repo,k=>k.startsWith('branches:'));json(res,201,{ref:name,node_id:idFor('Ref',store.nextId++),object:{sha,type:'commit',url:''}},requestId);return true; }
    if(tail[0]==='branches'&&method==='GET') {const rows=[...repo.refs].filter(([n])=>n.startsWith('refs/heads/')).map(([n,sha])=>({name:n.slice(11),commit:{sha}}));json(res,200,cache(repo,`branches:${url.search}`,rows),requestId);return true;}
    if(tail[0]==='commits'&&method==='GET') {const tip=ref(repo,url.searchParams.get('sha')??repo.default_branch), rows=[];let current=tip;while(current!==ZERO&&rows.length<100){const o=lookup(current);if(o?.type!=='commit')break;const c=parseCommit(o.data);rows.push({sha:current,node_id:idFor('Commit',parseInt(current.slice(0,8),16)),commit:{message:c.message.replace(/\n$/, ''),tree:{sha:c.tree},author:{date:'2025-01-01T00:00:00.000Z'},committer:{date:'2025-01-01T00:00:00.000Z'}}});current=c.parents[0]??ZERO;}json(res,200,cache(repo,`commits:${tip}`,rows),requestId);return true;}
    if(tail[0]==='contents'&&tail.length>1) {const filePath=tail.slice(1).join('/'), branch=url.searchParams.get('branch')??url.searchParams.get('ref')??((method==='PUT'||method==='DELETE')?String(body().branch??repo.default_branch):repo.default_branch),tip=ref(repo,branch),files=tip===ZERO?new Map<string,string>():flatten(commitTree(tip)??'');
      if(method==='PUT'){const b=body(),current=files.get(filePath);if(current&&b.sha!==current)return(fail(res,409,'SHA does not match',requestId),true);if(!current&&b.sha)return(fail(res,409,'File does not exist at supplied SHA',requestId),true);let data:Buffer;try{data=Buffer.from(String(b.content??''),'base64');}catch{return(fail(res,400,'Invalid base64 content',requestId),true);}const message=String(b.message??'update file'),result=updatePath(repo,branch,filePath,data,message);const status=current?200:201;json(res,status,{content:{name:filePath.split('/').at(-1),path:filePath,sha:result.blob,encoding:'base64',content:data.toString('base64'),size:data.length},commit:{sha:result.commit,message,tree:{sha:commitTree(result.commit)}}},requestId);return true;}
      if(method==='DELETE'){const b=body(),current=files.get(filePath);if(!current)return(fail(res,404,'File not found',requestId),true);if(b.sha!==current)return(fail(res,409,'SHA does not match',requestId),true);const message=String(b.message??'delete file'),result=updatePath(repo,branch,filePath,Buffer.alloc(0),message,true);json(res,200,{commit:{sha:result.commit,message}},requestId);return true;}
      if(method==='GET'){const sha=files.get(filePath);if(sha){const data=store.gitObjects.get(sha)!;json(res,200,cache(repo,`content:${branch}:${filePath}`,{type:'file',encoding:'base64',content:data.toString('base64'),sha,size:data.length,path:filePath}),requestId);return true;}const prefix=filePath+'/';const entries=[...files.keys()].filter(p=>p.startsWith(prefix)).map(p=>{const rest=p.slice(prefix.length),name=rest.split('/')[0]!;return {name,path:`${prefix}${name}`,type:rest.includes('/')?'dir':'file',sha:files.get(p)};});if(entries.length){const unique=[...new Map(entries.map(e=>[e.name,e])).values()];json(res,200,unique,requestId);return true;}return(fail(res,404,'File not found',requestId),true);}
    }
    if(tail[0]==='labels') {if(method==='POST'){const b=body(),name=String(b.name??'');if(!name)return(fail(res,422,'Label name is required',requestId),true);const label:Label={id:store.nextId++,name,color:String(b.color??'ededed')};repo.labels.set(name.toLowerCase(),label);json(res,201,label,requestId);return true;}if(method==='GET'){json(res,200,[...repo.labels.values()],requestId);return true;}}
    if(tail[0]==='issues'&&tail.length===1&&method==='POST'){const b=body(),names=Array.isArray(b.labels)?b.labels.map(String):[];const labels:Label[]=[];for(const name of names){const label=repo.labels.get(name.toLowerCase());if(!label)return(fail(res,422,`Label ${name} does not exist`,requestId),true);labels.push(label);}const issue=store.createIssue(repo,String(b.title??''),String(b.body??''),labels);json(res,201,issue.serializedResponse,requestId);return true;}
    if(tail[0]==='issues'&&tail.length===1&&method==='GET'){const state=url.searchParams.get('state')??'open',sort=url.searchParams.get('sort')??'created',dir=url.searchParams.get('direction')??'desc';const all=[...repo.issues.values()].filter(i=>state==='all'||i.state===state).sort((a,b)=>((a.number-b.number)*(dir==='desc'?-1:1)));const page=Math.max(1,Number(url.searchParams.get('page'))||1),per=Math.max(1,Math.min(100,Number(url.searchParams.get('per_page'))||30));const result=all.slice((page-1)*per,page*per).map(pubIssue);json(res,200,cache(repo,`issues:${state}:${sort}:${dir}:${page}:${per}`,result),requestId,link(url,page,per,all.length));return true;}
    if(tail[0]==='issues'&&tail.length>=2){const n=Number(tail[1]),issue=repo.issues.get(n);if(!issue)return(fail(res,404,'Issue not found',requestId),true);if(tail.length===2&&method==='GET'){json(res,200,issue.serialized,requestId);return true;}if(tail.length===2&&method==='PATCH'){const b=body();if(typeof b.title==='string')issue.title=b.title;if(typeof b.body==='string')issue.body=b.body;if(b.state==='open'||b.state==='closed')issue.state=b.state;if(Array.isArray(b.labels)){const ls:Label[]=[];for(const name of b.labels){const l=repo.labels.get(String(name).toLowerCase());if(!l){store.invalidate(repo,k=>k.startsWith('issues:'));refreshIssue(issue);return(fail(res,422,'Unknown label',requestId),true)};ls.push(l);}issue.labels=ls;}store.invalidate(repo,k=>k.startsWith('issues:'));refreshIssue(issue);json(res,200,issue.serializedResponse,requestId);return true;}if(tail[2]==='comments'&&tail.length===3&&method==='POST'){const b=body(),comment={id:store.nextId++,body:String(b.body??''),created_at:'2025-01-01T00:00:00.000Z',issue_number:issue.number};repo.comments.set(comment.id,comment);issue.comments++;store.invalidate(repo,k=>k.startsWith('issues:'));refreshIssue(issue);json(res,201,comment,requestId);return true;}if(tail[2]==='comments'&&tail.length===3&&method==='GET'){json(res,200,[...repo.comments.values()].filter(c=>c.issue_number===issue.number),requestId);return true;}}
    if(tail[0]==='pulls'&&tail.length===1&&method==='POST'){const b=body(),head=String(b.head??''),base=String(b.base??repo.default_branch);if(!repo.refs.has(`refs/heads/${head}`)||!repo.refs.has(`refs/heads/${base}`))return(fail(res,422,'Head or base branch does not exist',requestId),true);const pull=store.createPull(repo,String(b.title??''),String(b.body??''),head,base);pull.mergeable=pullMergeable(pull);json(res,201,pubPull(pull),requestId);return true;}
    if(tail[0]==='pulls'&&tail.length===1&&method==='GET'){const state=url.searchParams.get('state')??'open',all=[...repo.pulls.values()].filter(p=>state==='all'||p.state===state),page=Math.max(1,Number(url.searchParams.get('page'))||1),per=Math.max(1,Math.min(100,Number(url.searchParams.get('per_page'))||30));json(res,200,cache(repo,`pulls:${state}:${page}:${per}`,all.slice((page-1)*per,page*per).map(pubPull)),requestId,link(url,page,per,all.length));return true;}
    if(tail[0]==='pulls'&&tail.length>=2){const pull=repo.pulls.get(Number(tail[1]));if(!pull)return(fail(res,404,'Pull request not found',requestId),true);if(tail.length===2&&method==='GET'){json(res,200,cache(repo,`pull:${pull.number}`,pubPull(pull)),requestId);return true;}const paths=pullFiles(pull);if(tail[2]==='files'&&method==='GET'){json(res,200,paths.map(f=>({filename:f.path,status:f.status,additions:f.additions,deletions:f.deletions,changes:f.additions+f.deletions})),requestId);return true;}if(tail[2]==='merge'&&method==='PUT'){if(pull.mergeable===false)return(fail(res,405,'Pull request is not mergeable',requestId),true);const merged=mergePull(pull);if(!merged)return(fail(res,405,'Pull request is not mergeable',requestId),true);pull.state='closed';pull.merged=true;pull.merged_at='2025-01-01T00:00:00.000Z';pull.merge_commit_sha=merged;pull.updated_at=pull.merged_at;store.invalidate(repo,k=>k===`pull:${pull.number}`||k.startsWith('pulls:')||k.startsWith('branches:'));json(res,200,{sha:merged,merged:true,message:'Pull request successfully merged'},requestId);return true;}}
    return(fail(res,404,'Not Found',requestId),true);
  } catch(cause) { return(fail(res,400,cause instanceof Error?cause.message:'Invalid request',requestId),true); }
}

function ancestors(start: string): Set<string> {const set=new Set<string>(),pending=[start];while(pending.length){const h=pending.pop()!;if(set.has(h)||h===ZERO)continue;set.add(h);const o=lookup(h);if(o?.type==='commit')pending.push(...parseCommit(o.data).parents);}return set;}
export function pullFiles(p: PullRequest): Array<{path:string;status:string;additions:number;deletions:number;old?:string;next?:string}> {const base=flatten(commitTree(p.base.sha)??''),head=flatten(commitTree(p.head.sha)??''),out=[] as Array<{path:string;status:string;additions:number;deletions:number;old?:string;next?:string}>;for(const path of new Set([...base.keys(),...head.keys()])){const a=base.get(path),b=head.get(path);if(a===b)continue;const old=a?store.gitObjects.get(a)?.toString('utf8')??'':'',next=b?store.gitObjects.get(b)?.toString('utf8')??'':'';out.push({path,status:!a?'added':!b?'removed':'modified',additions:next?next.split('\n').filter(Boolean).length:0,deletions:old?old.split('\n').filter(Boolean).length:0,old:a,next:b});}return out;}
export function pullMergeable(p: PullRequest): boolean {const baseTip=ref(p.repository,p.base.ref),baseAnc=ancestors(baseTip),headAnc=ancestors(p.head.sha),common=[...headAnc].find(h=>baseAnc.has(h))??baseTip,base=flatten(commitTree(common)??''),ours=flatten(commitTree(baseTip)??''),theirs=flatten(commitTree(p.head.sha)??'');for(const path of new Set([...base.keys(),...ours.keys(),...theirs.keys()])){const b=base.get(path),o=ours.get(path),t=theirs.get(path);if(o!==b&&t!==b&&o!==t)return false;}return true;}
export function mergePull(p: PullRequest): string | undefined {const repo=p.repository,baseTip=ref(repo,p.base.ref);if(!pullMergeable(p)){p.mergeable=false;return undefined;}p.mergeable=true;const base=flatten(commitTree(baseTip)??''),theirs=flatten(commitTree(p.head.sha)??''),result=new Map(base);for(const path of new Set([...base.keys(),...theirs.keys()])){const b=base.get(path),t=theirs.get(path);if(t!==b){if(t)result.set(path,t);else result.delete(path);}}const tree=writeTree(result),sha=commit(tree,baseTip,`Squash merge pull request #${p.number}`);repo.refs.set(`refs/heads/${p.base.ref}`,sha);repo.refsVersion += 1;p.merge_commit_sha=sha;return sha;}

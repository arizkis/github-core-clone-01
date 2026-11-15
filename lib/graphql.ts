import type { ForgeResponse } from './server.ts';
import { store, refreshIssue, type Repository, type Issue, type PullRequest } from './store.ts';
import { publicRepo } from './mcp.ts';
import { mergePull, pullFiles, pullMergeable } from './rest.ts';

type Field = { name:string; alias:string; args:Record<string,unknown>; children:Field[] };
type Token = string;
const JSON_TYPE='application/json; charset=utf-8';
const TYPE_FIELDS:Record<string,string[]>={
  Issue:['id','number','title','state','body'],
  PullRequest:['id','number','headRefName','baseRefName','mergeable','state','merged','files'],
  Repository:['id','databaseId','name','nameWithOwner','owner','defaultBranchRef','viewerPermission','hasIssuesEnabled','issue','issues','pullRequests','pullRequest'],
  User:['login'], IssueConnection:['nodes','pageInfo'], PageInfo:['hasNextPage','endCursor'],
  PullRequestConnection:['nodes'], PullRequestChangedFile:['path'],
  __Type:['name','fields','enumValues'], __Field:['name'], __EnumValue:['name'],
  IssueState:['OPEN','CLOSED'], SearchType:['ISSUE','REPOSITORY','USER','COMMIT','ISSUE','DISCUSSION'],
};
function tokenize(text:string):Token[]{const found=text.match(/\.\.\.|"(?:\\.|[^"\\])*"|[_A-Za-z][_0-9A-Za-z]*|-?\d+|[!$():=@\[\]{},]/g);if(!found)throw new Error('Empty GraphQL document');return found;}
class Parser {
  private at=0; private readonly tokens:Token[]; private readonly variables:Record<string,unknown>;
  constructor(tokens:Token[],variables:Record<string,unknown>){this.tokens=tokens;this.variables=variables;}
  private peek():string{return this.tokens[this.at]??'';} private take():string{return this.tokens[this.at++]??'';}
  document():Field[]{while(this.peek()&&this.peek()!=='{'){const t=this.take();if(t==='('){let n=1;while(n&&this.peek()){const x=this.take();if(x==='(')n++;if(x===')')n--;}}}return this.selection();}
  private selection():Field[]{if(this.take()!=='{')throw new Error('Expected selection set');const out:Field[]=[];while(this.peek()&&this.peek()!=='}'){if(this.peek()==='...'){this.take();const on=this.take();if(on==='on'){this.take();out.push(...this.selection());}continue;}const first=this.take();let name=first,alias=first;if(this.peek()===':'){this.take();name=this.take();}const args:Record<string,unknown>={};if(this.peek()==='('){this.take();while(this.peek()&&this.peek()!==')'){const key=this.take();if(key===',')continue;if(this.take()!==':')throw new Error('Expected argument colon');args[key]=this.value();}this.take();}let children:Field[]=[];if(this.peek()==='{')children=this.selection();out.push({name,alias,args,children});}this.take();return out;}
  private value():unknown{const t=this.take();if(t==='$')return this.variables[this.take()];if(t==='['){const a:unknown[]=[];while(this.peek()&&this.peek()!==']'){if(this.peek()===','){this.take();continue;}a.push(this.value());}this.take();return a;}if(t==='{'){const o:Record<string,unknown>={};while(this.peek()&&this.peek()!=='}'){if(this.peek()===','){this.take();continue;}const k=this.take();if(this.take()!==':')throw new Error('Expected input colon');o[k]=this.value();}this.take();return o;}if(t.startsWith('"'))return JSON.parse(t);if(/^-?\d+$/.test(t))return Number(t);if(t==='true')return true;if(t==='false')return false;if(t==='null')return null;return t;}
}
function typeName(value:unknown):string{return (value as {__type?:string}|null)?.__type??(value===null?'Null':'Object');}
function globalId(value:string):{type:string;id:number}|undefined{try{const decoded=Buffer.from(value,'base64').toString();const at=decoded.lastIndexOf(':');if(at<0)return;return{type:decoded.slice(0,at),id:Number(decoded.slice(at+1))};}catch{return;}}
function repoById(value:unknown):Repository|undefined{const info=globalId(String(value??''));return info?.type==='Repository'? [...store.repositories.values()].find(r=>r.id===info.id):undefined;}
function issueById(value:unknown):Issue|undefined{const info=globalId(String(value??''));if(info?.type!=='Issue')return;for(const repo of store.repositories.values()){for(const issue of repo.issues.values())if(issue.id===info.id)return issue;}return;}
function pullById(value:unknown):PullRequest|undefined{const info=globalId(String(value??''));if(info?.type!=='PullRequest')return;for(const repo of store.repositories.values()){for(const pull of repo.pulls.values())if(pull.id===info.id)return pull;}return;}
function objType(value:unknown):string{if(value&&typeof value==='object'&&'__type'in value)return String((value as {__type:string}).__type);if(value&&typeof value==='object'&&'number'in value)return 'Issue';return 'Object';}
function repoView(repo:Repository):Record<string,unknown>{return{__type:'Repository',repo};}
function issueView(issue:Issue):Record<string,unknown>{return{__type:'Issue',issue};}
function pullView(pull:PullRequest):Record<string,unknown>{return{__type:'PullRequest',pull};}
function resolve(value:unknown,field:Field,viewer:string):unknown{
  const type=objType(value),args=field.args;
  if(type==='RootQuery'){
    if(field.name==='viewer')return{__type:'User',login:viewer};
    if(field.name==='repository'){const r=store.repository(String(args.owner??''),String(args.name??''));return r?repoView(r):null;}
    if(field.name==='node'){const g=globalId(String(args.id??''));if(!g)return null;for(const r of store.repositories.values()){if(g.type==='Repository'&&r.id===g.id)return repoView(r);for(const i of r.issues.values())if(g.type==='Issue'&&i.id===g.id)return issueView(i);for(const p of r.pulls.values())if(g.type==='PullRequest'&&p.id===g.id)return pullView(p);}return null;}
    if(field.name==='__type')return{__type:'__Type',name:String(args.name??'')};
  }
  if(type==='RootMutation')return mutate(field,viewer);
  if(type==='Repository'){
    const r=(value as {repo:Repository}).repo;
    if(field.name==='id'||field.name==='node_id')return r.node_id;
    if(field.name==='databaseId')return r.id;
    if(field.name==='name')return r.name;
    if(field.name==='nameWithOwner')return r.full_name;
    if(field.name==='owner')return{__type:'User',login:r.owner};
    if(field.name==='defaultBranchRef')return{name:r.default_branch};
    if(field.name==='viewerPermission')return 'ADMIN';
    if(field.name==='hasIssuesEnabled')return true;
    if(field.name==='issue'){const i=r.issues.get(Number(args.number));return i?issueView(i):null;}
    if(field.name==='pullRequest'){const p=r.pulls.get(Number(args.number));return p?pullView(p):null;}
    if(field.name==='issues'){
      let rows=[...r.issues.values()];const states=args.states===undefined?undefined:Array.isArray(args.states)?args.states:[args.states];if(states?.length)rows=rows.filter(i=>states.includes(i.state.toUpperCase()));
      const order=args.orderBy as Record<string,unknown>|undefined;if(order?.field==='CREATED_AT')rows.sort((a,b)=>(a.number-b.number)*(order.direction==='DESC'?-1:1));else rows.sort((a,b)=>a.number-b.number);
      const first=Math.max(0,Math.min(100,Number(args.first??rows.length)||0)),after=cursorIndex(args.after);const offset=after+1, slice=rows.slice(offset,offset+first);return{__type:'IssueConnection',nodes:slice.map(issueView),pageInfo:{__type:'PageInfo',hasNextPage:offset+slice.length<rows.length,endCursor:slice.length?encodeCursor(offset+slice.length-1):null}};
    }
    if(field.name==='pullRequests'){const rows=[...r.pulls.values()].slice(0,Math.max(0,Number(args.first??r.pulls.size)));return{__type:'PullRequestConnection',nodes:rows.map(pullView)};}
  }
  if(type==='Issue'){const i=(value as {issue:Issue}).issue;return({id:i.node_id,number:i.number,title:i.title,state:i.state.toUpperCase(),body:i.body}[field.name]);}
  if(type==='PullRequest'){
    const p=(value as {pull:PullRequest}).pull;
    if(field.name==='id')return p.node_id;if(field.name==='number')return p.number;if(field.name==='headRefName')return p.head.ref;if(field.name==='baseRefName')return p.base.ref;if(field.name==='mergeable')return pullMergeable(p)?'MERGEABLE':'CONFLICTING';if(field.name==='state')return p.merged?'MERGED':p.state.toUpperCase();if(field.name==='merged')return p.merged;
    if(field.name==='files'){const files=pullFiles(p).slice(0,Math.max(0,Number(args.first??100)));return{__type:'PullRequestConnection',nodes:files.map(f=>({__type:'PullRequestChangedFile',path:f.path}))};}
  }
  if(type==='User'&&field.name==='login')return (value as {login:string}).login;
  if(type==='IssueConnection'&&field.name==='nodes')return(value as {nodes:unknown[]}).nodes;
  if(type==='IssueConnection'&&field.name==='pageInfo')return(value as {pageInfo:unknown}).pageInfo;
  if(type==='PageInfo')return(value as Record<string,unknown>)[field.name];
  if(type==='PullRequestConnection'&&field.name==='nodes')return(value as {nodes:unknown[]}).nodes;
  if(type==='PullRequestChangedFile'&&field.name==='path')return(value as {path:string}).path;
  if(type==='__Type'){
    const t=(value as {name:string}).name;if(field.name==='name')return t;
    if(field.name==='fields')return(TYPE_FIELDS[t]??[]).map(name=>({__type:'__Field',name}));
    if(field.name==='enumValues')return(TYPE_FIELDS[t]??[]).map(name=>({__type:'__EnumValue',name}));
  }
  if((type==='__Field'||type==='__EnumValue')&&field.name==='name')return(value as {name:string}).name;
  if(value&&typeof value==='object'&&field.name in value)return(value as Record<string,unknown>)[field.name];
  throw new Error(`Cannot query field "${field.name}" on type "${type}".`);
}
function encodeCursor(index:number):string{return Buffer.from(`issue:${index}`).toString('base64');}
function cursorIndex(cursor:unknown):number{if(typeof cursor!=='string')return -1;const text=Buffer.from(cursor,'base64').toString();const n=Number(text.slice(text.lastIndexOf(':')+1));return Number.isInteger(n)?n:-1;}
function mutate(field:Field,viewer:string):unknown{
  const input=(field.args.input??{}) as Record<string,unknown>;
  if(field.name==='createIssue'){const repo=repoById(input.repositoryId);if(!repo)throw new Error('Repository not found');return{issue:issueView(store.createIssue(repo,String(input.title??''),String(input.body??''))) };}
  if(field.name==='closeIssue'){const issue=issueById(input.issueId);if(!issue)throw new Error('Issue not found');issue.state='closed';refreshIssue(issue);store.invalidate(issue.repository,k=>k===`issue:${issue.number}`||k.startsWith('issues:'));return{issue:issueView(issue)};}
  if(field.name==='createPullRequest'){const repo=repoById(input.repositoryId);if(!repo)throw new Error('Repository not found');const head=String(input.headRefName??''),base=String(input.baseRefName??repo.default_branch);const p=store.createPull(repo,String(input.title??''),String(input.body??''),head,base);p.mergeable=pullMergeable(p);return{pullRequest:pullView(p)};}
  if(field.name==='mergePullRequest'){const pull=pullById(input.pullRequestId);if(!pull)throw new Error('Pull request not found');const sha=mergePull(pull);if(!sha)throw new Error('Pull request is not mergeable');pull.state='closed';pull.merged=true;pull.merged_at='2025-01-01T00:00:00.000Z';pull.merge_commit_sha=sha;store.invalidate(pull.repository,k=>k===`pull:${pull.number}`||k.startsWith('pulls:')||k.startsWith('branches:'));return{pullRequest:pullView(pull)};}
  if(field.name==='addComment'){const issue=issueById(input.subjectId);const pull=pullById(input.subjectId);if(!issue&&!pull)throw new Error('Comment subject not found');const repo=issue?.repository??pull!.repository;const comment={id:store.nextId++,body:String(input.body??''),created_at:'2025-01-01T00:00:00.000Z',...(issue?{issue_number:issue.number}:{pull_number:pull!.number})};repo.comments.set(comment.id,comment);if(issue){issue.comments++;refreshIssue(issue);store.invalidate(repo,k=>k===`issue:${issue.number}`||k.startsWith('issues:'));}return{commentEdge:{node:{id:Buffer.from(`Comment:${comment.id}`).toString('base64'),body:comment.body}}};}
  throw new Error(`Cannot query field "${field.name}" on type "Mutation".`);
}
const known:Record<string,string[]>={RootQuery:['viewer','repository','node','__type'],RootMutation:['createIssue','closeIssue','createPullRequest','mergePullRequest','addComment']};
function project(value:unknown,fields:Field[],viewer:string,errors:unknown[]):unknown{
  if(Array.isArray(value))return value.map(v=>project(v,fields,viewer,errors));if(value===null||value===undefined)return null;
  const type=objType(value),out:Record<string,unknown>={};
  for(const field of fields){if((known[type]??TYPE_FIELDS[type]??[]).length&&!((known[type]??TYPE_FIELDS[type]??[]).includes(field.name))){errors.push({message:`Cannot query field "${field.name}" on type "${type==='RootQuery'?'Query':type==='RootMutation'?'Mutation':type}".`});continue;}
    try{const child=resolve(value,field,viewer);out[field.alias]=field.children.length?project(child,field.children,viewer,errors):child;}catch(e){errors.push({message:e instanceof Error?e.message:String(e)});out[field.alias]=null;}
  }return out;
}
function execute(document:string,variables:Record<string,unknown>,viewer:string):Record<string,unknown>{const fields=new Parser(tokenize(document),variables).document();let kind='query';if(/^\s*mutation\b/.test(document))kind='mutation';const root={__type:kind==='mutation'?'RootMutation':'RootQuery'};const errors:unknown[]=[];const data=project(root,fields,viewer,errors);return errors.length?{data,errors}:{data};}
export function handleGraphql(res:ForgeResponse,bytes:Buffer,requestId:string,viewer:string):void{
  let body:unknown;try{body=JSON.parse(bytes.toString('utf8'));}catch{res.writeHead(400,{'content-type':JSON_TYPE,'x-github-request-id':requestId});res.end(JSON.stringify({message:'Invalid JSON body'}));return;}
  try{const one=(entry:unknown)=>{const row=entry as Record<string,unknown>;if(typeof row?.query!=='string')return{errors:[{message:'A query document is required'}]};return execute(row.query,typeof row.variables==='object'&&row.variables!==null?row.variables as Record<string,unknown>:{},viewer);};const output=Array.isArray(body)?body.map(one):one(body);const data=Buffer.from(`${JSON.stringify(output)}\n`);res.writeHead(200,{'content-type':JSON_TYPE,'content-length':data.length,'x-github-request-id':requestId});res.end(data);}catch(cause){const data=Buffer.from(JSON.stringify({errors:[{message:cause instanceof Error?cause.message:'Invalid query'}]}));res.writeHead(200,{'content-type':JSON_TYPE,'content-length':data.length,'x-github-request-id':requestId});res.end(data);}
}

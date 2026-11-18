#!/usr/bin/env python3
"""GraphQL/MCP byte parity against the preserved TypeScript source."""
import base64
import contextlib
import http.client
import http.server
import json
import os
import signal
import subprocess
import sys
import threading
import time

sys.dont_write_bytecode = True
import preview
from reference import source_reference


def main():
    preview.PORTS.clear()
    preview.DONE = False
    coordinator = http.server.ThreadingHTTPServer(('127.0.0.1', 0), preview.Coordinator)
    threading.Thread(target=coordinator.serve_forever, daemon=True).start()
    processes, failures = [], []
    count = 0
    with source_reference() as reference:
        try:
            go_command = getattr(preview, 'FORGE_COMMAND', [preview.FORGE_BINARY])
            for key, command, directory in [('go', go_command, preview.ROOT), ('ts', ['node', '--experimental-strip-types', 'main.ts'], reference)]:
                env = dict(os.environ, MAKEFUN_ENV_URL=f'http://127.0.0.1:{coordinator.server_port}/{key}', MAKEFUN_SHOP_TOKEN='preview-secret', NODE_NO_WARNINGS='1')
                processes.append(subprocess.Popen(command, cwd=directory, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True))
            deadline = time.monotonic() + 30
            while len(preview.PORTS) < 2 and time.monotonic() < deadline:
                assert all(p.poll() is None for p in processes)
                time.sleep(.02)
            assert len(preview.PORTS) == 2

            def request(key, method, path, data=None, auth=True, headers=None):
                h = {'Host': 'entry.example', 'Connection': 'close'}
                if auth:
                    h['Authorization'] = 'Bearer preview-secret'
                if data is not None and not isinstance(data, bytes):
                    data = json.dumps(data, separators=(',', ':'), ensure_ascii=True).encode()
                    h['Content-Type'] = 'application/json'
                h.update(headers or {})
                connection = http.client.HTTPConnection('127.0.0.1', preview.PORTS[key], timeout=15)
                try:
                    connection.request(method, path, data, h)
                    r = connection.getresponse()
                    return r.status, r.getheaders(), r.read()
                except http.client.RemoteDisconnected:
                    return 'closed', [], b''
                finally:
                    connection.close()

            def check(method, path, data=None, auth=True, headers=None):
                nonlocal count
                count += 1
                answers = [request(k, method, path, data, auth, headers) for k in ['go', 'ts']]
                if answers[0] != answers[1]:
                    failures.append((count, method, path, data, answers))
                    print('FAIL', count, method, path, repr(data)[:350], repr(answers)[:2000], flush=True)
                raw = answers[0][2]
                return json.loads(raw) if raw else None

            def gql(document, variables=None, **options):
                payload = {'query': document}
                if variables is not None:
                    payload['variables'] = variables
                return check('POST', '/api/graphql', payload, **options)

            def rpc(method, params=None, id=1, **options):
                payload = {'jsonrpc': '2.0', 'id': id, 'method': method}
                if params is not None:
                    payload['params'] = params
                return check('POST', '/mcp', payload, **options)

            def tool(name, args, **options):
                return rpc('tools/call', {'name': name, 'arguments': args}, **options)

            viewer = check('GET', '/api/v3/user')['login']
            check('POST', '/graphql', {'query': '{viewer{login}}'})
            check('POST', '/api/graphql', {'query': '{viewer{login}}'}, auth=False)
            check('GET', '/api/graphql')
            check('GET', '/mcp', auth=False)
            for content_type in ['application/json', 'text/plain', 'application/JSON', '']:
                for data in [b'', b'{', b'null', b'[]', b'"query"', b'{"query":42}', b'{"query":"{viewer{login}}"}']:
                    check('POST', '/api/graphql', data, headers={'Content-Type': content_type})
            for raw in [b'{', b'null', b'[]', b'{}', b'1', b'true', b'{"id":{"z":1,"a":2},"method":"ping"}', b'{"jsonrpc":"2.0","method":1}', b'{"jsonrpc":"2.0","method":"ping"}']:
                check('POST', '/mcp', raw)
            rpc('ping', auth=False)
            for method in ['server/discover', 'initialize', 'ping', 'tools/list', 'tools/list', 'notifications/initialized', 'unknown', 'exportState', 'exportRepoZip']:
                rpc(method, {'protocolVersion': 'bad'}, id='id:' + method)
            check('POST', '/mcp', {'jsonrpc': '2.0', 'method': 'unknown', '_meta': {'protocolVersion': '2026-07-28'}})
            for id in [None, True, False, [], {'z': 1, '2': 'two', '1': 'one', 'a': {'q': '<>&'}}, ['a', None, 1], '😀\ud800']:
                rpc('ping', id=id)
            check('POST', '/mcp', b'{"jsonrpc":"2.0","id":1e999,"method":"ping"}')
            for params in [None, [], {}, {'name': 1}, {'name': 'unknown'}, {'name': 'CREATE_REPO'}, {'name': 'create_repo'}, {'name': 'create_repo', 'arguments': []}]:
                rpc('tools/call', params)
            for name in ['create_repo', 'get_repo', 'create_issue', 'list_issues', 'create_pull_request', 'merge_pull_request']:
                tool(name, {})
            created = tool('create_repo', {'name': 'api-preview', 'description': '<Go> & MCP', 'private': True})['result']['structuredContent']
            repo_id = created['node_id']
            repo = f'/api/v3/repos/{viewer}/api-preview'
            check('GET', repo)
            tool('get_repo', {'owner': viewer.upper(), 'repo': 'API-PREVIEW'})
            tool('get_repo', {'full_name': viewer + '/api-preview'})
            tool('get_repo', {'owner': 'other', 'repo': 'api-preview'})
            tool('create_issue', {'owner': viewer, 'repo': 'api-preview', 'title': 'MCP issue', 'body': '<>&\u2028\ud800'})
            tool('create_issue', {'owner': viewer, 'repo': 'api-preview', 'title': ['array', None, True], 'body': {'object': 'value'}})
            check('GET', repo + '/issues/1')
            tool('list_issues', {'owner': viewer, 'repo': 'api-preview', 'state': 'ALL'})
            tool('create_issue', {'owner': viewer, 'repo': 'api-preview', 'title': {'toString': None}})
            tool('create_repo', {'name': 'foreign', 'owner': 'someone-else'})
            check('GET', '/api/v3/repos/someone-else/foreign')
            gql('{repository(owner:"someone-else",name:"foreign"){id owner{login} viewerPermission}}')

            gql('{viewer{login}}')
            gql('{one:viewer{login} two:viewer{login} one:viewer{login}}')
            gql('{9:viewer{login} 2:viewer{login} __proto__:viewer{login}}')
            gql('{viewer{__typename unknown login}}')
            gql('{users{login} missing viewer{login}}')
            gql('{viewer{login{length toString}}}')
            for kind in ['Issue', 'PullRequest', 'Repository', 'User', 'SearchType', 'IssueState', 'Unknown', '__proto__']:
                gql('query($name:String!){__type(name:$name){name fields{name} enumValues{name}}}', {'name': kind})
            lookup = 'query X($owner:String!,$name:String!){repository(owner:$owner,name:$name){id databaseId name nameWithOwner owner{login} defaultBranchRef{name} viewerPermission hasIssuesEnabled}}'
            gql(lookup, {'owner': viewer, 'name': 'api-preview'})
            gql(lookup, {'owner': viewer, 'name': 'missing'})
            for variables in [None, [], True, {'owner': viewer}, {'owner': viewer, 'name': ['api-preview']}]:
                gql(lookup, variables)
            gql('query{repository(owner:"' + viewer + '",name:"api-preview")}')
            prefix = 'repository(owner:"' + viewer + '",name:"api-preview")'
            issue_query = '{' + prefix + '{issue(number:$number){id number title body state} issues(first:$first,after:$after,states:$states,orderBy:$order){nodes{id number title state} pageInfo{hasNextPage endCursor}}}}'
            for value in [1, '1', True, None, [1], {}, 1.5]:
                gql(issue_query, {'number': value, 'first': 1, 'states': ['OPEN'], 'order': {'field': 'CREATED_AT', 'direction': 'DESC'}})
            for first in [None, '2', -1, 0, 1.5, True, {}, []]:
                gql(issue_query, {'number': 1, 'first': first})
            for after in ['', 'invalid', base64.b64encode(b'issue:0').decode(), base64.b64encode(b'issue:-2').decode(), base64.b64encode(b'issue:1.5').decode()]:
                gql(issue_query, {'after': after, 'states': 'OPEN', 'first': 1})
            for states in [[], None, ['CLOSED'], 'OPEN']:
                gql(issue_query, {'states': states})

            mutation = 'mutation($input:CreateIssueInput!){createIssue(input:$input){issue{id number title body state}}}'
            issue = gql(mutation, {'input': {'repositoryId': repo_id, 'title': 'GraphQL issue', 'body': 'shared'}})['data']['createIssue']['issue']
            for input in [{}, None, [], True, {'repositoryId': repo_id, 'title': ['a', None, {}]}, {'repositoryId': 'bad'}, {'repositoryId': repo_id, 'title': {'toString': 'x'}}]:
                gql(mutation, {'input': input})
            gql('mutation($id:ID!){closeIssue(input:{issueId:$id}){issue{id state}}}', {'id': issue['id']})
            gql('mutation{closeIssue(input:{issueId:"bad"}){issue{id}}}')
            gql('mutation($id:ID!){addComment(input:{subjectId:$id,body:"comment"}){commentEdge{node{id body}}}}', {'id': issue['id']})
            check('GET', repo + '/issues/' + str(issue['number']) + '/comments')
            tool('list_issues', {'owner': viewer, 'repo': 'api-preview'})
            gql('{node(id:$id){... on Issue{id number title state body}}}', {'id': issue['id']})
            gql('{node(id:$id){... on Repository{id databaseId owner{login}}}}', {'id': repo_id})
            for id in ['bad', '', None, True, [], '::::', base64.b64encode(b'Issue:not-number').decode(), base64.b64encode(b'Unknown:1').decode()]:
                gql('{node(id:$id){id}}', {'id': id})

            content = check('PUT', repo + '/contents/README.md', {'content': base64.b64encode(b'base\n').decode()})
            sha = content['commit']['sha']
            check('POST', repo + '/git/refs', {'ref': 'refs/heads/feature', 'sha': sha})
            check('PUT', repo + '/contents/new.txt', {'branch': 'feature', 'content': base64.b64encode(b'new\n').decode()})
            pr = gql('mutation($i:CreatePullRequestInput!){createPullRequest(input:$i){pullRequest{id number headRefName baseRefName mergeable state merged files(first:10){nodes{path}}}}}', {'i': {'repositoryId': repo_id, 'title': 'GQL PR', 'headRefName': 'feature', 'baseRefName': 'main'}})['data']['createPullRequest']['pullRequest']
            gql('{' + prefix + '{pullRequests(first:2){nodes{id number}} pullRequest(number:1){id state}}}')
            gql('mutation($i:AddCommentInput!){addComment(input:$i){commentEdge{node{id body}}}}', {'i': {'subjectId': pr['id'], 'body': 'PR comment'}})
            gql('mutation($id:ID!){mergePullRequest(input:{pullRequestId:$id}){pullRequest{id state merged}}}', {'id': pr['id']})
            check('GET', repo + '/pulls/1')
            tool('create_pull_request', {'owner': viewer, 'repo': 'api-preview', 'title': 'MCP PR', 'head': 'absent', 'base': 'absent'})
            tool('merge_pull_request', {'owner': viewer, 'repo': 'api-preview', 'pull_number': 2})
            tool('merge_pull_request', {'owner': viewer, 'repo': 'api-preview', 'pull_number': True})
            tool('merge_pull_request', {'owner': viewer, 'repo': 'api-preview', 'pull_number': 999})
            check('GET', repo + '/pulls/2')
            gql('mutation{createPullRequest(input:{repositoryId:"bad"}){pullRequest{id}}}')
            gql('mutation{mergePullRequest(input:{pullRequestId:"bad"}){pullRequest{id}}}')
            gql('mutation{addComment(input:{subjectId:"bad"}){commentEdge{node{id}}}}')

            for document in ['', '   ', 'garbage', '{', '{viewer', '{viewer(login 1){login}}', '{viewer(a:{x 1}){login}}', '{viewer{login,login}}', '{viewer{login @skip(if:true)}}', '{viewer{login ... on User{login}}}', '{repository(owner:"\\x"){name}}']:
                gql(document)
            for terminator in ['\r', '\u2028', '\u2029']:
                gql('{repository(owner:"a' + '\\' + terminator + 'b",name:"x"){name}}')
            check('POST', '/api/graphql', [{'query': '{viewer{login}}'}, {'query': '{viewer{login}}'}])
            check('POST', '/api/graphql', [{'query': '{viewer{login}}'}, {'query': 'garbage'}, {'query': '{viewer{login}}'}])
            check('POST', '/api/graphql', [{'query': 'mutation{createIssue(input:{repositoryId:"' + repo_id + '",title:"batch side effect"}){issue{number}}}'}, {'query': ''}])
            check('GET', repo + '/issues?state=all')
            check('POST', '/api/graphql', b' ' * 2_000_001, auth=False)
            check('POST', '/api/graphql', b' ' * 2_000_001)
            check('POST', '/mcp', b' ' * 2_000_001, auth=False)
            check('POST', '/mcp', b' ' * 2_000_001)
            preview.DONE = True
            for p in processes:
                assert p.wait(timeout=15) == 0, p.stderr.read()
            print(f'GraphQL/MCP parity: {count} checks; {len(failures)} failures', flush=True)
            assert not failures
        finally:
            preview.DONE = True
            for p in processes:
                if p.poll() is None:
                    os.killpg(p.pid, signal.SIGKILL)
                p.communicate(timeout=5)
            coordinator.shutdown()
            coordinator.server_close()


if __name__ == '__main__':
    main()

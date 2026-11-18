#!/usr/bin/env python3
"""Offline byte-parity checks against the checked-in TypeScript reference.

Starts both implementations with a stub coordinator. No competition services
are contacted. Run after building ./forge: python3 tests/preview.py
"""
import base64
import concurrent.futures
import http.client
import http.server
import json
import os
from pathlib import Path
import random
import socket
import subprocess
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
FORGE_BINARY = os.environ.get('FORGE_PREVIEW_BINARY', './forge')
PORTS = {}
DONE = False
REGISTRATIONS = []


class Coordinator(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        value = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        key = self.path.split('/')[1]
        PORTS[key] = value['port']
        REGISTRATIONS.append((key, value, self.headers['Content-Type']))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{}')

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(json.dumps({'done': DONE}).encode())


def main():
    global DONE
    coordinator = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Coordinator)
    threading.Thread(target=coordinator.serve_forever, daemon=True).start()
    processes = []
    checks = 0
    failures = []
    try:
        for key, command in [('go', [FORGE_BINARY]), ('ts', ['node', '--experimental-strip-types', 'main.ts'])]:
            env = dict(os.environ, MAKEFUN_ENV_URL=f'http://127.0.0.1:{coordinator.server_port}/{key}',
                       MAKEFUN_SHOP_TOKEN='preview-secret', NODE_NO_WARNINGS='1')
            processes.append(subprocess.Popen(command, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE))
        deadline = time.monotonic() + 30
        while len(PORTS) < 2 and time.monotonic() < deadline:
            if any(p.poll() is not None for p in processes):
                raise RuntimeError([(p.poll(), p.stderr.read() if p.poll() is not None else b'') for p in processes])
            time.sleep(.02)
        assert len(PORTS) == 2, 'both servers must bind and register'
        assert all(v.keys() == {'port'} and ct == 'application/json' for _, v, ct in REGISTRATIONS)
        print('PASS: both servers bound and registered', flush=True)

        def request(key, method, path, data=None, auth=True, extra=None):
            conn = http.client.HTTPConnection('127.0.0.1', PORTS[key], timeout=10)
            headers = {'Host': 'entry.example', 'Connection': 'close'}
            if auth:
                headers['Authorization'] = 'Bearer preview-secret'
            if data is not None and not isinstance(data, bytes):
                data = json.dumps(data, ensure_ascii=True, separators=(',', ':')).encode()
                headers['Content-Type'] = 'application/json'
            headers.update(extra or {})
            conn.request(method, path, body=data, headers=headers)
            response = conn.getresponse()
            result = (response.status, response.getheaders(), response.read())
            conn.close()
            return result

        def check(method, path, data=None, auth=True, extra=None):
            nonlocal checks
            results = [request(key, method, path, data, auth, extra) for key in ['go', 'ts']]
            checks += 1
            if results[0] != results[1]:
                failures.append((method, path, data, results))
                print(f'FAIL #{checks} {method} {path}: {results!r}', flush=True)
            return results[0][0], json.loads(results[0][2]) if results[0][2] else None

        # Exercise curl explicitly as well as the raw HTTP differential driver.
        def curl_both(path, *args):
            nonlocal checks
            outputs = []
            for key in ['go', 'ts']:
                outputs.append(subprocess.check_output(['curl', '--noproxy', '*', '-sS', '-i',
                    '-H', 'Host: entry.example', '-H', 'Connection: close', *args,
                    f'http://127.0.0.1:{PORTS[key]}{path}']))
            checks += 1
            assert outputs[0] == outputs[1], outputs
            return outputs[0]

        curl_both('/healthz')
        unauthorized = curl_both('/api/v3/user')
        assert b'401 Unauthorized' in unauthorized and b'{"message":"Bad credentials"}\n' in unauthorized
        _, user = check('GET', '/api/v3/user')
        viewer = user['login']
        print(f'PASS: curl health and 401; deterministic viewer={viewer}', flush=True)
        check('GET', '/api/v3')
        check('GET', '/api/v3/')
        check('GET', '/api/v3/rate_limit')
        check('GET', '/api/v3/meta/extra')
        check('GET', '/healthz', auth=False)
        check('HEAD', '/healthz', auth=False)
        check('GET', '/api/v3/user', extra={'Authorization': 'Basic preview-secret'})
        check('GET', '/api/v3/user', extra={'Authorization': 'bearer\tpreview-secret'})
        for data in [{}, {'name': 1}, {'name': ''}, {'name': 'preview', 'description': '<Go> & TS\u2028\u2029', 'private': True}]:
            check('POST', '/api/v3/user/repos', data)
        prefix = f'/api/v3/repos/{viewer}/preview'
        check('POST', '/api/v3/user/repos', {'name': 'PREVIEW', 'description': 'ignored'})
        check('GET', '/api/v3/user/repos?per_page=1&page=1')
        check('GET', prefix)
        check('GET', prefix)
        check('PATCH', prefix, {'description': 'updated & <unescaped>'})
        check('GET', prefix)
        check('GET', prefix.upper().replace('/API/V3/REPOS/', '/api/v3/repos/'))
        check('POST', prefix + '/labels', {'name': 'Bug', 'color': 'c0ffee'})
        check('POST', prefix + '/labels', {'name': 'second'})
        check('POST', prefix + '/labels', {'name': 'BUG', 'color': '000000'})
        check('GET', prefix + '/labels/ignored')
        check('POST', prefix + '/labels', {})
        for label in ['\ud800', '\udfff', 'İ', 'ΟΣ']:
            check('POST', prefix + '/labels', {'name': label})
        check('GET', prefix + '/labels')
        check('POST', prefix + '/issues', {'title': 'unicode labels', 'labels': ['\ud800', '\udfff', 'i\u0307', 'ος']})
        issue = curl_both(prefix + '/issues', '-H', 'Authorization: Bearer preview-secret',
                         '-H', 'Content-Type: application/json', '--data-binary', '{"title":"curl authenticated issue"}')
        assert b'201 Created' in issue and b'curl authenticated issue' in issue
        print('PASS: curl authenticated issue creation', flush=True)
        check('GET', prefix + '/issues/1')
        check('POST', prefix + '/issues', {'title': ['array', None, True], 'body': {'object': 1}, 'labels': ['bug']})
        check('POST', prefix + '/issues', {'title': 1e30, 'labels': ['absent']})
        check('POST', prefix + '/issues', {'title': '\ud800 \udc00 \U0001f600', 'body': '<>&\u2028\u2029\b\f\n\t'})
        for direction in ['asc', 'desc', 'other']:
            check('GET', prefix + f'/issues?state=all&sort=updated&direction={direction}&per_page=1&x=a%20b~&page=1')
        for page in ['0', '-1', '1.5', 'Infinity', 'NaN', '0x2']:
            check('GET', prefix + f'/issues?page={page}&per_page=1.5')
        check('PATCH', prefix + '/issues/1', {'title': 'partial mutation', 'labels': ['missing'], 'state': 'closed'})
        check('GET', prefix + '/issues/1')
        check('PATCH', prefix + '/issues/1', {'labels': ['bug', 'second'], 'body': 'patched', 'state': 'open'})
        check('POST', prefix + '/issues/1/comments', {'body': ['a', 'b']})
        check('GET', prefix + '/issues/1/comments')
        check('GET', prefix + '/issues/1')
        check('GET', prefix + '/issues/0x1')
        check('GET', prefix + '/issues/1.0')
        check('GET', prefix + '/issues/99')
        check('POST', prefix.replace(viewer, 'stranger') + '/issues', b'{')
        check('POST', prefix + '/issues', b'{', auth=False)
        for bad in [b'{', b'{bad}', b'{"a":}', b'[1,]', b'null x', b'bad', b'undefined', b'"abc',
                    b'{"a" 2}', b'{"a":1,}', b'{"a":01}', b'{"a":1e}', b'"\\x"', b'"a\nb"',
                    b'[', b' ', b'truex', b'{"a":1', b'1.', b'-', b'{"a": [1 2]}',
                    b'"\\u123x"', b'"\\u12', b'{"a": "long invalid example", x: 1}']:
            check('POST', prefix + '/issues', bad)
        for data in [None, [], 3, True, 'text', {'title': 1e-7}, {'title': [1, [2, None], {}]}, {'title': '\ud800a\udfff'}]:
            encoded = json.dumps(data, separators=(',', ':')).encode()
            check('POST', prefix + '/issues', encoded)
        # Mutated documents cover parser error locations, nesting and diagnostics.
        rng = random.Random(33944905)
        seeds = [b'{"title":"example","labels":["bug"],"body":true}', b'{"title":123.45e-2}',
                 b'[1,2,{"title":"hello"}]', b'{"title":"\\uD800\\uDC00"}',
                 b'"this is a fairly long JSON string"', b'{"title":null,"body":[false,0]}']
        corpus = [b'{"x":', b'{"x"', b'[1', b'[[', b'{"x": [1,', b'"\\', b'NaN', b'Infinity',
                  b'"\\u', b'"\\u1', b'"\\u123', b'{"title":"\xff\xfe\xed\xa0\x80"}',
                  b'{"title":{"toString":"x"}}', b'{"labels":[{"toString":null}]}',
                  b'{"title":"\xe2\x82"}', b'{"title":"\xf0\x9f\x92"}']
        for _ in range(160):
            seed = rng.choice(seeds)
            at = rng.randrange(len(seed))
            kind = rng.randrange(3)
            if kind == 0:
                sample = seed[:at]
            elif kind == 1:
                sample = seed[:at] + seed[at + 1:]
            else:
                sample = seed[:at] + bytes([rng.choice(b'xyz,:]}-01\n')]) + seed[at + 1:]
            corpus.append(sample)
        for sample in corpus:
            check('POST', prefix + '/issues', sample)
        check('GET', prefix + '/issues?state=all')

        def put(path, content, **fields):
            return check('PUT', prefix + '/contents/' + path, dict(content=base64.b64encode(content).decode(), **fields))

        status, content = put('README.md', b'hello\n', message='initial')
        assert status == 201
        initial = content['commit']['sha']
        blob = content['content']['sha']
        check('GET', prefix + '/contents/README.md')
        check('GET', prefix + '/contents/README.md')
        check('PUT', prefix + '/contents/README.md', {'content': 'AA=='})
        check('PUT', prefix + '/contents/new', {'content': 'AA==', 'sha': 'wrong'})
        check('POST', prefix + '/git/refs', {'ref': 'refs/heads/feature', 'sha': initial})
        check('POST', prefix + '/git/refs', {'ref': 'refs/heads/feature', 'sha': initial})
        check('POST', prefix + '/git/refs', {'ref': 'invalid', 'sha': initial})
        check('POST', prefix + '/git/refs', {'ref': 'refs/tags/v1', 'sha': blob})
        put('dir/z.txt', b'z\n\nlast\n', branch='feature')
        put('dir/a.txt', b'a\n', branch='feature')
        put('dir/sub/b.txt', b'b\n', branch='feature')
        check('GET', prefix + '/contents/dir?ref=feature')
        check('GET', prefix + '/branches')
        check('GET', prefix + '/commits?sha=feature')
        check('POST', prefix + '/pulls', {'head': 'feature', 'base': 'main', 'title': 'merge dirs'})
        check('GET', prefix + '/pulls')
        check('GET', prefix + '/pulls/1')
        check('GET', prefix + '/pulls/1/files')
        check('PUT', prefix + '/pulls/1/merge', {})
        check('GET', prefix + '/pulls/1')
        check('GET', prefix + '/pulls?state=all')
        check('GET', prefix + '/commits')
        check('GET', prefix + '/contents/dir')
        check('DELETE', prefix + '/contents/README.md', {'sha': 'wrong'})
        check('DELETE', prefix + '/contents/README.md', {'sha': blob})
        check('GET', prefix + '/contents/README.md')
        check('DELETE', prefix + '/contents/README.md', {'sha': blob})
        # Divergent edits must conflict, while updates to independent paths merge.
        _, tip = check('GET', prefix + '/branches')
        main_sha = next(x['commit']['sha'] for x in tip if x['name'] == 'main')
        check('POST', prefix + '/git/refs', {'ref': 'refs/heads/conflict', 'sha': main_sha})
        _, row = put('same.txt', b'ours\n')
        put('same.txt', b'theirs\n', branch='conflict')
        check('POST', prefix + '/pulls', {'head': 'conflict', 'title': 'conflict'})
        check('GET', prefix + '/pulls/2/files')
        check('PUT', prefix + '/pulls/2/merge', {})
        check('POST', prefix + '/pulls', {'head': 'missing'})
        check('GET', prefix + '/pulls/999')
        for content_text in ['aGVsbG8', '-_8=', '!Y WJj$Z A==junk', '====', 'a', '\u0141Q==', '\U0001f600YWJj']:
            check('PUT', prefix + '/contents/base64/' + str(checks), {'content': content_text})
        for content_text in ['\ud841Q==', '\udc41Q==']:
            check('PUT', prefix + '/contents/base64/' + str(checks), {'content': content_text})
        # Empty and fractional pagination and duplicate query keys preserve URLSearchParams order.
        for name in ['other', 'third']:
            check('POST', '/api/v3/user/repos', {'name': name})
        check('GET', '/api/v3/user/repos?x=~&per_page=1&page=1&per_page=2&page=4&x=b')
        check('GET', '/api/v3/user/repos?per_page=0&page=0')
        check('GET', prefix + '/./issues/1')
        check('GET', prefix + '/%2e/issues/1')
        check('POST', prefix + '/%2e/issues', {'title': 'normalized'})
        check('GET', '/api/v3/user', extra={'Host': 'invalid host'})
        check('POST', prefix + '/issues', {}, extra={'Host': 'invalid host'}, auth=False)
        check('GET', '/api/v3/user', extra={'Host': '[::1]:80'})
        check('GET', prefix + '/issues/1', extra={'Host': 'invalid host'})
        for url in ['/api/v3//user', '/api/v3/user?', '/api/v3/user#fragment', '/api/v3/user%3F',
                    '/api/v3/../v3/user', '/api/v3/%2E%2e/v3/user', '/api/v3\\user',
                    '/api/v3/user?bad=%ff', 'http://EXAMPLE.com:80/api/v3/user',
                    'https://example.com:443/api/v3/user', '//example.com/api/v3/user',
                    'http:/example.com/api/v3/user', 'http:example.com/api/v3/user',
                    'ftp://example.com/api/v3/user', 'custom:/api/v3/user', 'data:/api/v3/user',
                    '/api/v3/user?x=%', '/api/v3/user#frag?query']:
            check('GET', url)
        for host in ['entry.example:00080', 'entry.example:', 'entry.example:65536',
                     'entry.example/path', 'u:p@entry.example', 'entry.example?ignored',
                     '127.000.000.001', '2130706433', '0x7f000001', 'é.example', '[::ffff:127.0.0.1]',
                     'a[b', '', '00009', '%EF%BD%85.example', 'e%CC%81.example',
                     'xn--', 'xn--9ca.example', '%C3%9F.example']:
            check('GET', '/api/v3/user/repos?per_page=1', extra={'Host': host})
        check('DELETE', prefix)
        check('GET', prefix)
        check('POST', '/api/v3/user/repos', {'name': 'preview'})
        check('GET', '/api/v3/user/repos')

        # Raw framing tests include pipelining, chunk trailers, limits, and silent close.
        def raw_request(key, wire):
            with socket.create_connection(('127.0.0.1', PORTS[key]), timeout=15) as sock:
                sock.sendall(wire)
                sock.shutdown(socket.SHUT_WR)
                result = bytearray()
                while True:
                    data = sock.recv(65536)
                    if not data:
                        return bytes(result)
                    result.extend(data)

        def check_wire(wire):
            nonlocal checks
            responses = [raw_request(key, wire) for key in ['go', 'ts']]
            checks += 1
            if responses[0] != responses[1]:
                failures.append(('wire', wire[:120], responses))
                print(f'FAIL raw #{checks}: {responses!r}', flush=True)

        auth = b'Authorization: Bearer preview-secret\r\n'
        health = b'GET /healthz HTTP/1.1\r\nHost: entry.example\r\n\r\n'
        check_wire(health + health)
        check_wire(b'HEAD /api/v3/user HTTP/1.1\r\nHost: entry.example\r\n' + auth + b'Connection: close\r\n\r\n')
        for version, connection in [('1.0', ''), ('1.0', 'keep-alive'), ('1.1', 'close')]:
            check_wire(f'GET /healthz HTTP/{version}\r\nConnection: {connection}\r\n\r\n'.encode())
        check_wire(b'POST ' + (prefix + '/issues').encode() + b' HTTP/1.1\r\nHost: entry.example\r\n' + auth +
                   b'Transfer-Encoding: gzip, chunked\r\nExpect: 100-continue\r\n\r\n2;x=y\r\n{}\r\n0\r\nX-Extra: yes\r\n\r\n' + health)
        for body_size in [2_000_000, 2_000_001]:
            check_wire(b'POST /api/v3/user/repos HTTP/1.1\r\nHost: entry.example\r\n' + auth +
                       f'Content-Length: {body_size}\r\n\r\n'.encode() + b' ' * body_size + health)
        check_wire(b'POST /api/v3/user/repos HTTP/1.1\r\n' + auth + b'Transfer-Encoding: chunked\r\n\r\n' +
                   b'1e8481\r\n' + b' ' * 2_000_001 + b'\r\n0\r\n\r\n' + health)
        for wire in [b'BROKEN\r\n\r\n', b'GET /healthz HTTP/1.1\r\nBad Header: x\r\n\r\n',
                     b'GET /healthz HTTP/1.1\r\n: x\r\n\r\n',
                     b'GET /healthz HTTP/1.1\r\nX: \x00\r\n\r\n',
                     b'POST /healthz HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\na',
                     b'POST /healthz HTTP/1.1\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\n\r\n',
                     b'POST /healthz HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\nZ\r\n',
                     b'GET /api/v3/%ff HTTP/1.1\r\n' + auth + b'\r\n',
                     b'GET /healthz HTTP/1.1\r\nX: ' + b'a' * 65537 + b'\r\n\r\n']:
            check_wire(wire)

        # Exercise mutex protection with actual simultaneous issue mutations.
        def concurrent_issue(index):
            status, _, value = request('go', 'POST', prefix + '/issues', {'title': f'parallel {index}'})
            assert status == 201
            return json.loads(value)
        with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
            rows = list(pool.map(concurrent_issue, range(96)))
        assert len({r['id'] for r in rows}) == len({r['number'] for r in rows}) == 96
        checks += 1
        print('PASS: 96 concurrent issue writes have unique IDs and numbers', flush=True)
        DONE = True
        for process in processes:
            code = process.wait(timeout=15)
            assert code == 0, (code, process.stderr.read())
        checks += 1
        print('PASS: coordinator done=true shuts both servers down with exit 0', flush=True)
        missing = subprocess.run([FORGE_BINARY], cwd=ROOT, env={k: v for k, v in os.environ.items() if k != 'MAKEFUN_ENV_URL'}, capture_output=True)
        assert missing.returncode == 1 and missing.stderr == b'MAKEFUN_ENV_URL is not set\n'
        checks += 1
        # Verify lifecycle status handling, retry timing, and strict done JSON.
        class Lifecycle(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers['Content-Length']))
                case = self.path.split('/')[1]
                times = registrations.setdefault(case, [])
                times.append(time.monotonic())
                statuses = {'reject400': [400], 'reject405': [405], 'conflict': [409], 'retry': [503, 200]}[case]
                self.send_response(statuses[min(len(times) - 1, len(statuses) - 1)])
                self.end_headers()

            def do_GET(self):
                case = self.path.split('/')[1]
                count = polls.get(case, 0)
                polls[case] = count + 1
                if case == 'retry' and count == 0:
                    self.close_connection = True
                    return
                replies = [b'{"done":true} trailing', b'{"done":1}', b'{"done":true}']
                self.send_response(200)
                self.end_headers()
                self.wfile.write(replies[min(count, len(replies) - 1)])

        registrations, polls = {}, {}
        lifecycle = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Lifecycle)
        threading.Thread(target=lifecycle.serve_forever, daemon=True).start()
        try:
            for case in ['reject400', 'reject405', 'conflict', 'retry']:
                result = subprocess.run([FORGE_BINARY], cwd=ROOT,
                    env=dict(os.environ, MAKEFUN_ENV_URL=f'http://127.0.0.1:{lifecycle.server_port}/{case}'),
                    capture_output=True, timeout=15)
                if case.startswith('reject'):
                    assert result.returncode == 1 and result.stderr == f'Registration rejected with HTTP {case[6:]}\n'.encode()
                else:
                    assert result.returncode == 0 and polls[case] == 3, (case, result, polls)
                if case == 'retry':
                    assert len(registrations[case]) == 2 and registrations[case][1] - registrations[case][0] >= .18
                checks += 1
        finally:
            lifecycle.shutdown()
            lifecycle.server_close()
        print('PASS: lifecycle 400/405 rejection, 409 acceptance, transient failures, backoff, strict done JSON', flush=True)
        print(f'{checks} checks; {len(failures)} byte-parity failures', flush=True)
        assert not failures
    finally:
        DONE = True
        for process in processes:
            if process.poll() is None:
                process.kill()
            process.communicate(timeout=5)
        coordinator.shutdown()


if __name__ == '__main__':
    main()

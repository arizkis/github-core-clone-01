#!/usr/bin/env python3
"""Offline smart-HTTP parity, real Git clients, and adversarial pack fixtures."""
import concurrent.futures
import gzip
import hashlib
import http.client
import http.server
import json
import os
import random
from pathlib import Path
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import zlib

sys.dont_write_bytecode = True
import preview

ROOT = preview.ROOT
BINARY = preview.FORGE_BINARY
TOKEN = 'Bearer preview-secret'
ZERO = '0' * 40


def pkt(value):
    data = value.encode() if isinstance(value, str) else value
    return f'{len(data)+4:04x}'.encode() + data


def lines(*values):
    return b''.join(b'0000' if v == 'flush' else b'0001' if v == 'delim' else pkt(v) for v in values)


def pack_entry(kind, data, size=None):
    size = len(data) if size is None else size
    out = bytearray()
    first = (kind << 4) | (size & 15)
    size >>= 4
    while size:
        out.append(first | 128)
        first = size & 127
        size >>= 7
    out.append(first)
    return bytes(out) + zlib.compress(data)


def pack(entries, version=2, count=None, extra=b''):
    body = b'PACK' + struct.pack('>II', version, len(entries) if count is None else count) + b''.join(entries) + extra
    return body + hashlib.sha1(body).digest()


def oid(kind, data):
    return hashlib.sha1(kind.encode() + b' ' + str(len(data)).encode() + b'\0' + data).hexdigest()


def command(old, new, ref, caps='report-status side-band-64k'):
    return pkt(f'{old} {new} {ref}\0{caps}\n') + b'0000'


def response_packets(data):
    out = []
    at = 0
    while at < len(data):
        n = int(data[at:at+4], 16)
        if n in (0, 1):
            out.append((n, b''))
            at += 4
        else:
            out.append((n, data[at+4:at+n]))
            at += n
    return out


def main():
    preview.PORTS.clear()
    preview.DONE = False
    coordinator = http.server.ThreadingHTTPServer(('127.0.0.1', 0), preview.Coordinator)
    threading.Thread(target=coordinator.serve_forever, daemon=True).start()
    processes, proxies = [], []
    counts = {'http': 0, 'cli': 0, 'pack': 0, 'concurrent': 0}
    failures, source_limits = [], []
    recordings = {'go': [], 'ts': []}
    try:
        for key, args in [('go', [BINARY]), ('ts', ['node', '--experimental-strip-types', 'main.ts'])]:
            env = dict(os.environ, MAKEFUN_ENV_URL=f'http://127.0.0.1:{coordinator.server_port}/{key}',
                       MAKEFUN_SHOP_TOKEN='preview-secret', NODE_NO_WARNINGS='1')
            processes.append(subprocess.Popen(args, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE))
        deadline = time.monotonic() + 30
        while len(preview.PORTS) < 2 and time.monotonic() < deadline:
            assert all(p.poll() is None for p in processes)
            time.sleep(.02)
        assert len(preview.PORTS) == 2

        def request(key, method, path, data=None, headers=None, auth=True):
            h = {'Host': 'entry.example', 'Connection': 'close'}
            if auth:
                h['Authorization'] = TOKEN
            if data is not None and not isinstance(data, bytes):
                data = json.dumps(data, separators=(',', ':')).encode()
                h['Content-Type'] = 'application/json'
            h.update(headers or {})
            c = http.client.HTTPConnection('127.0.0.1', preview.PORTS[key], timeout=30)
            c.request(method, path, data, h)
            r = c.getresponse()
            result = (r.status, r.getheaders(), r.read())
            c.close()
            return result

        def check(method, path, data=None, headers=None, auth=True, category='http'):
            results = [request(k, method, path, data, headers, auth) for k in ['go', 'ts']]
            counts[category] += 1
            if results[0] != results[1]:
                failures.append((method, path, results))
                print(f'FAIL {method} {path}:', [(r[0], r[1], r[2][:350].hex(), len(r[2])) for r in results], flush=True)
            return results[0]

        user = json.loads(check('GET', '/api/v3/user')[2])['login']
        check('POST', '/api/v3/user/repos', {'name': 'git-preview'})
        rest = f'/api/v3/repos/{user}/git-preview'
        git = f'/{user}/git-preview.git'
        for service in ['git-upload-pack', 'git-receive-pack', 'unknown']:
            for version in ['', 'version=2', 'version=1']:
                check('GET', git + '/info/refs?service=' + service, headers={'Git-Protocol': version})
        check('GET', '/missing/repo.git/info/refs?service=git-upload-pack')
        check('GET', git + '/info/refs?service=git-upload-pack', auth=False)
        check('GET', git + '/info/refs?service=git-upload-pack', headers={'Host': 'invalid host'})
        ls = lines('command=ls-refs\n', 'agent=git/preview\n', 'delim', 'peel\n', 'symrefs\n', 'unborn\n', 'flush')
        check('POST', git + '/git-upload-pack', ls)
        for body in [b'', b'0', b'0002', b'0003', b'0004', b'ffff', b'zzzz', b'0000', lines('command=unknown\n', 'flush'),
                     lines('command=fetch\n', 'flush'), lines('command=fetch\n', 'delim', 'want ' + 'f' * 40 + '\n', 'flush')]:
            check('POST', git + '/git-upload-pack', body)

        def proxy_handler(key):
            class Proxy(http.server.BaseHTTPRequestHandler):
                protocol_version = 'HTTP/1.1'

                def log_message(self, *_):
                    pass

                def forward(self):
                    if self.headers.get('Transfer-Encoding', '').lower() == 'chunked':
                        body = bytearray()
                        while True:
                            n = int(self.rfile.readline().split(b';')[0], 16)
                            if n == 0:
                                while self.rfile.readline() != b'\r\n':
                                    pass
                                break
                            body.extend(self.rfile.read(n))
                            assert self.rfile.read(2) == b'\r\n'
                        body = bytes(body)
                    else:
                        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                    headers = {k: v for k, v in self.headers.items() if k.lower() not in ['connection', 'transfer-encoding', 'content-length']}
                    headers['Connection'] = 'close'
                    c = http.client.HTTPConnection('127.0.0.1', preview.PORTS[key], timeout=30)
                    c.request(self.command, self.path, body if self.command == 'POST' else None, headers)
                    r = c.getresponse()
                    status, rh, rb = r.status, r.getheaders(), r.read()
                    recordings[key].append((self.command, self.path, body, status, rh, rb))
                    self.send_response_only(status)
                    for k, v in rh:
                        self.send_header(k, v)
                    self.end_headers()
                    self.wfile.write(rb)
                    self.close_connection = True
                    c.close()

                do_GET = do_POST = forward
            return Proxy

        for key in ['go', 'ts']:
            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), proxy_handler(key))
            proxies.append(server)
            threading.Thread(target=server.serve_forever, daemon=True).start()
        urls = {key: f'http://127.0.0.1:{proxies[i].server_port}{git}' for i, key in enumerate(['go', 'ts'])}
        with tempfile.TemporaryDirectory(prefix='.git-preview-', dir=ROOT) as temp:
            temp = Path(temp)
            local = temp / 'source.git'
            env = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL='/dev/null', GIT_TERMINAL_PROMPT='0',
                       GIT_AUTHOR_NAME='Preview', GIT_AUTHOR_EMAIL='preview@example.invalid',
                       GIT_COMMITTER_NAME='Preview', GIT_COMMITTER_EMAIL='preview@example.invalid',
                       GIT_AUTHOR_DATE='1735689600 +0000', GIT_COMMITTER_DATE='1735689600 +0000')
            def run(*args, input=None, cwd=local):
                return subprocess.run(['git', '-c', 'protocol.version=2', '-c', 'http.extraHeader=Authorization: ' + TOKEN,
                    '-c', 'pack.threads=1', '-c', 'pack.window=0', *map(str, args)], cwd=cwd, input=input,
                    env=env, capture_output=True, timeout=45)
            assert run('init', '--bare', '--initial-branch=main', local, cwd=temp).returncode == 0

            def git_oid(*args, input=None):
                p = run(*args, input=input)
                assert p.returncode == 0, p.stderr
                return p.stdout.decode().strip()

            def verify_pack(response):
                payload = bytearray()
                in_pack = False
                for _, value in response_packets(response[2]):
                    if value == b'packfile\n':
                        in_pack = True
                    elif in_pack and value:
                        assert value[0] == 1
                        payload.extend(value[1:])
                assert payload[:4] == b'PACK'
                verified = run('index-pack', '--strict', '--stdin', input=payload)
                assert verified.returncode == 0, verified.stderr
                counts['cli'] += 1

            def commit(text, parent=None):
                blob = git_oid('hash-object', '-w', '--stdin', input=text)
                tree = git_oid('mktree', input=f'100644 blob {blob}\tREADME.txt\n'.encode())
                args = ['commit-tree', tree]
                if parent:
                    args += ['-p', parent]
                tip = git_oid(*args, input=b'preview\n')
                git_oid('update-ref', 'refs/heads/main', tip)
                return tip

            def normalize(data):
                for url in urls.values():
                    data = data.replace(url.encode(), b'URL')
                    data = data.replace(url.removesuffix('.git').encode(), b'URL')
                return data.replace(str(temp).encode(), b'TMP')

            def cli(label, args, clones=False):
                before = {k: len(recordings[k]) for k in ['go', 'ts']}
                results = []
                for key in ['go', 'ts']:
                    actual = [arg.replace('{url}', urls[key]).replace('{clone}', str(temp / (label + '-' + key))).replace('{key}', key) for arg in args]
                    r = run(*actual)
                    results.append((r.returncode, normalize(r.stdout).replace((label + '-' + key).encode(), b'clone'),
                                    normalize(r.stderr).replace((label + '-' + key).encode(), b'clone')))
                counts['cli'] += 1
                wire = [recordings[k][before[k]:] for k in ['go', 'ts']]
                if results[0] != results[1] or wire[0] != wire[1]:
                    failures.append(('cli', label, results))
                    print('FAIL CLI', label, results, flush=True)
                    for i, rows in enumerate(wire):
                        print(['go', 'ts'][i], [(x[0], x[1], len(x[2]), x[3], len(x[5])) for x in rows], flush=True)
                if clones and results[1][0] != 0:
                    source_limits.append((label, results[1][2].decode(errors='replace')))
                    print('SOURCE LIMIT', label, results[1][2].decode(errors='replace'), flush=True)
                if b'unexpected disconnect while reading sideband packet' in results[1][2] and not any(x[0] == 'push-sideband-flush' for x in source_limits):
                    source_limits.append(('push-sideband-flush', results[1][2].decode(errors='replace')))
                    print('SOURCE LIMIT push-sideband-flush:', results[1][2].decode(errors='replace'), flush=True)
                return results[0]

            initial = commit(b'first version\n')
            assert b'[new branch]' in cli('initial-push', ['push', '--porcelain', '{url}', 'main:main'])[1]
            check('GET', git + '/info/refs?service=git-receive-pack')
            check('POST', git + '/git-upload-pack', ls)
            assert cli('ls-remote', ['ls-remote', '--symref', '{url}'])[0] == 0
            cli('clone-full', ['clone', '--no-checkout', '{url}', '{clone}'], clones=True)
            cli('clone-depth1', ['clone', '--depth', '1', '--no-checkout', '{url}', '{clone}'], clones=True)
            fetch = lines('command=fetch\n', 'delim', f'want {initial}\n', 'done\n', 'flush')
            verify_pack(check('POST', git + '/git-upload-pack', fetch))
            check('POST', git + '/git-upload-pack', fetch)
            second = commit(b'second version\n', initial)
            cli('ff-push', ['push', '--porcelain', '{url}', 'main:main'])
            check('GET', git + '/info/refs?service=git-receive-pack')
            check('POST', git + '/git-upload-pack', ls)
            for key in ['go', 'ts']:
                destination = temp / ('fetch-' + key)
                assert run('init', '--bare', '--initial-branch=main', destination, cwd=temp).returncode == 0
            cli('fetch-after-push', ['-C', str(temp / 'fetch-{key}'), 'fetch', '{url}'], clones=True)
            cli('non-ff-push', ['push', '--porcelain', '{url}', initial + ':refs/heads/main'])
            assert cli('forced-non-ff-push', ['push', '--force', '--porcelain', '{url}', initial + ':refs/heads/main'])[0] != 0
            cli('branch-create', ['push', '--porcelain', '{url}', initial + ':refs/heads/topic'])
            cli('branch-delete', ['push', '--porcelain', '{url}', ':refs/heads/topic'])
            tag_data = f'object {second}\ntype commit\ntag preview\ntagger Preview <preview@example.invalid> 1735689600 +0000\n\nannotated\n'.encode()
            tag = git_oid('hash-object', '-t', 'tag', '-w', '--stdin', input=tag_data)
            git_oid('update-ref', 'refs/tags/preview', tag)
            cli('tag-create', ['push', '--porcelain', '{url}', 'refs/tags/preview'])
            check('GET', git + '/info/refs?service=git-receive-pack')
            check('POST', git + '/git-upload-pack', ls)
            cli('ls-remote-tags', ['ls-remote', '--symref', '{url}'])
            for depth in ['', 'deepen 1\n', 'deepen 2\n', 'deepen invalid\n']:
                args = ['command=fetch\n', 'delim', f'want {second}\n']
                if depth:
                    args += [depth]
                args += ['done\n', 'flush']
                response = check('POST', git + '/git-upload-pack', lines(*args))
                assert response[0] == 200
                check('POST', git + '/git-upload-pack', lines(*args))
            check('POST', git + '/git-upload-pack', lines('command=fetch\n', 'delim', f'want {second}\n', f'have {initial}\n', 'done\n', 'flush'))
            check('POST', git + '/git-upload-pack', lines('command=fetch\n', 'delim', f'want {second}\n', f'have {second}\n', f'shallow {second}\n', 'deepen 2\n', 'done\n', 'flush'))
            # Parallel cache hits share identical immutable HTTP and sideband bytes.
            cached_request = lines('command=fetch\n', 'delim', f'want {second}\n', 'deepen 1\n', 'done\n', 'flush')
            with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
                replies = list(pool.map(lambda _: request('go', 'POST', git + '/git-upload-pack', cached_request), range(64)))
                ts_replies = list(pool.map(lambda _: request('ts', 'POST', git + '/git-upload-pack', cached_request), range(64)))
            reference = ts_replies[0]
            assert all(r == reference for r in replies + ts_replies)
            counts['concurrent'] += 1

            # New REST-generated objects force the server compression path.
            check('POST', '/api/v3/user/repos', {'name': 'rest-git'})
            for name, data in [('hello', b'hello\n'), ('repeat', b'abc' * 200), ('binary', bytes(range(256)) * 4)]:
                check('PUT', f'/api/v3/repos/{user}/rest-git/contents/' + name,
                      {'content': __import__('base64').b64encode(data).decode()})
            heads = json.loads(check('GET', f'/api/v3/repos/{user}/rest-git/branches')[2])
            rest_tip = heads[0]['commit']['sha']
            verify_pack(check('POST', f'/{user}/rest-git.git/git-upload-pack', lines('command=fetch\n', 'delim', f'want {rest_tip}\n', 'done\n', 'flush')))

            # Adversarial command lists and gzip are compared as implemented.
            for data in [b'', b'0', b'xxxx', b'0001', b'0004', b'ffff', pkt('done\n'), pkt('shallow ' + initial + '\n') + b'0000',
                         command(ZERO, second, 'refs/heads/new'), command(second, initial, 'refs/heads/main'),
                         command(ZERO, 'f' * 40, 'refs/heads/missing'), command(ZERO, ZERO, 'refs/heads/absent'),
                         command(ZERO, second, 'not-a-standard-ref'), command(ZERO, second, 'refs/heads/ignoredcaps', 'unrecognized')]:
                check('POST', git + '/git-receive-pack', data)
            for suffix in ['\n\n', '\r\n', '\n\0caps\n', '\u2028', '\u2029', '\u00a0', '']:
                check('POST', git + '/git-receive-pack', pkt(f'{ZERO} {ZERO} refs/heads/absent' + suffix) + b'0000')
            check('POST', git + '/git-receive-pack', gzip.compress(command(ZERO, ZERO, 'refs/heads/absent'), mtime=0), headers={'Content-Encoding': 'gzip'})

            content = b'rollback-test-content'
            blob = oid('blob', content)
            valid_entry = pack_entry(3, content)
            bad_streams = [b'', b'\x00', b'\x00\x00', b'\x78\x9c\x07', b'\x78\x9c\x01\x01\x00\x00\x00',
                           zlib.compress(content)[:-1], zlib.compress(content)[:-4] + b'\x00' * 4]
            cases = [b'', b'PACK', b'not a pack' * 4, pack([], version=4), pack([], count=1),
                     pack([b'\x00']), pack([b'\x50']), pack([b'\x60']), pack([b'\x70']),
                     pack([b'\xb0']), pack([b'\xb0' + b'\x80' * 9]), pack([pack_entry(3, content, size=1)]),
                     pack([], extra=b'junk'), pack([valid_entry]), pack([valid_entry], version=3)]
            cases += [pack([bytes([0x30]) + stream]) for stream in bad_streams]
            # A corrupted later object must not insert the preceding valid blob.
            cases += [pack([valid_entry, b'\x00']), pack([valid_entry, b'\x60'])]
            for i, candidate in enumerate(cases):
                if candidate == b'':
                    continue
                ref_name = f'refs/heads/pack-{i}'
                check('POST', git + '/git-receive-pack', command(ZERO, blob, ref_name) + candidate, category='pack')
            corrupt = bytearray(pack([valid_entry])); corrupt[-1] ^= 1
            check('POST', git + '/git-receive-pack', command(ZERO, blob, 'refs/heads/checksum') + corrupt, category='pack')
            rollback_data = b'this blob must remain absent after failure'
            rollback_hash = oid('blob', rollback_data)
            rollback_entry = pack_entry(3, rollback_data)
            result = check('POST', git + '/git-receive-pack', command(ZERO, rollback_hash, 'refs/heads/rollback') + pack([rollback_entry, b'\x00']), category='pack')
            assert b'unpacker error' in result[2]
            result = check('POST', git + '/git-receive-pack', command(ZERO, rollback_hash, 'refs/heads/rollback'), category='pack')
            assert b'missing object' in result[2]
            # DEFLATE errors are part of the observable unpack report, too.
            rng = random.Random(33944905)
            payload = bytes(rng.randrange(16) for _ in range(1000))
            compressed = zlib.compress(payload)
            for _ in range(128):
                damaged = bytearray(compressed)
                at = rng.randrange(len(damaged))
                damaged[at] ^= 1 << rng.randrange(8)
                entry = pack_entry(3, payload)[:-len(compressed)] + damaged
                check('POST', git + '/git-receive-pack', command(ZERO, ZERO, 'refs/heads/fuzz') + pack([entry]), category='pack')
            check('POST', git + '/git-upload-pack', ls)
            # Cache generation after removing/recreating HEAD, including unborn.
            check('POST', git + '/git-receive-pack', command(second, ZERO, 'refs/heads/main'))
            check('POST', git + '/git-upload-pack', ls)
            check('GET', git + '/info/refs?service=git-receive-pack')
            check('POST', git + '/git-receive-pack', command(ZERO, second, 'refs/heads/main'))
            check('POST', git + '/git-upload-pack', ls)

        preview.DONE = True
        for p in processes:
            assert p.wait(timeout=15) == 0, p.stderr.read()
        print('Git parity counts:', counts, 'failures:', len(failures), flush=True)
        print('Source client limitations:', source_limits, flush=True)
        assert not failures
    finally:
        preview.DONE = True
        for p in processes:
            if p.poll() is None:
                p.kill()
            p.communicate(timeout=5)
        for p in proxies:
            p.shutdown()
            p.server_close()
        coordinator.shutdown()
        coordinator.server_close()


if __name__ == '__main__':
    main()

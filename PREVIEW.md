# Go forge preview — turns 1 and 2

Turn 1 ports the deterministic scenario, shared store, HTTP transport, process
lifecycle, and every REST route in `lib/rest.ts`. Turn 2 ports the checked-in
`lib/gitpack.ts` and `lib/gitproto.ts`, including their observable limitations.
The existing TypeScript entry point remains the reference. GraphQL, MCP, and
the thin `main.ts` binary launcher are reserved for the following turns.

**This is a source-parity preview. The checked-in TypeScript fails real Git
clone/fetch and reports a sideband error after applying pushes; Go reproduces
those wire bytes. Details and deferred features appear below.**

## Build and run

The preview has no Go module dependencies. On the supplied Linux amd64 toolchain:

```sh
export PATH=/home/hatch/workspace/tools/go/bin:$PATH
GOPROXY=off CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
MAKEFUN_ENV_URL=http://127.0.0.1:COORDINATOR_PORT MAKEFUN_SHOP_TOKEN=TOKEN ./forge
```

`forge` listens on an ephemeral port on `127.0.0.1`, posts `{"port":N}` to the
coordinator's `/register`, and polls `/done`. Its in-memory state starts empty.
No competition submission is performed by the binary or the preview checks.

The committed preview binary is a stripped, statically linked Linux amd64 ELF,
7,303,328 bytes, built with Go 1.27.4 and `CGO_ENABLED=0`.

## Files

- `scenario.go`: unsigned 64-bit identity PRNG and memoized credential verdict.
- `store.go`: ordered state, IDs, fixed timestamps, cached serialization, git
  object storage, and tree/commit helpers used by REST.
- `server.go`: raw HTTP/1.0 and HTTP/1.1 transport, keep-alive, pipelining,
  chunked bodies and trailers, framing validation, and the 2,000,000-byte limit.
- `main.go`: coordinator registration, completion polling, and process exits.
- `rest.go`: application routing, REST resources, diffs, and merge semantics.
- `json.go`, `url.go`: ordered JSON, JavaScript coercions and JSON diagnostics,
  Unicode handling, URL normalization, and ordered pagination query encoding.
- `gitpack.go`: pkt-lines, sideband framing, ordered reachability, pack checksums,
  object SHA-1, DEFLATE validation, reusable ingest scratch, and retained entries.
- `gitproto.go`: raw-path git auth/routing, advertisements, ls-refs, fetch,
  receive-pack, ref updates, report status, generation caches, and single-flight.
- `deflate.go`: pure-Go compression matching the local Node reference, including
  its four-byte hash, lazy matching, window behavior, and Huffman choices.
  [ZLIB-NOTICE.txt](ZLIB-NOTICE.txt) contains the algorithm acknowledgments and
  license notices.
- `go.mod`, executable `forge`, and `tests/preview.py`.

All dispatches hold the store mutex. Observable collections use insertion order;
cached response bytes are immutable. A normal authenticated issue GET uses its
prebuilt response bytes. HTTP responses preserve header order and the reference's
route-specific newline behavior, without automatic Date headers or HTML escaping.

## Local validation

```sh
python3 tests/preview.py
python3 tests/git_preview.py
GOPROXY=off go test -count=1 ./...
GOPROXY=off CGO_ENABLED=1 go test -race -count=1 ./...
GOPROXY=off go vet ./...
```

The offline harness starts Go and TypeScript against a local stub coordinator.
Its 390 checks cover response status, ordered headers and raw JSON bytes, REST
mutations, git object hashes, pagination, Unicode, malformed JSON, URL handling,
raw request framing, overflow draining, and lifecycle status/retry behavior.
It invokes curl for health, a 401 response, and authenticated issue creation.
It also performs 96 simultaneous Go issue writes and checks ID/number uniqueness.
The same harness passed against a separate race-instrumented build with no race
reports; `go vet ./...` also passed.
This is a preview validation suite, not the competition's complete 120-check
conformance suite or an official performance measurement.

Turn 2 adds 247 checks: 78 direct HTTP comparisons, 154 pack/error/rollback
comparisons, 14 real-Git scenarios/pack validations, and a concurrent fetch wave.
Real Git requests and complete responses are recorded by local forwarding
proxies and compared byte for byte. Scenarios include initial/fast-forward
pushes, forced and unforced non-fast-forward rejection, branch creation/deletion,
annotated tags and peeling, unborn HEAD, clone attempts, and fetch after push.
Extracted full packs pass real Git's `index-pack --strict --stdin`.

Five Go tests cover 55 compression fixtures against Node's `deflateSync`, valid
inflate boundaries, sideband boundaries, single-flight sharing/failure cleanup,
generation/exact-request cache keys, and a single Write for cached replies.
The compressor is verified against this environment's Node 22.21.0 and native
zlib 1.3.1-470d3a2. Other native-zlib builds have not been verified.
The [Node-bundled algorithm](https://github.com/nodejs/node/tree/v22.21.0/deps/zlib)
is the compression reference; the executable uses only Go standard-library
imports and performs compression locally.

Final validation passed all 637 harness checks on both the static binary and a
separate race-instrumented binary, plus all five Go tests under the race detector
and `go vet ./...`. No race reports were emitted. The source limitations above
remain failures for real Git clients even though the parity comparisons pass.

Fetch caches hold fully sideband-framed bytes and prebuilt HTTP response bytes
for keep-alive and close connections. Concurrent callers share the same
immutable response. A cache hit does not recompress or reframe objects. Retained
entries bypass compression entirely. Ref-tip parent metadata speeds repeated
ancestry checks without changing traversal/error precedence.

## Turn 2 source conflicts, measured failures, and deferrals

The requested expanded features are absent from this checkout. The checked-in
TypeScript remains authoritative for this port; its files were not modified.

- Upload-pack always advertises protocol v2, regardless of Git-Protocol headers.
  Receive-pack always advertises v0 using the OID-patched template and the
  reference capability list, including `ofs-delta`.
- Nevertheless, both incoming delta forms (6 and 7) are rejected with
  `Delta objects are unsupported`. Outgoing packs retain full entries or deflate
  full objects in the established order; they do not create offset deltas.
  There is no retained-base eviction policy, pack-index writer, or REST pack/index
  download route to port. Those requested additions are deferred.
- `app.ts` and `gitproto.ts` have no gzip request inflation. Gzipped receive-pack
  input receives the same command-parser failure in both implementations. There
  is no progress stream; reports occupy sideband 1. Shallow lines and `done`
  in receive-pack command lists fail validation rather than being skipped.
- Pack parsing verifies the trailing checksum before decoding and stages all
  objects until the complete pack validates. A bad pack publishes no objects or
  validated entries. Ref processing occurs afterward, sequentially: ordinary
  per-ref failures do not roll back earlier successful refs or ingested objects.
- Real Git 2.43.0 pushes apply accepted refs but exit 1 with
  `unexpected disconnect while reading sideband packet`, because the reference
  omits the outer sideband flush. Both full and depth-1 clone, and subsequent
  fetch, fail with `expected 'packfile', received 'acknowledgments'`. The reference
  always sends the acknowledgments section even after `done`. These failures are
  explicitly reported by the harness; they are not treated as successful clones.
- Depth-1 closure visits only the commit, root tree, and its immediate entries,
  as in the source. No recursive shallow-tree correction is introduced.

## Source discrepancies and interpretation

- `main.ts` retries transient registration and polling transport failures with
  delays of 200, 400, 800, 1,600, then 2,000 ms. This differs from the brief's
  approximately 250 ms polling / transport-error exit description. The Go port
  follows the TypeScript: registration HTTP 400/405 exits 1, HTTP 200/409 succeeds,
  other failures retry, and only a JSON boolean `done: true` completes normally.
- The reference uses `server.listen(0)` without a host, whereas this preview
  follows the brief's explicit loopback-only `127.0.0.1` requirement.
- `lib/store.ts` exports its store singleton but contains no state-export
  endpoint or snapshot format. No new wire-visible export format is invented.
  The singleton, counters, insertion order, fixed timestamps, and serialization
  remain deterministic; there is no advancing logical timestamp in the source.

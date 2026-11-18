# Go forge preview — turns 1, 2, 3, and 4

## Turn 4 performance measurements

The retained change is limited to creation of new issues. It serializes the
ordered response directly into one owned buffer, shared by the cached JSON
and newline-terminated response views. IDs, numbers, timestamps, label order,
escaping, invalidation, and update/read serialization are unchanged. No push
or clone code changes remain.

`tests/perf_driver.ts` invokes the kit's exported benchmark with the exact
workloads: 50 concurrent depth-1 clones of a 200-commit repository, or 200
sequential authenticated issue creations per round. The diagnostic-only
`perfprofile` build tag exposes CPU, allocation, mutex, and blocking profiles
on a separate loopback listener selected with `FORGE_PPROF_ADDR`; it is excluded
from the shipped binary. Profiles ran for 20 seconds per workload.

Clone CPU samples were led by syscalls (30.0%), routing regex work, and request
handling. Pack construction was already effectively eliminated by warm-ups.
The global store lock covers construction, but aggregate measured store-lock
wait was only 78.1 ms over the 20-second profile. Issue creation accounted for
34.2% of sampled write allocations, including generic JSON objects and a second
cached-body copy. After the retained serializer change, sampled allocation
bytes per created issue decreased approximately 56% within CreateIssue and
14% across the whole workload (normalized by completed batches).

Two clone candidates were rejected and fully reverted:

- Snapshot immutable git objects once per flight, release the store mutex for
  construction and waiting, and bypass parsing on cached fetches: clone 37
  versus baseline 39 points, so the extra snapshot/refactor was removed.
- Cache validated upload routes, fully frame cached ls-refs responses, and
  bypass packet parsing on fetch cache hits: paired warmed clone measurements
  improved (baseline 47/47/49 versus candidate 50/50/50), but complete local
  grades scored clone 38 and 36 versus baseline 39. Those changes were removed
  because the workload points did not hold in the complete grader.

Every local grader invocation used one process-driver instance through
`node main.ts`. All had feature 600, agent 200, and identical canonical state
SHA-256 `a0e98d49854b961230227867235c8615858e006db3763f341ac6154af1445590`.

| Run | Score | Perf | Push | Clone | Read | Write |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Unchanged baseline | 985 | 185 | 50 | 39 | 50 | 46 |
| Snapshot candidate (reverted) | 984 | 184 | 50 | 37 | 50 | 47 |
| Smaller clone candidate (reverted) | 988 | 188 | 50 | 38 | 50 | 50 |
| Smaller clone confirmation (reverted) | 982 | 182 | 50 | 36 | 50 | 46 |
| Final write-only tuning | 983 | 183 | 49 | 34 | 50 | 50 |

Baseline/final p50 milliseconds: push 351.596/424.257, clone 120.940/162.161,
read 614.573/653.764, write 110.524/88.940. The final run improved write points;
the complete perf score decreased due to slower measurements on unchanged
paths. These are local advisory measurements, not a production ranking claim.
The process driver is unisolated, and its Node 22 differs from the server's
Node 26; the grader reports that mismatch. L1 replay reproduced on every run.

Final validation before packaging: 390 REST/lifecycle checks, 247 git checks,
195 GraphQL/MCP checks, and 4 launcher checks (836 total), all passing. The
first three harnesses also passed against the race binary (832 checks), as
did all five Go tests, race-enabled Go tests, and go vet. The existing real-git
source limitations described below remain byte-identical. The stripped static
binary is 7,450,784 bytes and executable mode 100755. No submission was made.

Final packaging removes this preview document, `tests/`, and the diagnostic
profile build file in a separate commit, while keeping ZLIB-NOTICE.txt.

Turn 1 ports the deterministic scenario, shared store, HTTP transport, process
lifecycle, and every REST route in `lib/rest.ts`. Turn 2 ports the checked-in
`lib/gitpack.ts` and `lib/gitproto.ts`, including their observable limitations.
Turn 3 ports GraphQL and MCP, replaces `main.ts` with the thin binary launcher,
and removes `app.ts` and the entire `lib/` directory. Parity tests recover the
unchanged TypeScript reference from commit `d9b97e1` into a temporary directory;
no TypeScript implementation remains in the current tree.

**This is a source-parity preview. The checked-in TypeScript fails real Git
clone/fetch and reports a sideband error after applying pushes; Go reproduces
those wire bytes. Details and deferred features appear below.**

## Build and run

The preview has no Go module dependencies. On the supplied Linux amd64 toolchain:

```sh
export PATH=/home/hatch/workspace/tools/go/bin:$PATH
GOPROXY=off CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
MAKEFUN_ENV_URL=http://127.0.0.1:COORDINATOR_PORT MAKEFUN_SHOP_TOKEN=TOKEN node main.ts
```

`forge` listens on an ephemeral port on `127.0.0.1`, posts `{"port":N}` to the
coordinator's `/register`, and polls `/done`. Its in-memory state starts empty.
No competition submission is performed by the binary or the preview checks.

The committed preview binary is a stripped, statically linked Linux amd64 ELF,
7,450,784 bytes, built with Go 1.27.4 and `CGO_ENABLED=0`, committed as mode
`100755`. The launcher locates it relative to its own file, inherits environment
and stdio, waits for the child, and returns its exit code. Spawn failure exits 1.

## Files

- `scenario.go`: unsigned 64-bit identity PRNG and memoized credential verdict.
- `store.go`: ordered state, IDs, fixed timestamps, cached serialization, git
  object storage, and tree/commit helpers used by REST.
- `server.go`: raw HTTP/1.0 and HTTP/1.1 transport, keep-alive, pipelining,
  chunked bodies and trailers, framing validation, and the 2,000,000-byte limit.
- `main.go`: coordinator registration, completion polling, and process exits.
- `rest.go`: application routing, REST resources, diffs, and merge semantics.
- `graphql.go`: the source tokenizer/parser, projections, introspection, node IDs,
  mutations, batching, coercion, field ordering, and error envelopes.
- `mcp.go`: JSON-RPC, protocol discovery/initialization, six tool schemas and
  calls, and MCP-specific public object serialization.
- `main.ts`: the nine-line Node standard-library launcher.
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
python3 tests/api_preview.py
python3 tests/launcher_preview.py
GOPROXY=off go test -count=1 ./...
GOPROXY=off CGO_ENABLED=1 go test -race -count=1 ./...
GOPROXY=off go vet ./...
```

The offline harness starts the Go binary through `node main.ts` and the
historical TypeScript reference against a local stub coordinator.
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

Turn 3 adds 195 GraphQL/MCP byte-parity checks and four launcher checks. Final
validation passed all 836 checks. The 832 REST, git, GraphQL, and MCP checks also
passed against a separate race-instrumented binary; all five Go tests passed
under the race detector, and `go vet ./...` passed. No race reports were emitted.
The source limitations remain failures for real Git clients even though parity
comparisons pass. Launcher tests include execution from another working
directory, inherited environment/stdio, child exit propagation, and spawn failure.

Fetch caches hold fully sideband-framed bytes and prebuilt HTTP response bytes
for keep-alive and close connections. Concurrent callers share the same
immutable response. A cache hit does not recompress or reframe objects. Retained
entries bypass compression entirely. Ref-tip parent metadata speeds repeated
ancestry checks without changing traversal/error precedence.

## Turn 2 source conflicts, measured failures, and deferrals

The requested expanded features are absent from the reference. The historical
TypeScript remains authoritative for this port; it was removed after validation.

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

## Turn 3 source discrepancies and interpretation

- The GraphQL route in `app.ts` is `/api/graphql`, not `/graphql`. The latter
  remains 404. The source uses a tokenizer/parser and field resolver, with no
  query-prefix dispatch or normalized-query shortcut. Its root fields are
  `viewer`, `repository`, `node`, and `__type`; no `users` root field is defined.
- Node IDs use permissive base64 decoding and JavaScript numeric coercion.
  Unknown/malformed IDs resolve to null or the mutation's existing not-found
  error; there is no dedicated node-ID decoding error to invent.
- MCP contains exactly the six advertised tools. Neither `exportState` nor
  `exportRepoZip` exists in `lib/mcp.ts`, `lib/store.ts`, or another forge module.
  Their requested canonical document and archive format therefore cannot be
  determined from the source and remain deferred. They return the reference's
  unknown-method/tool error. The kit environment owns its canonical `state.json`;
  the local grade's live/replay comparison verifies that document independently.
- MCP public issue/repository/pull shapes differ from REST in field order and
  optional fields. MCP merge marks a pull closed/merged without making the
  REST/GraphQL git merge. Those differences are retained.
- All TypeScript implementation files were removed. `main.ts`, `LICENSE`,
  `package.json`, `tsconfig.json`, README badges, and the referral line remain;
  the badges/referral were compared byte for byte to the preceding commit.

## Earlier source discrepancies and interpretation

- The original TypeScript lifecycle retries registration/polling failures with
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

## Turn 3 kit local grade

The final code snapshot was graded through the kit's Node `main.ts` entrypoint,
on one practice instance with the live run and the kit's replay check:

```sh
node /home/hatch/workspace/makefun/projects/github-core-clone-01/kit-fresh-1118/github-core-clone-01-kit/cli/makefun.mjs grade-local \
  --kit /home/hatch/workspace/makefun/projects/github-core-clone-01/kit-fresh-1118/github-core-clone-01-kit \
  --bot /home/hatch/workspace/makefun/projects/github-core-clone-01/entry-go \
  --worktree --driver process --instances 1 \
  --save /home/hatch/workspace/makefun/projects/github-core-clone-01/go-turn3-grade-final --json
```

The Docker-driver invocation was attempted first and could not run: this host
has no Docker engine or executable. The process driver runs the entry and suite
unisolated on loopback. The kit also reports this exact runtime mismatch:
`node: the server's harness runs v26.13.1, this one v22.21.0`.
These are local validation figures, not official results or a ranking forecast.

| Column | Final local value |
| --- | ---: |
| score | 989 |
| feature | 600 |
| agent | 200 |
| perf | 189 |
| perf_p50_us | 218344 |
| working | 1 |

All 120 conformance checks passed: git 30/30, GraphQL 20/20, issues 15/15,
MCP 15/15, protocol 5/5, pulls 20/20, repositories 15/15.
**Failed feature or agent checks: none.**

Replay finished successfully and L1 reproduced. Both live and replay state
documents have SHA-256
`a0e98d49854b961230227867235c8615858e006db3763f341ac6154af1445590`.
The final workload points were push 50, clone 39, read 50, write 50. An earlier
local run scored 987 with the same 120 passing checks; the performance difference
is a local timing measurement, not a functional improvement claim.

The retained run is in the sibling `go-turn3-grade-final/` directory, including
`summary.json`, `trial-0/state/state.json`, and `trial-0/state/perf.json`. No model
requests were made. Transcripts were not attached to this local preview run, so
its eligibility metadata records `no_transcripts`, `coverage_below_gate`, and
`cost_unpriced`; it is not a submission or transcript-eligibility check.

# Go REST preview

Turn 1 ports the deterministic scenario, shared store, HTTP transport, process
lifecycle, and every REST route in `lib/rest.ts`. The existing TypeScript entry
point remains available as the reference. Git smart HTTP, GraphQL, MCP, and the
thin `main.ts` binary launcher are reserved for the following turns.

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
7,176,352 bytes, built with Go 1.27.4 and `CGO_ENABLED=0`.

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
- `go.mod`, executable `forge`, and `tests/preview.py`.

All dispatches hold the store mutex. Observable collections use insertion order;
cached response bytes are immutable. A normal authenticated issue GET uses its
prebuilt response bytes. HTTP responses preserve header order and the reference's
route-specific newline behavior, without automatic Date headers or HTML escaping.

## Local validation

```sh
python3 tests/preview.py
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

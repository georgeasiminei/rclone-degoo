# Scalable storage baseline — 2026-10-15

This is the first P1 implementation slice from
[SCALABLE_STORAGE_PLAN.md](SCALABLE_STORAGE_PLAN.md). It measures the current
layout and adds failure regressions. It does not select or implement a new
remote format. All provider responses and object names are generated locally;
no Degoo account, installed mount, pending cache, or remote vault is used.

## Reproduce

Run the ordinary correctness and failure tests:

```sh
RCLONE_CONFIG=/notfound go test ./backend/degoo/... -count=1
```

Run the larger directory correctness fixture explicitly, without other heavy
validation in parallel:

```sh
RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 \
  go test ./backend/degoo -run '^TestScalabilityDirectory$' -count=1 -v
```

The ordinary test includes 1,000 and 10,000 entries; the environment switch
also includes 100,000 entries. This separates a slow baseline measurement from
routine race/CI checks: the existing backend gives a metadata operation ten
seconds, including all pages and local hint processing. Resource contention
or instrumentation can exhaust that deadline. The fixture preserves it and
reports failure rather than extending it to make the baseline pass.

Collect timings, allocations, response bytes, requests, pages, and cache hits:

```sh
RCLONE_CONFIG=/notfound go test ./backend/degoo -run '^$' \
  -bench '^BenchmarkScalabilityDirectory$' -benchtime=1x -benchmem -count=1
```

Use the single-iteration command for comparison. The snapshot lifetime is five
seconds; longer warm benchmarks can include expiry and must not be interpreted
as exclusively warm access; unexpected HTTP work makes the benchmark fail.
Benchmark allocations include the localhost HTTP
server, response generation, HTTP/JSON processing, and backend work. They are
cumulative allocated bytes per operation, not peak or retained index memory.
Response bytes count generated GraphQL JSON bodies, excluding HTTP headers,
requests, TLS, and object content. Directory initialization and authentication
are excluded. Timing is descriptive, with no machine-specific pass threshold.

The fixture generates only one response page at a time. It checks requested
parent, limit, and cursor alignment. Correctness checks verify every returned
name in sorted order, the final child's size, exact page/request counts,
cache reuse, direct-ID lookup, and a new complete scan after explicit refresh.

## Current layout measurements

The initial Linux amd64 / Go 1.26.0 correctness run used 1,000-entry responses.
The target was the last child, forcing lookup to examine the whole listing.

| Siblings | Cold lookup pages / requests | Response bytes | Cold lookup | Warm lookup | Cold full list | Warm full list |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 1 / 1 | 166,056 | 12.4 ms | 0.061 ms | 10.8 ms | 1.10 ms |
| 10,000 | 10 / 10 | 1,660,596 | 381 ms | 0.774 ms | 395 ms | 9.75 ms |
| 100,000 | 100 / 100 | 16,606,086 | 5.61 s | 15.5 ms | 5.69 s | 98.5 ms |

Every warm lookup and warm full list had one snapshot hit and zero HTTP
requests/response bytes. Refresh caused another 1, 10, or 100 listing pages.
Direct-ID lookup used one `GetOverlay4`, zero listing pages, and a 190-byte
response at all three scales. These are fixture contracts, not verification
that Degoo currently returns matching metadata or stable IDs.

An initial single-iteration benchmark measured warm-lookup allocations of
139,872, 1,368,672, and 13,607,520 bytes at the three scales. The snapshot hit
copies the full `[]Item` before `lookupChildInItems` scans it. Zero requests
therefore does not mean bounded local lookup work. Full logical enumeration
necessarily remains proportional to child count.

The fixture also compares response ceilings of 1,000 and 100 items while the
client continues requesting its existing limit of 1,000. This models smaller
provider pages; it does not implement a client page-size option or measure
live throttling. At 10,000 entries, the smaller response ceiling raises calls
from 10 to 100 and JSON bytes from 1,660,596 to 1,665,987. It cannot eliminate
the complete cold scan. In the final isolated benchmark at 100,000 entries,
1,000-item responses took 100 requests / 16,606,086 JSON bytes and 100-item
responses took 1,000 requests / 16,660,887 bytes. Cold lookup was approximately
5.64 versus 5.90 seconds in that single sample. This includes the local hint
processing cost and is not a latency distribution or a provider recommendation.

A separate local cost is visible in `rememberItemParentLocked`: once the
4,096-entry hint table fills, each insertion scans it to find the oldest
sequence. This adds repeated table walks during a large listing while holding
the metadata mutex. It is a candidate for a focused bounded-cache improvement;
this slice records the baseline without changing eviction behavior.

No persistent path-to-ID index exists in this layout. Peak retained metadata
memory, snapshot occupancy across many parents, and persistent-index size for
the candidate designs still need measurement. Allocation counts cannot close
that part of P1-FIXTURES.

## Operation tracing

`scalability_lookup_test.go` traces the real backend against its generated HTTP
mock. Constructor/root validation is excluded. The write cases use a small
single-page parent and default upload verification. They establish request
fan-out and invalidation, not large-directory mutation latency or cloud
consistency.

| Operation | Listing requests | Direct metadata reads | Payload reads | Mutation / parent / global invalidations |
| --- | ---: | ---: | ---: | ---: |
| Cold `NewObject` | 1 | 0 | 0 | 0 / 0 / 0 |
| Warm `NewObject` | 0 | 0 | 0 | 0 / 0 / 0 |
| Open with listed URL | 0 | 0 | 1 | 0 / 0 / 0 |
| Create | 3 | 2 | 1 | 2 / 8 / 0 |
| Overwrite | 4 | 2 | 1 | 4 / 12 / 0 |
| Rename, source resolved | 1 | 0 | 0 | 1 / 2 / 0 |
| Delete, source resolved | 1 | 0 | 0 | 1 / 4 / 0 |

Fresh listings are included in listing counts and parent invalidations; these
are event counters, not distinct directories. Payload reads include upload
verification. Write authorization, storage upload, registration, rename, and
delete requests are asserted separately by operation in the tests.

The direct-ID tests check success and a null overlay response. A missing
opaque ID becomes an `api.Error`, rather than `fs.ErrorObjectNotFound`, with no
fallback listing. This distinction must remain explicit when an index starts
relying on IDs. Provider deletion, stale IDs, mismatched parent/name, and auth
failure semantics need further evaluation before choosing the indexed format.

The chunker fixture composes real chunker over Degoo with a generated manifest
and two data chunks, with zero and 256 unrelated siblings. Discovery uses one
HTTP parent listing; the backend snapshot serves chunker's additional sibling
scan locally (one miss and one hit). It performs no direct metadata or payload
reads during `NewObject`. Chunker open/read tracing remains a gap because the
shared mock does not support its ranged GET. This isolates chunk discovery;
it does not exercise the supported crypt layer or a mounted large logical file.

## Rate-limit and cancellation evidence

New regressions cover:

- A successful first page followed by a second-page HTTP 429 with a
  120-second `Retry-After`. The call returns an error and no partial children;
  no partial snapshot is stored.
- A separate small folder succeeds and is cached before that 429. Subsequent
  access to that cached folder fails with the same global cooldown deadline,
  without HTTP activity. This records the conservative policy's effect; it
  does not establish the provider's quota scope.
- Caller cancellation while the second page is in flight stops the request,
  returns `context.Canceled`, and exposes/caches no partial directory.

Existing `TestHTTP429HonorsLongRetryAfterAndBlocksBeforeToken` and
`TestRateLimitCooldownPersistsAndReloads` supply repeated-call and restart
coverage: the 65,043-second provider delay is preserved, requests and token
work stop during the hold, and a fresh client loads the persisted deadline.
These tests are reused rather than duplicated. No provider cooldown is
shortened, ignored, or probed through.

## Validation and limitations

The 100,000-entry correctness fixture and all 30 benchmark combinations passed
in isolated runs. An initial benchmark overlapped repository-wide tests and
failed several 100,000-entry cases at the existing ten-second metadata deadline;
the final benchmark passed after that competing workload ended. The ordinary
correctness and race suites intentionally skip the largest case. The fresh
build, credential-free repository-wide Go suite, scoped race/vet/lint, Markdown,
relative links, and diff checks passed; see [VALIDATION.md](VALIDATION.md).

No live API, encrypted mounted-scale test, quota probe, data reset, or runtime
installation was performed. There is no claim that these synthetic timings
predict Degoo latency. The correctness fixture checks complete pagination;
repeat/duplicate cursor failures remain covered by the existing API tests.

## Remaining decision evidence

P1-FIXTURES and P1-LOOKUP remain in progress. Next steps are peak retained
metadata/index measurement; paginated mutation and encrypted chunk-stack
traces; direct-ID failure/identity checks; and bounded local-index and remote
shard prototypes with publication/rebuild counters. P1-RATELIMIT's controlled
mock cases are complete. Live quota scope and numeric thresholds remain
unknown. P1-DECISION must still compare all three alternatives and record the
format, writer/conflict policy, exposure, and reset consequences before P2/P3
format implementation.

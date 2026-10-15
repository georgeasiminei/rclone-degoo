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

No persistent path-to-ID index exists in this layout. The follow-up below
measures post-GC retained heap and snapshot/hint occupancy. Peak metadata
memory, memory with many populated parent snapshots, and persistent-index
size for candidate designs remain unmeasured. Allocation counts cannot close
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
reads during `NewObject`. That first fixture isolates chunk discovery. The
follow-up below adds ranged GET support and traces the supported crypt layer;
a mounted large logical file remains outside these generated tests.

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

P1-FIXTURES and P1-LOOKUP remain in progress. The follow-up below supplies
retained-heap and occupancy measurements, paginated mutations, encrypted-stack
traces, and direct-ID identity/error regressions. Peak metadata footprint,
multiple populated parent snapshots, and live direct-ID failure/stability
semantics remain unverified. Bounded local-index and remote-shard prototypes
still need publication/rebuild counters. P1-RATELIMIT's controlled mock cases
are complete. Live quota scope and numeric thresholds remain unknown.
P1-DECISION must compare all three alternatives and record the format,
writer/conflict policy, exposure, and reset consequences before P2/P3 format
implementation.

## P1 follow-up: bounded parent-hint eviction and identity checks

The parent-hint table retains at most 4,096 entries. An ordered list replaces
its per-insertion search for the oldest sequence. Remembering an existing ID
updates its parent and moves it to the newest position; forgetting a hint
removes its order entry, and a global clear discards both structures. Lookup
reads do not refresh recency. The generation/epoch invalidation rules remain
in effect. Each insertion or refresh has constant-time map/list work rather
than a walk over all retained hints. The list adds bounded per-hint memory.

Reproduce the focused comparison:

```sh
RCLONE_CONFIG=/notfound go test ./backend/degoo/api -run '^$' \
  -bench '^BenchmarkParentHintInsertion$' -benchtime=200ms -count=1
```

On Linux amd64 / Go 1.26.0, cycling through 4,097 generated IDs with the table
full measured 25,569 ns/op before and 202.4 ns/op after, in single local runs.
The ordered implementation measured about 63 B/op and one allocation/op in
that workload; the scan implementation measured zero. The unsaturated 65-ID
refresh control measured 45.00 versus 48.84 ns/op and zero allocations. These are
local microbenchmarks, not provider latency claims. Tests cover eviction,
refresh, removal/reinsertion, global clear, and stale mutation epochs.

Direct opaque-ID reads reject an empty or mismatched response ID before
returning metadata or recording its parent hint. Both regression cases failed
before the check was added. Null overlays and HTTP 403 responses return API
errors with one request and no fallback listing. Existing unauthorized-refresh
cases retain their coverage. Parent/name/size matching remains the caller's
responsibility because `GetItem` receives only the opaque ID. These generated
responses establish client behavior, not live ID stability, provider deletion
semantics, or concurrency guarantees.

## P1 follow-up: retained metadata and occupancy

Reproduce the memory and occupancy measurements in isolation:

```sh
RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 \
  go test ./backend/degoo -run '^TestScalabilityRetainedMetadataMemory$' \
  -count=1 -v
RCLONE_CONFIG=/notfound go test ./backend/degoo/api \
  -run '^TestScalabilityMetadataOccupancyBounds$' -count=1 -v
```

The memory fixture establishes a localhost connection before the baseline,
collects garbage, lists the generated parent, drops caller-owned result
slices, performs a warm read, and collects garbage again while keeping the
client reachable. It samples process heap, including fixture/HTTP/runtime
retention; it does not isolate exact index bytes. Cumulative allocation counts
cover only the cold listing. The API call uses its shared 30-second flight
ceiling; the backend's enclosing ten-second lookup deadline is tested by the
separate `TestScalabilityDirectory` fixture.

| Children | Post-GC retained heap delta | Cumulative cold-list allocation | Pages / requests | JSON response bytes |
| ---: | ---: | ---: | ---: | ---: |
| 1,000 | 281,720 B | 5,329,720 B | 1 / 1 | 166,056 |
| 10,000 | 2,451,760 B | 52,619,120 B | 10 / 10 | 1,660,596 |
| 100,000 | 19,012,888 B | 508,246,264 B | 100 / 100 | 16,606,086 |

These single Linux amd64 / Go 1.26.0 samples follow the ordered eviction fix.
Every warm read had one snapshot hit and zero requests. The retained delta is
neither a process peak nor a strict byte ceiling; no peak was measured.
Snapshots still hold entire child arrays. A cap on parent count does not
bound total child records or heap independently of directory size.

The API occupancy fixture requests 40 generated empty parents and retains 32
snapshots, then observes 32 warm hits with zero requests. Accessing the oldest
evicted parent makes one request and keeps occupancy at 32. A separate
5,000-child paginated listing retains exactly 4,096 parent hints and 4,096
order nodes; the oldest hint is evicted and the newest remains. This verifies
entry-count bounds and eviction without claiming a total byte bound. Memory
for 32 populated snapshots and peak local-index footprint remain unmeasured;
P1-FIXTURES stays in progress.

## P1 follow-up: paginated mutations and encrypted stack

Reproduce the generated lifecycle tests:

```sh
RCLONE_CONFIG=/notfound go test ./backend/degoo \
  -run '^TestScalability(PaginatedMutations|EncryptedChunkerTrace)$' \
  -count=1 -v
```

The shared mock can return complete 1,000-entry pages and HTTP 206 byte ranges.
The raw backend lifecycle uses 1,005 unrelated generated siblings; reads and
mutation confirmation must cross two pages per scan. Cold lookup and refresh
each use two requests, and warm lookup uses none. Generated writes are read
back; rename/delete check that the prior path is absent.

| Operation | Listing-page requests | Mutation / parent / global invalidations |
| --- | ---: | ---: |
| Create | 6 | 2 / 8 / 0 |
| Overwrite | 8 | 4 / 12 / 0 |
| Rename, source resolved | 2 | 1 / 2 / 0 |
| Delete, source resolved | 2 | 1 / 4 / 0 |

The encrypted lifecycle uses real chunker → crypt → Degoo with an explicit
`128B` plaintext chunk size. Its 624-byte generated file becomes five encrypted
data chunks and one encrypted manifest. Checks reject plaintext path/chunk-name
fragments, JSON metadata, and repeated plaintext payload fragments in remote
objects. Full reads and a range crossing chunk boundaries decrypt to the
expected bytes. This fixture has a small remote parent; it does not combine
the encrypted stack with the large-directory fixture or a mounted VFS.

| Operation | Listing pages | Direct metadata reads | Payload reads | Snapshot hits / misses | Mutation / parent / global invalidations |
| --- | ---: | ---: | ---: | ---: | ---: |
| Create | 23 | 12 | 6 | 19 / 11 | 17 / 58 / 0 |
| Cold lookup | 1 | 0 | 0 | 1 / 1 | 0 / 0 / 0 |
| Warm lookup | 0 | 0 | 0 | 2 / 0 | 0 / 0 / 0 |
| Cold full open | 0 | 0 | 6 | 0 / 0 | 0 / 0 / 0 |
| Explicit range open | 0 | 0 | 4 | 0 / 0 | 0 / 0 / 0 |
| Warm full open | 0 | 0 | 6 | 0 / 0 | 0 / 0 / 0 |
| Refresh and lookup | 1 | 0 | 0 | 1 / 1 | 0 / 0 / 0 |
| Overwrite | 33 | 12 | 7 | 22 / 16 | 24 / 82 / 0 |
| Rename | 8 | 2 | 1 | 13 / 6 | 10 / 24 / 0 |
| Delete | 11 | 0 | 0 | 1 / 5 | 6 / 24 / 0 |

These counters exclude constructor/root validation. Mutation payload reads
include verification. Cold full open includes five ranged downloads and one
manifest read; the explicit range requests four encrypted payload ranges.
The persistent small-object cache is not enabled in this fixture, so warm open
still downloads content. The test verifies complete readback after overwrite
and after rename, using the object returned directly by `Move`.

That last assertion exposed a backend contract defect: `Object.Update`
discarded the new published object's ID, URL, size, and timestamps. Chunker
updates its renamed manifest in place, so its returned object then held a
deleted manifest ID. The receiver now adopts the confirmed published metadata
only after success. A direct regression failed before the fix and now passes
two successive updates through the same object; a corrupt-upload failure
preserves the original readable receiver. Reads and mutations pin a synchronized
metadata snapshot before waiting for path serialization. A remove begun during
an update still targets the version it observed, preserving the existing
stale-handle protection. The existing concurrent update/remove regression
passes under the race detector. Move rechecks source ID and parent after
acquiring its path locks, rejecting a replaced source before any move/rename
mutation. The stale-source regression failed before that check and now leaves
the replacement readable and destination absent, with zero mutation requests.
The normal warm path adds a source snapshot hit without additional listing
pages. No generic chunker change is needed.

The post-fix 100,000-entry backend correctness run passed under the existing
ten-second metadata deadline. Cold full list measured 1.15 seconds, compared
with 5.69 seconds in the first baseline. It still used 100 requests and
16,606,086 JSON bytes. These single samples demonstrate the removed local CPU
cost, not a bounded metadata hot path or a provider latency guarantee.

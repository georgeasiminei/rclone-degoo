# Scalable storage baseline — 2026-10-15

This is the first P1 implementation slice from
[SCALABLE_STORAGE_PLAN.md](SCALABLE_STORAGE_PLAN.md). It measures the current
layout and adds failure regressions. The initial slices do not select or
implement a new remote format. The later decision is conditional and adds no
production format. All provider responses and object names are generated
locally; no Degoo account, installed mount, pending cache, or remote vault is
used.

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

No production persistent path-to-ID index exists in this layout. The follow-up
below
measures post-GC retained heap and snapshot/hint occupancy. At that stage, peak
metadata memory, populated-parent memory, and candidate
persistent-index sizes were unmeasured. The later candidate and memory
sections close the generated-fixture gap; allocation counts alone cannot do so.

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

P1-FIXTURES is complete for generated evidence. The follow-ups below supply
retained/sampled-peak heap, populated-snapshot occupancy, paginated and
encrypted-stack traces, direct-ID regressions, and executable bounded
persistent-ID/encrypted-node candidates. P1-LOOKUP remains in progress for live
ID stability, deletion, and failure semantics. P1-RATELIMIT's controlled mock
cases are complete; live quota scope and numeric thresholds remain unknown.
P1-DECISION must settle provider publication/root discovery, physical placement,
writer/conflict policy, retention/cleanup, stable directory IDs, and the format
and reset consequences before P2/P3 format implementation. Test-model counters
cannot establish provider guarantees.

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
refresh control measured 45.00 versus 48.84 ns/op and zero allocations. These
are
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
entry-count bounds and eviction without claiming a total byte bound. At that
stage, memory for 32 populated snapshots and sampled peak
footprint remained unmeasured. The later memory section supplies those
measurements and closes P1-FIXTURES for generated data.

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

## P1 candidate prototypes: persistent IDs and encrypted radix pages

Delivery: [PR #46](https://github.com/georgeasiminei/rclone-degoo/pull/46),
targeting `staging` and merged on 2026-10-15.

These are executable test-only experiments, not a new mount format. The
persistent-ID prototype uses the actual generated GraphQL client. The remote
index prototype uses a local encrypted object map with direct ID addressing;
its counters are model object operations, not HTTP requests or measured Degoo
behavior. Neither prototype is enabled by configuration or installed helpers.

Reproduce the encrypted model, including the largest generated case:

```sh
RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 \
  go test ./backend/degoo \
  -run
'^TestScalability(EncryptedShardModel|ShardModelSplit|ShardModelFailure)$' \
  -count=1 -v
```

The remote candidate uses HMAC-SHA256 logical keys, a radix of 16, at most
64 records per leaf, at most 16 references per branch, and a 128 KiB encrypted
node ceiling. AES-GCM encrypts the complete node, including names, keys, data
IDs, and child references, with the model object ID authenticated as associated
data. This is a generated-key experiment; production KDF, key separation,
format headers, version/parent authentication, and rollback protection remain
P3-FORMAT work. Its ID allocator is synthetic. Real opaque object names,
placement, and provider-ID allocation are not modeled.

The model bulk-builds the index once. Cold point access starts from a trusted
root handle with no local metadata. An overwrite copies the leaf and its
ancestors, leaving the previous root readable. Rename stages removal and
insertion and keeps the data ID stable. A full rebuild starts with an empty
result map and reads every reachable encrypted node, validating every recovered
name-to-key mapping and the expected records. Rebuild reads exclude unreachable
old or staged nodes. Root bootstrap and authentication are excluded.

| Logical records | Initial nodes / encrypted bytes | Cold point reads / bytes | Overwrite reads / writes | Rename reads / writes | Full rebuild reads |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 126 / 152,182 | 2 / 8,734 | 2 / 2 | 4 / 4 | 126 |
| 10,000 | 273 / 1,442,089 | 3 / 7,613 | 3 / 3 | 6 / 6 | 273 |
| 100,000 | 4,369 / 14,553,865 | 4 / 4,220 | 4 / 4 | 8 / 8 | 4,369 |

The maximum encrypted node in these three runs was 9,145 bytes. The
1,000-record case splits some leaves at the threshold, so occupancy and object
counts are not proportional to record count between these particular samples.
Missing point lookup required 2/3/4 reads. The largest model lookup did not
scan 100,000 siblings or rebuild the complete index. These are deterministic
fixture counts; they establish no network latency or provider quota.

A 64-to-65-record split used one read and 17 new objects totaling 9,545
encrypted
bytes for the generated key distribution. Failure is injected before every
one of those writes and each ancestor write of a separate overwrite. The old
root stays readable, and the number of unreachable successful staged writes
is asserted. Tampered ciphertext, cross-ID ciphertext substitution, a missing
node, an oversized record, and an overfull fully colliding hash path fail.
These checks exercise local model ordering and authentication only.

### Comparison and decision limits

| Alternative | Evidence | Remaining limitation |
| --- | --- | --- |
| Smaller listing pages | Current layout needs 100 versus 1,000 calls at 100,000 siblings for ceilings of 1,000 versus 100 | Every cold point lookup still scans siblings; no live rate-limit improvement demonstrated |
| Bounded persistent local IDs | Actual temporary files and generated direct-ID requests; see the local-index measurements below | Cache misses and refresh still need complete current-layout enumeration; cached IDs require freshness and identity checks |
| Encrypted bounded radix pages | Cold point reads of 2/3/4 nodes and ancestor-only overwrite in this model | Trusted root discovery, remote atomic publication, data chunk/version records, physical bucket placement, cleanup, and concurrency remain unimplemented |

The model supports investigating encrypted remote shards further: it bounds
each metadata read and avoids a mandatory full rebuild on cold access. It
also adds remote objects and write fan-out, and its bounded nodes do not bound
physical provider-directory populations. Adaptive physical placement must
bound all index, data, temporary, and marker directories separately. A plain
map of all model objects is deliberately not evidence for P3-SHARDS.

The root assignment is an in-process atomic operation assumed by the model.
There is no tested Degoo compare-and-swap, atomic pointer replacement,
cross-client lock, or publication marker protocol behind it. A future design
must establish publication and conflict semantics using available provider
operations, define a single-writer fencing policy if required, and handle
ambiguous registration and root-update responses before exposing a complete
version. A local process lock alone cannot exclude a second machine.

The prototype retains old/staged objects. One interrupted attempt has bounded
fan-out in these tests; repeated retries and historical roots grow the store
without bound. A transaction inventory, retry identity, retention limit,
split headroom under skewed keys, and remote garbage collection remain required.
No claim of bounded orphan accumulation or crash-safe remote publication follows
from these tests. No chunk data is uploaded or validated by the index model.
Its simplified
records contain only name, keyed lookup digest, and data ID; file sizes, times,
version/chunk references, and transaction records are absent. Its byte totals
cannot be compared as equivalent metadata coverage against full API listings.

Names and index contents are encrypted. Object sizes/counts, access timing,
and node traversal patterns remain observable; bucket topology could also be
visible in a production placement scheme. Stable logical directory IDs are
still to be evaluated so directory rename can change metadata without moving
all descendant data. No production encryption format, reset, reseed, or
migration policy is selected here. P1-DECISION remains open pending provider
identity/publication evidence and a complete protocol comparison.

### Sampled peak and populated-snapshot memory

Reproduce in isolation, since concurrent heavy validation changes timing:

```sh
RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 \
  go test ./backend/degoo -run '^TestScalabilityRetainedMetadataMemory$' \
  -count=1 -v
RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 \
  go test ./backend/degoo/api -run
'^TestScalabilityPopulatedMetadataSnapshots$' \
  -count=1 -v
```

The cold-list sampler requests a process `HeapAlloc` sample every 500
microseconds, including samples before and after the listing. Scheduling and
`ReadMemStats` overhead affect the measurement. Observed maxima are lower
bounds on a true instantaneous peak, not RSS or cache-only measurements. The
fixture reports sample count, baseline, cumulative allocation, and retained
heap separately. The post-GC measurements keep the client reachable and discard
caller-owned result slices.

| Children per parent | Sampled peak process heap | Post-GC process heap delta | Cold pages |
| ---: | ---: | ---: | ---: |
| 1,000 | about 4.00 MB | +0.25 MB | 1 |
| 10,000 | about 9.94 MB | +2.44 MB | 10 |
| 100,000 | about 66.98 MB | +19.01 MB | 100 |

The populated-snapshot fixture uses distinct generated IDs per parent, so
parent hints do not alias across snapshots. It asserts every snapshot's child
count, exact pagination counters, warm reuse, and the 32-parent count limit.
The 33-parent case also re-fetches an evicted populated snapshot. Production
cache expiry and operation deadlines are preserved.

| Children per parent | Requested / retained parents | Retained child records | Logical snapshot payload | Post-GC process heap delta |
| ---: | ---: | ---: | ---: | ---: |
| 1,000 | 33 / 32 | 32,000 | 7.55 MB | +9.32 MB |
| 10,000 | 3 / 3 | 30,000 | 7.08 MB | +8.37 MB |
| 100,000 | 2 / 2 | 200,000 | 47.2 MB | +51.89 MB |

MB here is decimal and rounded. Logical payload sums slice capacity times
`sizeof(Item)` and the lengths of ID/name/parent/URL strings. It excludes
allocator rounding, maps, hint/order nodes, and possible shared string backing;
it must not be presented as exact allocated bytes. Process deltas include the
HTTP fixture, transport, and test bookkeeping. The largest populated case made
200 initial page requests and cumulatively allocated approximately 1.02 GB;
that allocation count is neither a peak nor a retained amount.

These measurements close the generated P1 memory-fixture gap. They demonstrate
that a parent-count limit alone does not bound memory by bytes or child count.
A production byte limit and selected-format index footprint remain P2-BOUNDS
and P3 work. Results establish no live provider behavior.

### Bounded persistent local-ID cache measurements

```sh
RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 \
  go test ./backend/degoo -run '^TestScalabilityPersistentLocalIndex' \
  -count=1 -v
```

The test-only cache writes generated parent/name/ID records as JSONL beneath a
private temporary directory, with a 512-entry and 16 KiB file-content bound.
Byte eviction retains the last 192 fixture records in a 16,348-byte file at
all three scales. It stores no download URL. Restart reparses that bounded
file into memory. A hit makes one real generated `GetOverlay4` request and
checks the returned ID, parent, and name; it does not scan siblings. The
bounded cache rewrites its file on rebuild/invalidation, not on read hits.
The old disposable file is removed before writing its replacement so a
simultaneous old-plus-temporary file does not exceed the content-byte budget;
this does not claim crash durability, filesystem-block accounting, or production
cache path hardening.

| Siblings | Cold pages / response bytes | Cold rebuild | Restart hit pages / direct reads / response bytes |
| ---: | ---: | ---: | ---: |
| 1,000 | 1 / 166,056 | 13.7 ms | 0 / 1 / 190 |
| 10,000 | 10 / 1,660,596 | 126.0 ms | 0 / 1 / 190 |
| 100,000 | 100 / 16,606,086 | 1.159 s | 0 / 1 / 190 |

Restart hit duration was approximately 0.23–0.27 ms in these isolated samples.
Reopen parsing time is outside lookup timing. Cold rebuild includes the full
paginated listing and bounded file rewrite. Peak transient memory still
includes the backend's complete listing, even though the retained local ID
cache is small. A cache size bound necessarily evicts other paths: the hit
result cannot generalize to every path in an arbitrarily large vault.

Regressions independently force entry eviction and byte eviction, verify
mutation/refresh generation changes, and rebuild after missing
or corrupt records. Deterministic concurrent-list/direct-read cases ensure an
invalidation crossing an in-flight lookup returns a clear failure and cannot
repopulate or return stale state. This is an explicit prototype invalidation
hook, not production wiring to backend mutations. It invalidates the whole
prototype cache, not a narrowly scoped production namespace.

A direct-ID read cannot establish that a cached name is still the authoritative
winner if another client replaces it under a different provider ID. No remote
revision check or freshness lease is modeled. Direct-ID failures propagate;
they do not silently trigger a fallback sibling scan. Provider stability,
conflicts, ambiguous errors, and external mutation freshness remain decision
inputs. This alternative speeds retained keys after a restart while leaving
the current layout's O(N) cold enumeration and remote sibling growth intact.

### Completed-operation counter ordering

The final race run of the first attempt exposed an existing sampling race:
`ListDirFresh` signaled its shared flight complete before recording its final
parent-invalidation counter. A caller sampling immediately after delete could
therefore observe three events instead of four. A deterministic API regression
blocks post-completion cancellation; it failed before the fix with zero final
invalidation events recorded. The worker now records that event before closing
the completion channel, preserving exact operation-trace assertions. Cache
invalidation itself, request counts, and cooldown policy are unchanged. The
counter snapshot remains local-only.

## P1 lookup contracts and publication decision

Delivery: [#48](https://github.com/georgeasiminei/rclone-degoo/pull/48),
targeting `staging`.

The [decision record](SCALABILITY_DECISION.md) compares root publication,
writer fencing, physical placement, stable directory identity and cleanup.
The conditional target is immutable encrypted remote pages with stable logical
IDs and an authoritative conditional root boundary. Its provider primitive is
unverified; the current mount and wire format remain unchanged. P1-LOOKUP and
P1-DECISION retain their live evidence gates rather than treating a modeled
CAS or positive readback as a provider guarantee.

### Live availability and direct-ID failure contract

Read-only local inspection on 2026-10-15 found a persisted provider hold until
**2026-10-15 21:30:29 UTC**. No provider call, token renewal, login, runtime
change, or reset was attempted. The deadline was not shortened or removed.
A later session must re-read it because another client may extend the hold.
Live ID stability, continuity after rename/move/overwrite, deleted-ID response,
and root consistency/fencing remain unverified.

New GetItem fixtures use the real GraphQL client against generated transports:

| Case | Direct metadata HTTP attempts | Listings | Result |
| --- | ---: | ---: | --- |
| Cancellation / deadline in flight | 1 each | 0 | Wrapped context error; no returned item |
| HTTP 503 with Retry-After 2 | 1 | 0 | Retryable API error with preserved two-second delay |
| Malformed JSON | 1 | 0 | API error; no returned item |
| HTTP 401 then renewed token accepted | 2 | 0 | One refresh; exact requested ID returned |
| Repeated HTTP 401 | 2 | 0 | One refresh; non-retryable auth error; no loop |
| HTTP 429 with Retry-After 120 | 1 | 0 | Typed cooldown persisted without reducing deadline |
| Fresh client loading that cooldown | 0 | 0 | Same persisted deadline; zero token acquisition calls |

These complement existing null/403, response-ID mismatch and GraphQL auth
fixtures. Null is an API error, not a canonical not-found result. GetItem has
no parent/name/size expectation; an indexed caller must validate its expected
identity and authenticated version. One successful ID read does not establish
that a cached pathname is still the current namespace winner.

Reproduce the added direct-ID cases:

```sh
RCLONE_CONFIG=/notfound go test ./backend/degoo/api \
  -run '^TestGetItem' -count=1 -v
```

### Publication and placement experiments

Reproduce the new publication fixtures:

```sh
RCLONE_CONFIG=/notfound go test ./backend/degoo \
  -run '^TestScalabilityPublication' -count=1 -v
```

The small snapshot copier reuses the encrypted radix-node store but is a
separate root/placement experiment. It admits at most 64 records and a
128 KiB encrypted node; each snapshot writes an index node and a root node.
It checks record/byte limits before writes and caps staged state at eight
objects / 512 KiB. Known orphan inventory is capped at 16 IDs, and publication
stops when projected orphan count reaches the eight-object threshold. An
exhausted staging budget rejects repeated attempts before allocation. Unknown
publication or incomplete inventory stops further staging and cleanup.
These are explicit fixture parameters, not provider quotas or a final format.

| Experiment | Observed model result |
| --- | --- |
| Explicit failure before root apply | One attempted root write; previous encrypted root remains readable |
| Lost response before versus after root apply | Same client error and stale root view, different actual heads; neither permits cleanup or further staging |
| Unfenced writer after successful readback | Three root writes allow the paused writer to replace the newer root |
| Modeled authority with expected revision and epoch | Stale revision rejected; old and self-invented future epochs independently rejected; CAS alone accepts a stale owner with the latest revision |
| Rename a generated directory with one child | Stable directory, file and data IDs preserved; four encrypted-node writes and two reads across old/new snapshots |
| Six physical roles with 96 objects each | 576 objects; 8,650 directory records; maximum occupancy 16, counting files and child directories including root/control |
| Short physical route forced full | Next allocation rejected; no overflow directory or bucket |
| Seven known orphans; cleanup work budget three | Seven deletions over seven steps, 13 inspections; current/previous complete roots retained |

The 8,650-directory result exposes a substantial cost of the fixed-depth
16-digit placement experiment: it is not a recommendation for production
folder topology. Adaptive expansion and provider folder/placeholder overhead
must be measured before selecting P3 physical allocation. A forced short-route
collision fails closed; no hash-distribution claim substitutes for the cap.

The model's root has a fixed control slot and switches for conditional write,
authority-issued epochs, and strong reads. None is a verified Degoo primitive.
The root value is model memory, not a serialized authenticated provider control
record. Index reads validate AES-GCM through the earlier model; there is no
chunk upload or verification. Directory rename tests identity preservation,
not asymptotic update cost, cycle rejection, replacement or mounted handles.

Inventories are bounded in-memory bookkeeping collected after modeled writes.
There is no durable intent-before-network, ambiguous provider-ID discovery,
reader pin protocol beyond current/previous metadata roots, restart recovery, remote
inventory pagination, or recycle/hard-delete exercise. Cleanup sorts the
bounded queue locally; the per-step budget counts inventory inspections and
object deletions, not all CPU instructions or physical-directory removals.
The earlier 100,000-record radix experiment supplies lookup/update/rebuild
scale evidence; this snapshot copier does not rewrite that many records.
Allocation and cleanup assume a single actor; they do not enforce writer epochs.
Data IDs are symbolic; pins protect index/root pages only, not actual file
chunks. Missing physical identity/path data stops cleanup and retains its
reservation. Production publication, inventory/GC and fencing remain P3/P4 gates.

An initial model run exposed an uninitialized placement-parent map; the
preflight was corrected before final combined validation. Integration review
also made the unapplied/applied lost-acknowledgement errors identical and added
admission refusal during uncertain publication or incomplete inventory. This
avoids claiming that distinguishable synthetic failures prove ambiguity, or
that a publication-only hold bounds further staging. No provider request,
selected-format production code, reset or installation is part of this work.

## Durable allocation and adaptive-placement follow-up

Delivery: [PR #49](https://github.com/georgeasiminei/rclone-degoo/pull/49),
targeting `staging`;
merging and source-branch deletion remain with the user.

This continuation starts from merged PR #48 at `773b9f8c3`. A local-only
cooldown check at 09:56 UTC on 2026-10-15, repeated at 10:00 UTC, still found
expiry at **21:30:29 UTC**. No Degoo API/authentication request, runtime update,
reset, cache deletion or pending-write discard is part of these experiments.
P1-LOOKUP and P1-DECISION retain their provider gates. See the
[reviewed contracts and live protocol](SCALABILITY_DECISION.md#reviewed-provider-contracts--pr-48-follow-up)
for the pinned public reference, negative finding's scope, and future bounded
continuity/deletion/fencing matrix. GetOverlay4 does not establish recycle state;
account-wide trash enumeration cannot fill that scoped evidence gap.

Reproduce the additional offline experiments:

```sh
RCLONE_CONFIG=/notfound go test ./backend/degoo \
  -run '^TestScalability(Durable|AdaptivePlacement)' -count=1 -v
```

### Allocation intent, safety state and disk reopen

The durable model uses real owner-private temporary files. It syncs pending
bytes separately and writes a bounded allocation-intent journal before a
simulated remote create/delete. Journal replacement syncs the new file,
renames it, and syncs the containing directory. A reopened store reconstructs
its high-water mark, unresolved intents, protected references and cleanup
cursor from the file. Missing/corrupt safety state fails closed rather than
following the disposable ID-cache prototype's rebuild/reset behavior.

The fixture parameters are eight non-reused physical slots, at most sixteen
combined object/intent entries, a 16 KiB journal and two candidate inspections
per cleanup step. These are experiment limits, not a production transaction
inventory or a provider cap. Exhaustion remains explicit even after an object
is removed: freeing occupancy does not recycle its allocation ordinal.

Create acknowledgements lost before or after apply retain the known slot and
transaction identity across reopen. The same transaction cannot issue a blind
replay. Discovery with unknown visibility retains the reservation; an explicit
simulated authoritative discovery can resolve it. Lost delete acknowledgements
retain the object and delete intent until authoritative absence is modeled.
Cleanup checkpoints persist a cursor, and reachability protects modeled file-data
IDs through current, previous and reader-pinned roots. An assumed epoch
authority atomically checks allocation and deletion, extending PR #48's
publication-only fencing experiment.

A failed checkpoint stops the current store instance until reopen, including a
failure after rename but before directory sync. A single fixed journal temp
path bounds abandoned replacement files to one old plus one temporary journal
(32 KiB of content); that is not filesystem-block or pending-data accounting.
An injected incomplete-graph state and dangling references refuse cleanup.
The eight failed-create attempts leave a 766-byte journal and eight reserved
slots; the ninth attempt sends no simulated create. The cleanup fixture retains
all six protected root/data IDs while reconciling two orphan deletions and
finishing its eight-entry cursor across two-entry steps. A 4,096-byte pending
local file survives reopen. Stale-epoch create/delete attempts send zero
simulated side effects.

Graph completeness/freshness is still supplied by the fixture, not established
by authenticated provider reads. Cross-store concurrent local journal mutation
is outside this single-coordinator experiment.

The cleanup sweep conservatively skips objects referenced by any inventory
record, including unreachable parents. Child-before-parent order can therefore
retain an orphan after the parent is removed. A lost delete response with the
object still present also remains held without automatic replay. These are
explicit liveness limits: production GC needs repeatable inventory generations,
progress scheduling and an authorized reconciliation/retry protocol. The finite
non-reused slot limit eventually stops writes; this model does not claim that
cleanup restores lifetime allocation capacity or account bytes.

This proves local model behavior only. The simulated remote map survives store
reconstruction in the same process; no host crash, torn filesystem, actual
provider-ID discovery, remote inventory pagination, authenticated chunk tree,
reader-pin registration/expiry or real provider fence is exercised. The test
journal is local safety state, not the rebuildable metadata cache. Losing it
cannot silently initialize an empty vault, and the experiment supplies no
remote-only recovery path after its loss. Staging large logical files still
requires paged remote intents and a bounded control/bootstrap protocol before
P3 implementation.

### Adaptive physical placement and cleanup cost

The append-only model fills bounded direct-object slots and lazily creates
binary routing children as needed, retaining stable locations for existing
objects. It counts child folders, objects, pending creates, known ghost slots
and reserved headroom. Allocation stops when visibility is unknown or the
finite routing depth is exhausted; repeated versions consume physical slots.
It does not rebalance/move existing immutable data to split a full container.

The six-role, 576-object comparison uses the same generated IDs as PR #48's
fixed-depth placement. Adaptive limits are fanout sixteen, two reserved slots
per directory, twelve direct objects and depth sixteen. These are exploratory
parameters, not selected format constants or live quota evidence.

| Generated comparison | Adaptive | Fixed-depth PR #48 model |
| --- | ---: | ---: |
| Objects, six roles | 576 | 576 |
| Physical directory records, including vault/role roots | 74 | 8,650 |
| Reserved slots | 148 | Not modeled separately |
| Maximum occupancy including adaptive reserve | 16 | 16 |
| Observed routing depth | 3 | Fixed maximum sixteen-digit route |
| Serialized placement checkpoint | 48,363 B | Not measured |

This trades the fixed model's sparse deep directory tree for higher occupancy
and a binary spill path with explicit twelve-object and headroom limits. It
does not use the same branching policy or provide a general optimality result.
Repeated-version, forced-skew and finite-depth exhaustion checks retain the
bounds. Cleanup counts candidate inspection, object deletion and empty-directory
removal separately; it preflights the full bounded ancestor chain. Insufficient
work budget refuses mutation, and a deep three-directory prune needs five work
units (one inspection, one object and three directories). This CPU preflight is
bounded by route depth but is not included in the mutation-work counter.

Structural save/reopen checks serialize the entire finite placement map after
injected boundaries. They check reconstructed reservations and stable paths;
they do not prove intent-before-network ordering. That ordering is exercised
separately by the durable journal model. The serializer is not a scalable,
paged allocator checkpoint, byte-bounded production record or remote authority.
Pending folder states are resolved by an explicitly supplied outcome;
Degoo visibility, uniqueness, folder placeholders and deletion acknowledgements
remain unverified. Caller-selected generated IDs and in-memory folder paths are
not proof that provider-assigned locators can be durably discovered or recovered.

## Production snapshot admission and indexed warm lookup

Delivery: [PR #50](https://github.com/georgeasiminei/rclone-degoo/pull/50),
targeting `staging`; merging remains with the user. Implementation commit:
`be976e776`.

This P2 increment starts from merged PR #49 at `a0115f690`. It changes the
existing backend without introducing a remote format, provider operation or
runtime deployment. The full scalable-storage plan remains active. Cooldown
checks at 10:20 and 14:23 UTC on 2026-10-15 still found the shared-client hold
through 21:30:29 UTC. No provider/authentication request or data reset occurred.

Each API client now caps retained snapshots at 32 parents, 262,144 records and
64 MiB of accounted bytes. Byte accounting includes Item slice capacity,
ID/name/parent/URL string contents, retained parent keys, name-index slices and
a fixed logical charge per name-map entry. It is not exact Go allocator usage
or a process RSS ceiling. Parent hints separately retain at most 4,096 entries
and 1 MiB of ID/parent string content. Retained strings are copied so a small
substring cannot pin a large uncharged response buffer.

Admission evicts oldest snapshots until all budgets fit. An individual
oversized snapshot bypasses retention; every item is still returned to its
caller. Replacement, expiry, targeted mutation and global flush release the
same accounting. This is a local resource policy, not a Degoo quota. Full
listing results and in-flight responses still scale with directory size;
production cold point lookup requires the P3 remote index. The backend's
existing directory-ID cache and remembered-placeholder set are also not made
byte-bounded by this API change; P2-BOUNDS remains open for complete coverage.

Each admitted snapshot indexes exact names and sorted unique names for backup
prefix fallback. A warm lookup copies only matching records. Exact duplicates,
backup-only recovery and promoted-directory placeholder resolution keep their
existing backend behavior. A cold selector shares complete pagination/flight
coalescing with ListDir; it does not issue a guessed selective server query.
Fresh reads continue to bypass snapshots and paginate the complete directory.
Cache access still checks cancellation and the persisted provider cooldown.
The five-second TTL and metadata deadlines are unchanged.

Reproduce the warm correctness/allocation and comparable benchmark cases in
isolation from heavy validation:

```sh
RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 \
  go test ./backend/degoo -run '^TestLookupChildIndexedWarmAllocationBounds$' \
  -count=1 -v
RCLONE_CONFIG=/notfound go test ./backend/degoo -run '^$' \
  -bench '^BenchmarkScalabilityDirectory$/^entries=(1000|10000|100000)$/^page=1000$/^warm_lookup$' \
  -benchtime=1x -benchmem -count=1
```

| Siblings | Recorded initial warm NewObject allocation | Indexed warm NewObject allocation | New sample time | HTTP / pages |
| ---: | ---: | ---: | ---: | ---: |
| 1,000 | 139,872 B/op | 784 B/op | 8.8 microseconds | 0 / 0 |
| 10,000 | 1,368,672 B/op | 768 B/op | 13.5 microseconds | 0 / 0 |
| 100,000 | 13,607,520 B/op | 768 B/op | 13.0 microseconds | 0 / 0 |

These are single Linux amd64 / Go 1.26.0 samples of the same existing benchmark,
compared with its recorded first baseline. Each new operation records one
snapshot hit. A separate sixteen-iteration direct lookup fixture measures
561/560/560 bytes per operation at the three scales and asserts that warm
allocation does not grow with unrelated siblings. Timing is descriptive and
not a latency guarantee. The default correctness suite covers 1,000/10,000;
the 100,000-entry allocation case is opt-in and passed separately. Cold scans
still need 1/10/100 pages at these scales.

Generated tests cover byte and record eviction, oversized admission without
partial listing results, stale replacement removal, accounting release,
matching order and caller isolation, duplicate/backup/placeholder semantics,
cancellation, cooldown refusal, and mutation invalidation. The existing
populated-snapshot fixture additionally checks accounted byte/record/hint bounds
and reports them alongside heap samples. No mounted large-scale or live
provider guarantee follows from these results.

The final populated-snapshot run retained 32,000 / 30,000 / 200,000 item records
at the 1,000 / 10,000 / 100,000 per-parent scales. Accounted snapshot storage
was 10,373,376 / 9,720,504 / 64,800,336 bytes, each below 67,108,864 bytes;
parent-hint strings were 241,664 bytes in all three cases. Exact parent-count,
complete-pagination and zero-request warm assertions still pass. At 200,000
records, the whole-process post-GC heap delta was about +69.6 MB, compared with
the earlier +51.89 MB sample without name indexes. The index costs additional
retained memory to remove per-lookup sibling scans/copies; the new byte/record
admission prevents indefinite cache growth. Neither sample isolates cache heap
or establishes a strict process memory ceiling.

An additional regression reproduced lost backup recovery when the initial
selector narrowed fallback to one lowercase encoded target prefix. The existing
backup decoder accepts uppercase hexadecimal too. Fallback now selects the
reserved backup-name group and applies that decoder, preserving both accepted
encodings and original duplicate resolution. Exact-name hits remain indexed;
backup recovery work may grow with the number of backups, rather than all
ordinary siblings. The regression failed before the correction and passed
afterward. The comparable final benchmark above includes this correction.

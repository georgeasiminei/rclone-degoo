# Degoo scalable storage plan and progress

**Plan date:** 2026-10-15; **Planning status:** complete; **Implementation
status:** in progress (P1 memory evidence and index candidate prototypes).

This durable tracker records evidence, open decisions, validation gates, and
next steps for large directories and logical files above Degoo's reported
object limit. Requirements remain in [REQUIREMENTS.md](REQUIREMENTS.md); broader
history is in [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) and
[VALIDATION.md](VALIDATION.md).

## Scope and operating rules

- Linux mounted use remains the initial product target. Ordinary file and
  directory operations, transparent encryption, and recovery behavior remain
  requirements.
- Until explicit go-live, all current Degoo data and pending uploads are
  disposable test state; create only generated data for validation.
- The user authorized pre-go-live format/cache invalidation and test-data
  resets when needed. Before resetting, notify the user of affected cache and
  pending writes, remote scope, and `rsync` reseed impact. Default to the
  configured vault; expand to account-wide test data only if needed. This is
  advance notice under standing authorization, not a request for approval.
- There is no requirement to drain the pending queue to zero before a planned
  pre-go-live reset and no legacy-format migration requirement. Pending local
  writes must still survive ordinary restarts, outages, and authentication
  failures. An announced test reset is the exception.
- Do not run a reset as part of documentation or planning work. Do not include
  credentials, private paths, account IDs, file names, or other private vault
  inventory in this document.
- Start implementation from current `origin/staging` on a feature branch and
  target the fork's `staging`; the user merges and removes the source branch.
  Record each PR and its validation evidence below.

## Evidence and current understanding

The current supported mount composes VFS → chunker → crypt → Degoo. Chunker
data objects and manifests are siblings in a remote parent. The backend's
`NewObject` and chunker sibling discovery can require listing that full parent
to resolve an item. The bounded listing snapshot cache helps repeated and
concurrent calls but does not avoid a cold scan or make a point lookup
independent of directory size. Known mutations invalidate affected listing
snapshots; unknown parents still use conservative global invalidation.

The API currently requests up to 1,000 entries per listing page. A larger page
limit reduces round trips for a complete listing but does not make enumeration
correct beyond that page, solve repeated scans, or provide bounded point
lookups. Pagination must remain complete for every chosen design.

Rate-limit observations are inconclusive and must not be turned into a numeric
quota claim:

- A known-working Windows request also succeeded when replayed on Linux with a
  fresh token for the same account. Separate Linux browser-default/list
  requests failed. Minimal-header and HTTP/1 and HTTP/2 controls returned 200;
  no operating-system cause is established.
- For the affected folder, the first page sometimes returned 200 and a later
  page or repeated request returned 429. A small-folder control returned 200.
- The provider's quota scope is unknown. It may not be a fixed per-folder or
  per-request limit. There is no evidence for a reliable threshold of 100
  entries or requests.
- A single 429 persisted as a global cooldown can hold up unrelated metadata
  work. Keep honoring provider `Retry-After`; measure the scope and the
  conservative hold's effects before changing that policy.

The [2026-10-15 incident record](RATE_LIMIT_INVESTIGATION.md) preserves the
underlying live comparisons, including browser success before the client's
deadline, exact response/deadline samples, and service-worker 504 masking.
It separates confirmed local failure mechanisms from the unproven provider
cause. The incident snapshot is also recorded in [VALIDATION.md](VALIDATION.md).
These observations do not close P1-LOOKUP, P1-DECISION, or live recovery gates.

Limited evidence includes a generated 2 MiB file, range read and matching
hash, lifecycle operations, nested operations, and fresh-cache remount. Live
support above 20 GiB, pagination beyond 1,000 entries, quota scope, upload
recovery, and cold index rebuild remain unverified.

## Design goal and decision gate

Keep remote state authoritative and make local metadata a rebuildable cache.
A cold local cache must be able to reconstruct the mounted view from remote
encrypted state. A single giant encrypted index is not an acceptable
destination: it would make updates, recovery, and publication depend on one
unbounded object. Shard metadata so each read, update, and recovery unit has a
bounded size.

Normal cold-cache access should bootstrap a trusted remote root and traverse
bounded index pages for the requested path. A full index rebuild is a recovery
operation, not a prerequisite for every mount or first file open. Measure both
paths separately.

The leading candidate for P1 evaluation is **adaptive ciphertext buckets with
an encrypted, sharded remote index**. The index would map encrypted logical
keys to opaque provider IDs and versioned data/manifest records. Bucket or
shard sizing would adapt to observed entry counts and object limits. The hot
path would use the index and direct provider ID lookup where supported,
without fetching every sibling. The index must encrypt names and user metadata,
support bounded shard reads and writes, and rebuild from remote state after
local cache loss. This is a candidate, not an architecture decision.

P1 must compare it against at least these options:

1. **Smaller listing pages.** Measure rate-limit behavior and page count. This
   may lower response size but can increase request count; it does not remove
   an O(N) cold scan.
2. **A persistent local ID/index cache over the current chunker layout.**
   Measure warm lookup gains, mutation invalidation, storage bounds, and cold
   rebuild cost. It can accelerate known entries but cannot be the only source
   of truth.
3. **Adaptive encrypted buckets plus a sharded remote index.** Measure object
   count, index update fan-out, lookup cost, crash consistency, and rebuild
   cost. Prefer this candidate only if evidence shows it bounds the metadata
   hot path without compromising the requirements below.

The decision record must include alternatives rejected, measured counters,
provider assumptions, remote metadata visible to Degoo, format/version
consequences, and whether existing pre-go-live objects will be invalidated.
Update the broader implementation plan before implementation relies on a
material format or layering choice.

## Invariants for any design

- Encrypt file content before upload. Encrypt remote file names, index keys,
  manifest contents, and all metadata that can reveal user names or paths.
- Keep account authentication separate from vault encryption. Store no secret
  or plaintext user inventory in logs, test fixtures, or diagnostics.
- Keep each physical object below the currently verified provider cap after
  encryption headers, tags, manifests, and transport overhead. The reported
  20 GiB cap is not yet a verified guarantee. The existing 19 GiB safety
  setting and configured plaintext chunk sizes are provisional, not quota
  evidence.
- Support logical files larger than 20 GiB. Reassemble them transparently,
  validate every referenced part, and fail a read rather than return a
  truncated or mixed version.
- Publish a logical version as complete only after all referenced objects and
  encrypted metadata are validated. Keep the previous complete remote version
  readable where the chosen protocol permits it.
- Bound every physical directory, index shard, transaction record, temporary
  object set, backup, and publication marker. Reserve explicit split headroom
  for updates and crash recovery; a nominal 64-entry bucket is only a design
  starting point, not a provider quota or a committed limit.
- Use direct provider ID lookup for known objects where the API supports it.
  If the API cannot do this, document measured fallback behavior and keep
  bounded point-lookup guarantees explicit.
- Listing an entire logical directory with `ls` is inherently O(N) in the
  number of children. A stat/open/lookup of one known path must not require
  scanning all siblings once metadata is warm; cold rebuild work must be
  measured and recoverable.
- Do not ignore or shorten a provider `Retry-After`. Preserve local pending
  writes during throttling and auth outages. Do not turn a failed or partial
  listing into an empty directory.
- Keep index and transaction updates crash-consistent. A restart must expose a
  previous complete version or a clear failure, never a partially published
  file. Bound orphan discovery and cleanup; do not promise remote garbage
  collection until it is implemented and tested.
- Keep cache statistics and diagnostics local-only. Listing/index caches must
  be bounded and invalidated by relevant writes, renames, deletes, and remote
  refreshes.

## Phase gates and task tracker

Statuses are `complete`, `in progress`, `blocked`, `not started`, or `not needed`.
Keep IDs stable; `evidencePR` names the implementation/validation PR or is `TBD`.
Explain a skipped or superseded task and its replacement before marking it
`not needed`. Planning completion is not implementation evidence.

The agent taking a task owns its implementation and evidence; the coordinating
agent owns integration, PR delivery, and the handoff. Record the active owner,
date, and next action when work starts or stops. The user owns the go-live
decision. The coordinating Codex agent owns the P1 baseline work started on
2026-10-15; the exact remaining work is recorded below.

### P0 — durable plan

- **P0-DOC** — Status: `complete`; dependencies: none; evidencePR:
  [#42](https://github.com/georgeasiminei/rclone-degoo/pull/42). Create this
  durable plan and record that implementation has not started. The PR targets
  `staging` and was merged on 2026-10-15.

### P1 — reproduce, measure, and select the design

**Gate:** Reproducible counters and a written design comparison.

- **P1-FIXTURES** — Status: `complete`; dependencies: P0-DOC; evidencePR:
  [#43](https://github.com/georgeasiminei/rclone-degoo/pull/43),
  [#44](https://github.com/georgeasiminei/rclone-degoo/pull/44),
  [#46](https://github.com/georgeasiminei/rclone-degoo/pull/46). Add generated
  directory/object fixtures at increasing sizes,
  including 1,000, 10,000, and 100,000 entries. Record remote/API calls, page
  count, bytes, cache hits, cold and warm lookup latency, full-list latency,
  and peak local index size. Generated sampled-peak/multiple-parent memory
  and bounded persistent-ID/encrypted-node prototypes now supply these
  measurements; sampled heap peaks are lower bounds, not strict memory caps.
  Do not use private vault names or account data.
- **P1-RATELIMIT** — Status: `complete`; dependencies: P0-DOC; evidencePR:
  [#43](https://github.com/georgeasiminei/rclone-degoo/pull/43). Add controlled
  mock cases for first-page success followed by later
  page 429, repeated 429, unaffected small-folder success, cancellation, and
  persisted global cooldown. Preserve exact `Retry-After` behavior. Live
  probes, if needed, use generated test state and record date, client platform,
  operation, page, and response class without secrets or private IDs.
- **P1-LOOKUP** — Status: `in progress`; dependencies: P1-FIXTURES;
  evidencePR: [#43](https://github.com/georgeasiminei/rclone-degoo/pull/43),
  [#44](https://github.com/georgeasiminei/rclone-degoo/pull/44).
  Trace `NewObject`, chunk/manifest sibling discovery,
  create, overwrite, rename, and delete. Count listings and invalidations per
  operation. Verify direct provider lookup by opaque ID and establish its
  failure semantics.
- **P1-DECISION** — Status: `not started`; dependencies: P1-FIXTURES,
  P1-RATELIMIT, P1-LOOKUP; evidencePR: `TBD`. Compare smaller pages, a bounded
  persistent local ID cache, and adaptive ciphertext buckets with an encrypted
  sharded remote index. Record the selected design, measured tradeoffs,
  encryption boundaries, object-count bounds, index rebuild, publication
  protocol, writer/concurrency and conflict policy, and pre-go-live reset/reseed
  impact. Evaluate stable logical IDs so a directory rename need not rename or
  relocate every encrypted data object beneath it. Update
  IMPLEMENTATION_PLAN.md before coding the selected format.

### P2 — bound metadata hot paths

- **P2-HOTPATH** — Status: `not started`; dependencies: P1-DECISION;
  evidencePR: `TBD`. Implement the selected lookup path, including direct ID
  reads where available and narrowly scoped cache invalidation. Compare
  before/after API-call counters for stat, open, create, overwrite, rename,
  delete, and refresh. Existing-format lookup improvements may start here;
  integration using the new remote index also depends on P3-FORMAT and
  P3-SHARDS. Phase numbers do not require P2 to finish before P3 starts.
- **P2-BOUNDS** — Status: `not started`; dependencies: P2-HOTPATH;
  evidencePR: `TBD`. Bound cache entries and memory, verify complete paginated
  listing above 1,000 children, and assert that a known-path lookup does not
  grow linearly with sibling count after cache warmup. For the selected indexed
  format, also verify cold point lookup traverses bounded index pages without
  scanning the entire vault; this integration gate depends on P3-SHARDS.
  Preserve O(N) full directory listing behavior as an explicit cost.

### P3 — versioned encrypted layout and publication

**Gate:** Bounded objects/shards and crash-safe publication.

- **P3-FORMAT** — Status: `not started`; dependencies: P1-DECISION;
  evidencePR: `TBD`. Specify the versioned encrypted object/index/manifest
  schema, key derivation and authenticated fields, shard addressing, maximum
  record size, chunk sizing after encryption overhead, and how a local cache
  reconstructs from remote state. Do not add a single unbounded index.
- **P3-PUBLISH** — Status: `not started`; dependencies: P3-FORMAT;
  evidencePR: `TBD`. Implement staged upload and atomic logical-version
  publication. Define idempotency, write authorization, version selection,
  overwrite behavior, and bounded temporary/backup/marker counts. Keep the
  last complete version readable until the new version is confirmed where
  supported.
- **P3-SHARDS** — Status: `not started`; dependencies: P3-FORMAT;
  evidencePR: `TBD`. Implement bounded index shards/buckets and their update,
  split, merge, lookup, and invalidation rules. Prove that no single directory
  or index object grows without bound and that split headroom is sufficient
  for an interrupted update.
- **P3-REBUILD** — Status: `not started`; dependencies: P3-PUBLISH,
  P3-SHARDS; evidencePR: `TBD`. Rebuild local metadata from remote encrypted
  state after deleting or relocating the local index/cache. Verify names,
  file versions, directories, and chunk references without relying on a warm
  cache or plaintext sidecar.

### P4 — mounted recovery, auth, and cooldown semantics

- **P4-RECOVERY** — Status: `not started`; dependencies: P3-PUBLISH,
  P3-REBUILD; evidencePR: `TBD`. Exercise mounted create, overwrite, truncate,
  rename, delete, fsync, close, process crash, and restart. Assert pending
  local data survives normal outages and rejected authentication, and that an
  interrupted publication is either recovered or reported incomplete.
- **P4-FAULTS** — Status: `not started`; dependencies: P4-RECOVERY;
  evidencePR: `TBD`. Inject partial pages, 401/403, 429 with short and long
  `Retry-After`, timeouts, 5xx, process termination at every publication
  boundary, and stale indexes. No request may bypass an active provider
  deadline; throttled list errors must not look like empty directories.
- **P4-OPS** — Status: `not started`; dependencies: P4-RECOVERY;
  evidencePR: `TBD`. Verify status and statistics distinguish locally durable
  data, queued/uploading state, and confirmed remote publication without
  triggering network requests. Record cleanup limits for orphaned physical
  objects.

### P5 — optional announced pre-live reset and reseed

**Gate:** If selected, reset notice precedes any mutation.

- **P5-RESET** — Status: `not started`; dependencies: P3-FORMAT, P4-RECOVERY;
  evidencePR: `TBD`. Optionally reset generated test objects to exercise the
  selected format. Give advance notice of cache/pending-write effects, remote
  scope, and `rsync` reseed impact, then proceed under standing authorization.
  Default to the configured vault; expand to account-wide test data only if
  needed. Record the reset date, actual scope, non-secret vault label or
  fingerprint, resulting format version, and reseed/readback results here.
  Keep private provider IDs and local paths out of that record. Skip this phase
  if an isolated clean namespace suffices, and record why it was unnecessary.

### P6 — generated live acceptance and go-live gate

**Gate:** Generated live checks pass; go-live remains explicit.

- **P6-LIVE** — Status: `not started`; dependencies: P2-BOUNDS, P3-REBUILD,
  P4-FAULTS, P4-OPS; evidencePR: `TBD`. On Linux, use generated data only. Validate
  more than 1,000 entries and a representative large directory, cold rebuild,
  point lookup, create/read/update/rename/delete, encrypted names and metadata,
  and logical files above 20 GiB with verified per-object overhead. Capture
  hashes and aggregate request counts; keep private names and IDs out of
  reports.
- **P6-SOAK** — Status: `not started`; dependencies: P6-LIVE; evidencePR:
  `TBD`. Run a bounded mounted soak with restarts, a cold-cache remount,
  transient failures, 429 deadlines, and pending-write recovery. Report the
  exact tested scale and the provider behaviors still unknown. Inject timing
  and quota faults in controlled fixtures; do not deliberately exhaust a live
  quota or repeatedly probe through an observed provider cooldown.
- **P6-GOLIVE** — Status: `not started`; dependencies: P6-SOAK; evidencePR:
  `TBD`. Stop before designating any vault or data production. Record the
  user's explicit go-live statement and the final format/version, operational
  recovery procedure, verified object cap, and remaining limits.

## Cross-session resume and update checklist

For each future task:

1. Read `AGENTS.md`, `REQUIREMENTS.md`, this tracker, `IMPLEMENTATION_PLAN.md`,
   and relevant `VALIDATION.md` sections.
2. Check branch and `git status`; fetch `origin/staging` before branching.
   Do not overwrite another agent's work.
3. Pick the earliest unmet task with completed dependencies; update its stable
   ID, owner, date, status, evidencePR, and measured result. Leave an exact next
   action and any blocker before ending the session.
4. Give reset notice as described above. No queue drain or migration is
   required for an authorized pre-go-live test reset.
5. Record PR links and exact validation commands/results here and in the
   relevant handoff. Mark gates complete only when evidence exists.
6. Keep this file portable: omit chat transcripts, credentials, usernames,
   private paths, provider IDs, and private file listings.

## Dated handoff

### 2026-10-15 — plan established

- Planning is complete. No scalability implementation was started in this
  task.
- Planning delivery: [PR #42](https://github.com/georgeasiminei/rclone-degoo/pull/42).
  Markdown lint passed for all four changed documents, relative file links
  resolved, task IDs were unique, and `git diff --cached --check` passed.
  No source tests or live provider validation were needed for this
  documentation-only change. No runtime or data reset was performed.
- The principal known cost is full-parent listing for `NewObject` and chunker
  sibling discovery. A 1,000-item page limit is a batching choice, not a
  correctness boundary or established quota.
- The provider's selective 429 scope and any safe numeric threshold remain
  unknown. Do not infer a 100-entry limit from the observations.
- The encrypted sharded index with adaptive ciphertext buckets is the leading
  P1 candidate. P1 evidence and comparison must decide the design.
- First implementation work is P1-FIXTURES and P1-RATELIMIT. P1-LOOKUP follows
  P1-FIXTURES; P1-DECISION follows when all three measurements are available.
- Current Degoo data and pending uploads remain disposable tests until an
  explicit user go-live statement. Any intentional pre-live reset requires
  advance notice describing its concrete scope and `rsync` reseed impact.

### 2026-10-15 — P1 baseline implementation

- Owner: coordinating Codex agent, with delegated API-failure and lookup-trace
  test work. Delivery:
  [PR #43](https://github.com/georgeasiminei/rclone-degoo/pull/43), targeting
  `staging` and merged on 2026-10-15. No installed runtime or vault was
  reset or modified; all requests went to generated localhost fixtures.
- P1-FIXTURES now has generated 1,000-, 10,000-, and opt-in 100,000-entry
  correctness cases plus cold/warm lookup/list and direct-ID benchmarks.
  Requests, pages, JSON bytes, snapshot hits, latency, and allocations are
  measured. Cold lookup needs 1/10/100 requests at the three scales; warm
  lookup needs none but still copies/scans every sibling. Peak retained
  metadata/index memory remains unmeasured, so the task remains in progress.
- P1-RATELIMIT's controlled cases are complete: later-page 429, repeated
  cooldown access, unrelated cached-folder blocking, cancellation, and
  persisted restart hold. Existing long-delay/restart tests are reused.
  This establishes no live numeric quota or provider quota scope.
- P1-LOOKUP now traces backend create/overwrite/rename/delete invalidation,
  direct-ID success/null results, and real chunker manifest/sibling discovery
  over the mock backend. Large paginated mutations and the encrypted stack
  still need measurement before closing this task.
- The 100,000-entry correctness run passed in isolation. Concurrent heavy
  validation caused ten-second metadata-deadline failures in the initial
  benchmark; those failures are evidence of the current limit, not grounds
  to extend the production deadline. See
  [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) for counters, reproduction,
  measurement limits, operation traces, and next evidence gates.
- Validation passed: fresh Go build, the credential-free repository-wide Go
  suite with a short private temporary path and fresh binary on `PATH`, scoped
  race/vet/lint, the opt-in 100,000-entry correctness case, all 30 benchmark
  combinations, Markdown lint, relative links, and diff checks. An initial full
  suite failed existing Unix-socket tests due to a long temporary prefix; the
  short-path rerun passed. Details are in [VALIDATION.md](VALIDATION.md).
- Next action: finish retained-memory measurement and paginated/encrypted
  mutation traces, then compare bounded persistent local-index and sharded
  remote-index prototypes. P1-DECISION and all format/publication work remain
  unstarted. A local CPU cost also merits a focused follow-up: parent-hint
  eviction scans its full 4,096-entry table for every new item after saturation.

### 2026-10-15 — P1 memory and encrypted-operation follow-up

- Owner: coordinating Codex agent, with delegated retained-memory and
  encrypted-operation test work. Delivery:
  [PR #44](https://github.com/georgeasiminei/rclone-degoo/pull/44), targeting
  `staging` and merged on 2026-10-15. This slice starts from merged
  PR #43; no installed runtime, pending cache, or remote vault is modified.
- P1-FIXTURES now records post-GC process heap deltas at 1,000/10,000/100,000
  entries (about 0.28/2.45/19.0 MB), separately from cumulative allocation.
  Occupancy checks retain 32 snapshots after 40 empty parents and 4,096
  hints/order nodes after 5,000 children. Peak index memory and memory for
  multiple populated snapshots remain unmeasured; this task stays in progress.
- P1-LOOKUP now includes paginated create/overwrite/rename/delete with 1,005
  siblings, and real chunker → crypt → Degoo full/range reads, overwrite,
  rename, and delete. Direct-ID regressions reject empty/mismatched response
  identities; missing and forbidden responses use no fallback listing.
  Live opaque-ID stability/deletion/failure semantics remain unverified, so
  the task stays in progress. See [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md)
  for exact aggregate counters and the encrypted fixture's small tested scale.
- Parent-hint eviction uses constant-time ordered map/list updates instead of
  scanning 4,096 entries per insertion. The saturated insertion microbenchmark
  measured 25,569 versus 202.4 ns/op. The 100,000-entry backend correctness
  fixture passes under the existing deadline; cold full list measured 1.15 s
  versus the recorded 5.69 s baseline, with the same 100 page requests.
- The encrypted rename test exposed stale metadata on an updated object.
  `Object.Update` now adopts confirmed publication metadata on success.
  Operations pin synchronized version snapshots so a remove waiting on an
  update cannot delete the replacement. Repeated-update, failed-update,
  encrypted rename readback, and concurrent update/remove regressions pass.
  Move revalidates its pinned source after acquiring path locks; a stale
  source cannot issue move/rename mutations against a replaced object.
- Validation passed: fresh build, credential-free repository-wide Go tests,
  scoped race/vet/lint, 100,000-entry correctness and retained-memory fixtures,
  occupancy and encrypted/paginated lifecycle tests, Markdown/links, and diff
  checks. See [VALIDATION.md](VALIDATION.md) for commands and initial failures.
- Next action: measure peak metadata/index footprint and multiple populated
  snapshots; establish live direct-ID assumptions without probing through an
  active cooldown; compare bounded persistent local-index and encrypted
  remote-shard prototypes with lookup/update/publication/rebuild counters.
  P1-DECISION and P2–P6 remain unstarted. No new format or reset is selected.

### 2026-10-15 — P1 memory completion and executable index candidates

- Owner: coordinating Codex agent, with delegated memory and persistent-ID
  fixtures. Branch: `feat/degoo-scalability-prototypes`, based on merged PR #44.
  Delivery: [PR #46](https://github.com/georgeasiminei/rclone-degoo/pull/46),
  targeting `staging` and awaiting the user's merge. No installed runtime,
  cache, credentials, or remote test state was changed.
- P1-FIXTURES is complete for generated evidence: cold-list sampled heap peaks,
  populated snapshots, bounded persistent-ID disk occupancy, and encrypted
  radix-node lookup/update/rebuild counters are reproducible. Samples are
  qualified lower bounds; no strict production memory ceiling is established.
- At 100,000 siblings, two populated API snapshots retain 200,000 records,
  about 47.2 MB logical payload and a +51.89 MB post-GC process heap delta.
  The backend cold-list sample reaches about 66.98 MB process heap. The
  parent-count bound therefore remains insufficient as a byte bound.
- The test-only encrypted radix model uses 4 cold node reads and 4 copied
  nodes for an overwrite at 100,000 records; full rebuild reads 4,369 nodes.
  Encryption/integrity and interrupted split/overwrite checks pass locally.
  The assumed root swap and direct-ID map do not prove provider publication,
  fencing, root discovery, physical directory bounds, or orphan cleanup.
- The persistent-ID prototype provides an actual bounded on-disk cache with
  restart hits, direct-ID revalidation, eviction, invalidation, and recovery
  from missing/corrupt cache state. Cold misses still enumerate all siblings.
  See [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) for the comparison,
  measurements, assumptions, and exact reproduction commands.
- P1-LOOKUP stays in progress for live direct-ID stability, deletion, and
  failure semantics. P1-DECISION stays open; no production format is selected.
  Next action: establish provider direct-ID and publication/root-selection
  behavior with bounded generated-vault probes after checking the persisted
  cooldown, then complete the decision record. If live access is unavailable,
  continue offline root-publication and bounded physical-placement experiments
  without representing their assumptions as provider guarantees. Define writer
  fencing, recovery/cleanup limits, and stable directory IDs before P3.
- A deterministic regression reproduces an invalidation-counter ordering race:
  fresh-list completion woke callers before its final counter update. The
  event is recorded before completion, retaining exact trace assertions.
- Validation passed: fresh build, credential-free repository-wide Go suite,
  scoped race/vet/lint, isolated opt-in 100,000-entry candidate and memory
  fixtures, Markdown lint, 39 relative links, and staged diff checks. The
  initial race attempt exposed the counter-ordering regression above; the
  initial full-suite attempt required a shorter Unix-socket temporary path.
  Final build/full/race/vet/lint ran against restored sources in an isolated
  worktree after concurrent work changed the shared checkout. See
  [VALIDATION.md](VALIDATION.md) for exact commands and scope.
- No reset/reseed was necessary. P2–P6 remain unstarted. The user merges this
  PR and removes its source branch under the fork's workflow.

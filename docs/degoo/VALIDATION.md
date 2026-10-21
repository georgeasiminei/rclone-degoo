# Degoo validation status — 2026-10-15

Offline implementation checks are recorded separately from live acceptance.
Live smoke checks passed against a dedicated disposable Degoo folder. A full
2 MiB logical file has been published and verified after a fresh-cache
remount, but the remaining live gates mean this does not establish general
provider compatibility or complete release acceptance.
See [API_EVIDENCE.md](API_EVIDENCE.md) and the remaining checklist in
[USAGE.md](USAGE.md).

## Borg lock writeback and cloud maintenance — 2026-10-21

The latest scheduled Google Drive backup completed after account
reauthorization. Degoo created its archive and completed pruning, then compact
failed deleting an obsolete segment while `GetFileChildren5` was rate limited.
The client held requests for about one hour; uploads later drained. Borg's
missing-segment compaction warnings did not establish repository integrity.

A generated local-Fs regression deterministically reproduced a zero-byte lock
marker upload landing after fallback directory move's source snapshot. It
failed with `RenameDir rmdir: directory not empty` before the fix. With a
writeback prefix pause, it passes through rename, marker upload, unlink and
directory removal without late resurrection. Additional tests cover an active
upload's VFS callback, unrelated uploads, nested pauses, renamed queue items,
context cancellation and pending-write preservation.

A second generated regression held a remote directory listing open while
requesting local runtime statistics. It failed before statistics stopped
walking VFS directory metadata and passes afterward. This reproduces the
monitoring delay observed during live repository inspection without requiring
provider credentials.

A subsequent live archive check hung after a parallel stream failed to open
while Degoo was throttling. Its producer had exited, but the consumer continued
waiting for bytes. An offline Open-429 regression reproduced the hang before
the fix. Open errors, partial buffered data followed by an error, short EOF,
and cancellation/close ordering pass afterward, including scoped race tests
for the chunked reader and pool. The outer reader also preserves stream errors
instead of shadowing and dropping them.

Offline validation passed:

- `go build` and all 195 packages in the credential-free full Go test suite.
- Scoped VFS/cache/writeback tests and race tests, plus the runtime-statistics
  regression under the race detector.
- `go vet` for VFS and Degoo commands; golangci-lint 2.15.0 for VFS, Degoo
  commands and the Degoo backend reported zero issues.
- The native Linux FUSE directory-removal regression, retaining dirty-file
  cancellation and hidden-orphan cleanup behavior.
- All 515 Python helper tests, package self-check and Markdown/diff checks.

The separate backup-application patch passed 35 mocked script/cache-waiter
tests. A read-only scan confirmed that its older rclone cache uses `0644`
metadata files inside owner-private directories; the waiter accepts this
layout while rejecting writable, replaced or changing records. The real cache
wait succeeded with zero dirty files. No cache permissions were changed.

Live Google Drive validation passed a latest-archive reference check and
restored a 661,174,206-byte compressed database dump. SHA-256 matched the
source and gzip validation passed. The restored temporary copy was removed.
This is a representative restore through the mount, not full repository CRC or
cryptographic file-content verification. Degoo reference checking and patched
runtime deployment remain in progress; no repair mode or format reset has
been used.

## Directory removal after interrupted uploads — 2026-10-19

A live `rmdir` reproduced `Input/output error`, with the mount recording
`Dir.Remove failed to remove directory: directory not empty`. Four generated
create/delete/rmdir attempts succeeded while uploads were running, so deleting
a dirty file alone was not a reproducer. A bounded read-only raw inspection
of the affected directory found a category-0 directory marker, a 32-byte
encrypted orphan chunk, and a 143-byte unpublished staged object. The folder
setup helper omitted all three category-0 records. A fresh query of the
zero-byte internal marker's children succeeded and returned an empty list.

The generated encrypted-stack regression reproduces this exact shape without
credentials. It failed before chunker cleanup was implemented and passes with
both cleanup layers enabled. Raw-backend fixtures separately reproduced
orphan-marker and abandoned-stage failures before their fixes. Tests cover
normal clean recursive deletion, dirty queued cancellation across remount,
visible-content refusal, active-upload exclusion, mutation barriers, malformed
internal names, nested data below an internal placeholder, delete failures,
listing failures, and unchanged standalone chunker defaults.

Offline validation passed:

- `go build` with a fresh executable on `PATH` for subprocess tests.
- `RCLONE_CONFIG=/notfound go test -timeout 20m ./...`: all 195 tested packages.
- Scoped package tests and `go test -race` for chunker, Degoo, its API/auth
  and internal packages, and `cmd/degoo`.
- `go vet` and golangci-lint on those packages; lint reported zero issues.
- `go test ./cmd/degoo -run '^TestVaultNativeDirectoryRemoval$'
  -degoo-fuse-test -count=1`: real local FUSE deletion of queued dirty data,
  nonempty-directory refusal, and cleanup of a hidden encrypted orphan chunk.
- Packaging self-check, Markdown lint and `git diff --check`.

Initial scoped and race command tests hit the existing Unix-socket path limit
when `TMPDIR` included the validation directory name. The failing socket tests
passed with `TMPDIR=/tmp`; the full suite and final command race run used that
normal short temporary path. No storage format, encryption setting or cache
identity changed, and no reset was performed.

The candidate was packaged from commit `d44a3d744` and transferred to the
affected host for the ordinary contained updater. Its initial preflight
refused an unsafe generated Python bytecode cache; only owned regular `.pyc`
files and that generated directory were removed before retry. The retry
verified the package and flushed cached writes, but normal FUSE unmount was
blocked by a user's Bash working directory inside the mount. The update
stopped without replacing the running program. The mounted service remained
active with four uploads and all 335 pending files retained at that check.
Vault configuration, cache location and cache identity hashes were unchanged.
Live verification of the patched mount remains pending until that terminal
leaves the mount and the update can be retried; no forced or lazy unmount was
attempted. This is a deployment prerequisite, not a failed deletion regression.

## Uninstall with a missing unused cache directory — 2026-10-19

An installation using an external `cache_dir` failed before the uninstall
wizard's first prompt because layout validation required the unused contained
`cache/` directory. Layout validation now resolves the configured path first
and permits only the unused contained directory to be absent. A present
contained path is still checked, and a missing configured contained cache
remains a refusal. Ownership, private permissions, symlink checks, cache
identity and pending-write retention remain enforced.

Generated fixtures reproduce the original failure and exercise read-only
inventory plus removal with retained pending bytes and recovery metadata.
The retained payload, VFS metadata and identity bytes match exactly. Additional
cases reject a substituted symlink and the missing configured contained cache.
All 70 focused uninstall tests and all 499 Python helper tests pass. Python
compilation, Markdown lint on three changed documents, 57 relative targets
and diff checks pass. No Go source is changed.

On the affected host, restoring an empty unused contained directory made its
older installed helper usable. The owned external cache was empty; its root
permissions were restricted to `0700`, and the failing user service was stopped
without changing enablement. Read-only inventory and cache safety inspection
then passed. The wizard reached its first retention prompt and was cancelled
before any retention or deletion choice; the vault configuration hash remained
unchanged. Configuration and cache contents were preserved; no Degoo request
or remote-data operation was needed.

## Optional vault import and chosen encryption password — 2026-10-19

A fresh wizard installation accepts a chosen encryption password without a
saved `vault.json`. Hidden entry and confirmation happen before account login;
the private unbound configuration is created first so automatic-login
credentials use the chosen data key. Imports and ordinary upgrades preserve
existing encryption settings. Remote folder selection supports both a new
child folder and an existing dedicated folder.

Existing folders selected without an imported configuration require a separate
read-only encrypted-content check before local binding or writable startup.
The check uses direct metadata and range-download APIs, without constructing
the mutating filesystem backend. It authenticates a bounded encrypted sample.
Provider failures, wrong encryption parameters and inconclusive results stop
setup. An apparently empty listing cannot verify the password and requires
explicit initialization confirmation.

This is a legacy crypt/chunker setup feature. It is not an integrity audit of
the entire vault, proof of fresh provider listings, or completion of the
scalable plan's remote bootstrap/latest-version recovery gates. Custom salts
and format settings still need to be retained. No live credentials, service,
cache, mount or provider data was changed during implementation.

Python validation passed: all 496 helper tests, including 63 installer tests
and 89 wizard tests, plus compilation of the changed helpers. Generated
fixtures verify exact secret storage, preservation of imported settings,
create/check/bind ordering, explicit empty-folder confirmation, and failed
checks blocking binding and service startup. A real staged-install regression
failed before the resume fix and now confirms that an unbound install can save
its first chosen key before login. Bound installs with a missing key and any
attempt to replace an existing key remain refused.

The verifier bounds work to 45 seconds, 32 pages of at most 128 items, 1,024
listed records, 32 directories and depth eight below the vault root. It
downloads at most the crypt header and first authenticated block (65,584 bytes)
of one candidate object. Placeholder probes share the listing budget and
require the exact SHA-256-derived self-marker before treating a zero-byte
provider item as a directory. The first candidate authentication failure stops
the check; a later healthy sibling cannot hide an observed corrupt block. Empty
encrypted files alone cannot authenticate a password.

Generated Go fixtures cover custom salts, changed names/metadata, malformed
responses, mixed plaintext/encrypted entries, damaged and truncated content,
ignored ranges, cancellation, pagination/traversal limits and both promoted
and self-marked directory representations. No test contacts Degoo.
An existing populated-cache capacity test initially failed under race-detector
load because its five-second snapshots expired during measurement. The fixture
now extends snapshot expiry while measuring capacity and eviction; production
expiry and separate expiry tests are unchanged.

Final validation passed:

- A fresh Go build and the full credential-free Go suite: 195 test packages.
- Race tests and `go vet` for `./cmd/degoo` and `./backend/degoo/api`.
- Scoped `golangci-lint`: zero issues.
- All 496 Python helper tests and Python compilation.
- Markdown lint on seven changed documents, 97 relative documentation targets,
  and staged diff checks.

Go checks use `CI=true`, `RCLONE_CONFIG=/notfound`, short private temporary and
cache paths, and the fresh binary on `PATH`. These offline results do not add
a live FUSE/provider acceptance claim.

## Wizard mount-directory selection — 2026-10-18

The wizard offers the mount directory during local settings, defaulting to
`/mnt/degoo` for a fresh install and the saved path for an upgrade. An explicit
`--mountpoint PATH` takes precedence. Safe custom absolute paths, including
spaces and expanded home paths, are normalized and saved in `install.json`;
the generated launchers and service use the recorded selection. The legacy
`mount` token remains accepted.

Validation rejects relative/traversal/control-character paths, filesystem-root
or program/cache overlap, symlink components, active targets, and nonempty
directories before configuration is rewritten. A path change requires the old
mount to be stopped and its underlying directory empty. Existing owner-private
mode `0500` is preserved. The fixed privileged immutable helper remains limited
to `/mnt/degoo`; custom paths use the ordinary directory guard.

Generated fixtures cover fresh and upgrade wizard defaults, explicit arguments,
invalid-answer retry, paths with spaces, safe stop ordering, unsafe path/cache
overlap, staged updates preserving configuration and cached bytes, restore
preserving the newly selected target while recovering pending cache data, and
uninstall preserving external mount directories or refusing nonempty ones.
No maintenance production change was needed outside the installer and wizard;
the existing helpers consume the shared configured-mountpoint validation.
No live mount, cache, credentials or provider data was changed for this work.
This is offline maintenance coverage rather than a new live FUSE/provider gate.

Validation passed: all 477 Python helper tests, including 54 installer tests,
79 wizard tests and the custom-path maintenance regressions. Python
compilation, Markdown lint for seven documents, 79 documentation targets
(including three archive-relative targets), and `git diff --check` passed.
The path-selection and maintenance regressions failed before the shared
validator was added. A final integration check also rejects relative or
traversing installer roots before normalization. No Go source was changed.

## User-service registration and mount recovery — 2026-10-18

After a fresh installation, declining the wizard's enable/start prompt left
the generated service definition unregistered. The user copied that user unit
into the system manager and enabled it. With no `User=` setting, it ran as
root and failed the private-file ownership checks before mounting. The user
then made the ordinary underlying mount directory writable and created a
small local test file there. That file was not in the vault or upload queue.

The correction adds `degoo-rc service install`, sharing enable's verified
linking, ownership recording, daemon reload and fragment checks while issuing
no enable/start/stop request. Declining startup invokes registration only;
fresh installations stay stopped and without automatic startup. Existing
enablement is preserved. Registration failures return a nonzero result and
recovery instructions. The CLI help, completion, requirements and usage docs
describe the new action and ordinary-user manager. The status helper also
reports systemd's `linked` states as not enabled, instead of mistaking unit
registration for automatic startup.

Live repair stopped and disabled the mistaken system unit, confirmed its
definition matched the generated user unit, removed that copy, and reloaded
the system manager. The local test file was copied to a private recovery
directory and verified before removal from the underlying mountpoint. That
directory was made empty and protected with mode `0500`. The corrected helper
registered the user unit; an ordinary-user start produced an active
`fuse.rclone` mount. The unit is linked without a default-target enablement
link. Local statistics work, with no pending or retrying uploads.

The recovered test file was copied through the confirmed FUSE mount; fsync
and local readback matched, and VFS subsequently reported `uploaded`. The
private recovery copy remains. This does not independently establish cold
remote recovery or general cloud integrity. An offline check found that the
manual vault-password edit had invalidated saved account-credential
decryption. The user completed login again; a repeat offline check confirmed
the credentials decrypt under the current chosen password and salt. No
credentials or plaintext file contents were printed. Forced password fallback
after rejected renewal remains untested in this repair.

Validation passed: all 465 Python helper tests, including 49 installer/service
tests, 75 wizard tests and 26 information-helper tests; Python compilation;
Markdown lint for seven documents; 77 documentation targets (including three
archive-relative targets); and `git diff --check`. The generated regressions
failed before the relevant fixes. There is no Go source change in this work.
After validation, the four changed Python helpers were applied to the live
installation while holding the updater lock and checking each original against
the branch base. `degoo-rc status --json` then reported healthy, mounted,
`disabled; active`, and zero pending uploads. `service help` exposes the new
registration action. No mount restart was needed for these helper updates.

## Uninstall metadata lock mismatch — 2026-10-18

An explicitly requested fresh uninstall, with all retention options declined
and the vault-secret and removal confirmations supplied, refused cleanup
because the persistent metadata cache contained `.lock.lock`. The runtime
passes `.lock` to a shared locking helper which appends `.lock`; the older
uninstaller recognized only `.lock`. The observed file was an owner-private,
empty regular file with one link. It contained no pending file content.

The correction recognizes these two exact lock names with the existing
ownership, permissions, regular-file, link-count and size checks. Lock files
do not consume the 8,192-entry metadata-object budget. Arbitrary names and
unsafe files still refuse cleanup; upload-recovery and cache-identity checks
remain in place. Cache retention copies these locks, so the restore auditor
needs the same exact-name correction to accept a valid retained cache.
Generated fixtures exercise cleanup, restoration and the rejection paths.

Validation passed: all 458 Python helper tests, including 66 uninstall tests
and 16 restore tests; Python compilation; Markdown lint; relative document
links; and `git diff --check`. The pending-data fixture verifies restoration
of a retained external cache containing both metadata lock variants.
The metadata-object budget remains 8,192 even when either or both locks exist.
No Go source was changed, and no provider request was needed for validation.

The user-authorized live retry used the corrected source helper against the
existing installation. Its recorded binary hash matched; an independent scan
found all local VFS metadata clean and well formed, with no pending uploads.
Cleanup completed with no retention selected: the installed program,
configuration (including the vault secret), cached file data, logs, owned
command link and user-service links were removed. The external cache retains
only the empty cache-lock sentinel intentionally preserved by the helper.
Two inspected service overrides from earlier cache/pacing setup were removed
separately. Systemd then reported `LoadState=not-found`, `ActiveState=inactive`,
an empty fragment path and no drop-ins. No remote request or mutation occurred.
A fresh installation and rsync reseed remain for the user; no replacement
vault or new remote format was created.

## Update restart after remote deletion — 2026-10-18

The update from successful staging build `39101975418` (`49d220745d31`)
verified and installed its package, but the service exited before mounting.
Fresh startup logs showed a configured-root `GetOverlay4` failure reported as
`GraphQLException`. The saved access token had been renewed and was unexpired;
the persisted provider cooldown had elapsed. Read-only API checks returned
HTTP 200 with `Got empty result!` for that root and `Invalid input!` for its
children, while account-root listing succeeded. Bounded, shallow discovery
found no matching configured ID or cached vault name. The user confirmed
deleting the remote contents to start fresh. No remote object was created,
changed, or deleted during diagnosis.

The owned service was stopped to end its restart loop; enablement, installed
package, configuration and cache were preserved. The local pending-upload
inventory reported zero; that is not proof that cached clean ranges can
recover the deleted remote data. No rebind, cache reset, or remount was
performed. Private folder IDs, account inventory and credentials are omitted.

The follow-up adds a read-only candidate root check before mounted update
preparation, a root-only `location` option, bounded fresh failure summaries,
and stopping an owned service only when a failed restart leaves no mount.
Offline validation passed:

- Python helper discovery: all 454 tests. Regression cases cover candidate
  root checks, older-candidate fallback, timeout/failure before shutdown,
  unchanged-payload no-op, fresh sanitized errors, rotated/truncated logs,
  and stopping only an owned service with a confirmed absent mount.
- Fresh `go build` and the credential-free repository-wide Go suite: all
  195 test packages, with isolated cache/configuration and `CI=true` for the
  existing unreliable-network-test policy.
- `go test -race -count=1 ./cmd/degoo`, `go vet ./cmd/degoo`, and
  `golangci-lint run ./cmd/degoo`: passed. Root-only regressions cover one
  request, unavailable ancestors, failed roots, and cancellation.
- Python compilation, Markdown lint for the three changed documents,
  36 relative documentation links, and `git diff --check`: passed.

These checks do not establish live successful restart or restore deleted
remote files. The safeguards have not been installed on the affected server;
the service remains stopped pending deliberate fresh-vault recovery.

## Live selective rate-limit investigation — 2026-10-15

The [incident record](RATE_LIMIT_INVESTIGATION.md) preserves the earlier live
diagnostics and their limits; this documentation update makes no new provider
requests and does not rerun the incident. At inspection, the mounted service
was active but native `ls` returned `EIO`; 78 local writes totaling
33,286,847,218 bytes (about 31.0 GiB) were queued for retry with no active
uploads. Cache-volume space was available. A saved global API cooldown held
the client until `2026-10-15 21:30:29 UTC` (`2026-10-16 00:30:29 EEST`).

The user reported working Windows-browser listing, download, and test upload
on the same LAN during that hold. Independent same-account listing replays
from Linux returned HTTP 200 before the deadline, including HTTP/1.1 and
minimal-header controls. Other listing requests returned 429, including a
second page after a successful 1,000-entry page. Smaller browser-style pages
also failed on a later attempt. Linux web requests additionally exposed the
service-worker/CORS path that can mask a network failure as an empty 504.

This establishes selective request outcomes and the client's broader local
hold, not an account-wide ban, a reliable page-size remedy, or a known quota.
No complete affected-directory listing, independent cloud audit, or resumed
upload was established. No mount restart, cache reset, installed-account
authentication change, or remote mutation occurred in that investigation.
The exact comparison matrix, retry headers, and unresolved causes are in the
incident record; offline scalability fixtures remain separate evidence.

## Production metadata bounds and indexed warm lookup — 2026-10-15

Delivery: [PR #50](https://github.com/georgeasiminei/rclone-degoo/pull/50),
targeting `staging`; merging remains with the user. Implementation commit:
`be976e776`.

This increment rebases the preserved partial P2 work onto merged PR #49 at
`a0115f690`. It changes the existing backend's snapshot admission and warm name
lookup, without a new provider operation, remote format, installed-runtime
change or data reset. The active user goal is still the full scalable storage
plan. The user's robustness-first, single-writable-Linux-host preference is
recorded in requirements and the decision; publication/recovery proof remains
open, including remote effects completing after a local writer dies.

Local-only checks at 10:20 and 14:23 UTC found the shared-client provider hold
still expiring at **2026-10-15 21:30:29 UTC**. No live Degoo request or account
authentication work was performed. Worker service limits interrupted API and
architecture work; the coordinating agent preserved partial backend tests and
completed implementation directly. No unmerged work was discarded.

Ten new test functions cover snapshot byte/record admission, complete uncached
oversized results, matching/copy isolation, cancellation and cooldowns,
mutation invalidation, parent-hint string admission, duplicate names,
backup-only recovery, promoted placeholders, fresh pagination and warm
allocation across generated scales. The existing populated-snapshot fixture
now asserts accounted-byte/record/hint budgets too. See
[SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) for exact counters, accounting
scope, timing qualifications, and the additional retained-memory tradeoff.

The initial implementation passed the credential-free repository-wide suite
(all 186 test packages), then review found that a target-specific lowercase
backup prefix could lose recovery accepted by the existing hex decoder. A
regression failed with no returned item for that valid backup. The correction
selects the indexed backup-name group and retains the original decoder. It
passed cold and warm recovery, exact-name precedence and duplicate/placeholder
cases. The final build and suite include this correction; cache lifetime and
metadata deadlines were not extended.

Checks:

- Fresh full `go build -o` to a temporary binary, used first on `PATH` with a
  temporary FUSE-helper alias and isolated runtime cache. Short owner-private
  `TMPDIR`/`GOTMPDIR` paths preserve existing socket-test requirements.
- `CI=true RCLONE_CONFIG=/notfound go test -timeout 20m -count=1 ./...`:
  the final run passed all 186 test packages after the backup correction. The
  existing CI unreliable-network-test policy skips external self-update tests;
  this does not validate an external download.
- `RCLONE_CONFIG=/notfound go test -race -p 1 ./backend/degoo/... -count=1`:
  backend, API and auth passed sequentially.
- `go vet ./backend/degoo/...` and
  `golangci-lint run ./backend/degoo/...`: passed with zero lint issues.
- `RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 go test ./backend/degoo
  -run '^TestLookupChildIndexedWarmAllocationBounds$' -count=1 -v`: passed at
  1,000, 10,000 and 100,000 siblings, zero warm requests, about 560 bytes per
  direct lookup and no growth with unrelated siblings.
- The same opt-in environment with `go test ./backend/degoo/api -run
  '^TestScalabilityPopulatedMetadataSnapshots$' -count=1 -v`: passed through
  two 100,000-entry snapshots with 64,800,336 accounted bytes, below 67,108,864;
  hints retained 241,664 string bytes. Parent-count and warm-hit assertions
  remain intact; no deadline or correctness assertion was weakened.
- Existing warm NewObject benchmarks, isolated single-iteration samples:
  784/768/768 bytes per operation and zero HTTP requests/pages at
  1,000/10,000/100,000 siblings. These are local fixture timings, not provider
  performance guarantees.

Changed Markdown lint, 65 relative file links, eighteen unique task IDs,
formatting and staged diff checks passed. The final race and full-suite runs
were sequential to avoid the existing populated-snapshot fixture's TTL/load
sensitivity. No timeout or cache freshness lifetime was increased.

This does not close P2-BOUNDS: transient whole-directory/in-flight storage,
backend directory-ID/placeholder caches and P3-dependent cold index integration
remain outside this increment. No new-format mounted recovery, live scale,
large encrypted logical file, provider publication/fencing or power-loss
exercise was run. P1 provider and P3–P6 acceptance gates remain open.

## Durable allocation and adaptive placement — 2026-10-15

Delivery: [PR #49](https://github.com/georgeasiminei/rclone-degoo/pull/49),
targeting `staging`;
merging and source-branch deletion remain with the user.

This continuation starts from merged PR #48 at `773b9f8c3`, using a fresh
isolated worktree. Fetch/ancestry and actual-change checks found the existing
branches integrated; no previous worktree or branch was overwritten. The
implementation plan was updated before adding the two Linux-only test models.
No production source, mount format, installed runtime or data was changed.

Local-only checks at 09:56 and 10:00 UTC still found the persisted provider
hold through **2026-10-15 21:30:29 UTC**. No Degoo API request, authentication
renewal/login, reset, pending-write discard or cache removal was attempted.
Provider continuity/deletion and publication/fencing remain unverified.
[SCALABILITY_DECISION.md](SCALABILITY_DECISION.md) records the reviewed public
source scope and a future bounded live matrix; no speculative schema mutation
or account-wide trash inventory was used.

The eleven new tests exercise real temporary-file journal replacement,
intent-before-simulated-create/delete ordering, disk reopen, checkpoint errors,
unknown acknowledgements, pending bytes, incomplete graphs, protected file-data
IDs, stale epochs, finite lifetime allocation and cleanup cursors. The separate
adaptive fixture compares the physical directory tree and counts reservations,
ghosts, skew/depth exhaustion and directory-prune work. Its saved state is a
whole-map structural serializer after modeled faults; it does not establish
durable ordering before remote effects. Exact counters and liveness limits are
in [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md).

The first repository-wide run passed 185 packages but failed three existing
command socket tests: the private temporary prefix still exceeded their Unix
socket length budget. All three passed with an eight-character private temporary
root. The final repository-wide run passed all 186 test packages under that
setup. No test timeout, cache lifetime or production deadline was increased.
`CI=true` uses the existing unreliable-network-test policy, including self-update
tests; it does not establish external download compatibility.

An initial overlapping full-suite/race run passed backend and auth race tests,
but the API populated-snapshot memory fixture took 6.74 seconds for its
1,000-entry case. Its five-second cache expired during construction, retaining
25 rather than the expected 32 parents. The isolated fixture rerun passed under
race detection; the final sequential scoped race run also passed after the full
suite. This is a resource-sensitive fixture limitation, not a provider result.

Checks completed:

- Fresh `go build -o` to a temporary binary, placed first on `PATH` for the
  repository tests; runtime caches and temporary directories were isolated.
- `CI=true RCLONE_CONFIG=/notfound go test -timeout 20m -count=1 ./...`:
  all 186 test packages passed with short private `TMPDIR`/`GOTMPDIR`.
- `RCLONE_CONFIG=/notfound go test ./backend/degoo -run
  '^TestScalability(Durable|AdaptivePlacement)' -count=1 -v`: all eleven passed.
- `RCLONE_CONFIG=/notfound go test ./backend/degoo/api -run '^TestGetItem'
  -count=1 -v`: existing direct-ID failure/cooldown cases passed.
- `go vet ./backend/degoo/...` and
  `golangci-lint run ./backend/degoo/...`: passed with zero lint issues after
  removing one unused initial store assignment in the new test.
- `RCLONE_CONFIG=/notfound go test -race -p 1 ./backend/degoo/... -count=1`:
  backend, API and authentication packages passed after isolated diagnosis.
- Changed Markdown lint, 60 relative links, eighteen unique task IDs,
  `gofmt` and staged diff checks passed.

No live backend integration, new-format mount recovery, remote-only journal
rebuild, provider fencing, encrypted logical file above 20 GiB or host power-loss
exercise was run. Local graph completeness and remote authority/visibility are
explicit fixture assumptions. Cleanup can retain safe orphans and unresolved
present deletes; non-reused lifetime slot ordinals eventually stop allocation.
P1-LOOKUP and P1-DECISION retain their live gates, and P3-FORMAT is unstarted.

## Scalable storage lookup and publication decision — 2026-10-15

Delivery: [#48](https://github.com/georgeasiminei/rclone-degoo/pull/48),
merged into `staging` on 2026-10-15 at `773b9f8c3`.

The work starts from fetched `origin/staging` at `a4dcb1902`, after confirming
PR #46 is merged and both its commits are integration ancestors. The shared
checkout's update-progress commits are also merged. Implementation uses a new
isolated worktree; the shared checkout and other worktrees remain untouched.

The implementation plan was updated before new test-only publication
experiments. [SCALABILITY_DECISION.md](SCALABILITY_DECISION.md) records the
conditional architecture, alternatives, observable metadata, publication and
fencing assumptions, recursive placement, stable directory identity, cleanup,
and exact provider activation gates. It selects no production wire format.
The current mount and encryption stack are unchanged.

Local read-only inspection found a persisted provider cooldown until
**2026-10-15 21:30:29 UTC**. No live Degoo API call, authentication renewal/login,
installed-runtime change, cache deletion, or test-vault reset occurred. No
provider direct-ID stability, atomic root replacement, fencing, physical cap,
or garbage-collection guarantee follows from the localhost/model fixtures.
P1-LOOKUP and P1-DECISION remain open for their provider evidence gates.

The new API tests cover direct-ID cancellation/transport timeout, malformed
JSON, 503 with Retry-After, successful single HTTP 401 refresh, bounded repeated
401, and persisted/reloaded 429 with zero token/API work while held. Existing
null, forbidden, identity-mismatch and GraphQL-auth fixtures remain in use.
Publication fixtures test identical lost acknowledgements before/after apply,
CAS versus epoch fencing, bounded snapshot/staging admission, stable directory
IDs, finite physical placement, and single-actor metadata orphan cleanup.
[SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) records counters and limits.

The first full-suite attempt failed four existing `cmd/degoo` socket fixtures
because the temporary path was too long. Those tests passed with an eight-byte
private temporary root. The next unrestricted full run passed `cmd/degoo` but
failed the existing `cmd/selfupdate/TestInstallOnLinux`: the latest upstream
beta archive returned HTTP 404. No updater code was changed. Final offline
repository validation uses `CI=true`, the repository's existing
`testy.SkipUnreliable` policy for network-dependent tests. This skips the
unreliable self-update tests; it is not a successful external-download test.

Final checks passed:

- Fresh `go build -o` to a temporary binary, used on `PATH` for repository
  tests. Runtime caches were isolated and temporary directories owner-private.
- `CI=true RCLONE_CONFIG=/notfound go test -timeout 20m -count=1 ./...`:
  186 test packages passed, using the short private `TMPDIR`/`GOTMPDIR`.
- `RCLONE_CONFIG=/notfound go test -race ./backend/degoo/... -count=1`:
  backend, API and authentication packages passed.
- `go vet ./backend/degoo/...` and
  `golangci-lint run ./backend/degoo/...`: passed with zero lint issues.
- `RCLONE_CONFIG=/notfound go test ./backend/degoo/api -run '^TestGetItem'
  -count=1 -v` and the three `TestScalabilityPublication` cases: passed.
- `RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 go test ./backend/degoo
  -run '^TestScalabilityEncryptedShardModel$' -count=1 -v`: passed at
  1,000, 10,000 and 100,000 records. This repeats the earlier index comparison;
  no new provider or peak-memory measurement is claimed.
- Changed-document Markdown lint, relative links, unique task IDs, and staged
  diff checks passed.

The first publication-model attempt found a nil placement-parent map;
preflight was corrected. Review then added truly identical unapplied/applied
lost-acknowledgement errors, staging refusal under unknown state, independent
CAS/fence checks, and retention of orphan reservations with unknown paths.
All final checks above include these corrections. No live backend integration,
new-format mounted recovery, or live large-file acceptance was run.

## Scalable storage P1 memory and candidate prototypes — 2026-10-15

Delivery: [PR #46](https://github.com/georgeasiminei/rclone-degoo/pull/46),
targeting `staging` and merged on 2026-10-15.

This slice completes generated P1-FIXTURES measurements and adds test-only
persistent-ID and encrypted radix-index candidates. It changes no production
mount format. [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) records the
measurements, reproduction commands, and model/provider distinction; the
active task state and exact next action are in
[SCALABLE_STORAGE_PLAN.md](SCALABLE_STORAGE_PLAN.md).

The opt-in fixtures passed at 1,000, 10,000, and 100,000 records: sampled cold-list
process heap, multiple populated API snapshots, persistent-ID cache/reopen,
and encrypted-node lookup/update/rename/rebuild. Additional checks cover disk
entry/byte eviction, missing/corrupt cache recovery, in-flight invalidation,
all write boundaries of a generated leaf split, overwrite staging failures,
ciphertext tampering/substitution, and missing nodes. The largest memory and
lookup measurements ran separately from repository-wide validation.

The first race-suite attempt hit the existing delete operation-trace assertion:
three parent invalidations were counted instead of four, with the expected
requests and other counters. Ten isolated repetitions passed. The fresh-list
worker closed its completion channel before recording the final invalidation.
A deterministic regression holds post-completion cleanup and failed before
the fix with zero end-invalidation events. Recording the event alongside the
invalidation, before waking callers, makes completed-operation counters reliable.
It retains the existing metadata-to-statistics mutex order and does not change
request, cache-invalidation, or cooldown behavior.

The initial full repository run passed all Degoo backend packages but failed
one existing command socket fixture because the temporary prefix still made
its Unix-socket path too long. Final validation uses a shorter owner-private
disk-backed temporary directory. No production socket-path behavior is changed.

Final checks passed against the restored scalability sources in an isolated
worktree, leaving concurrent work on the shared checkout untouched:

- Fresh `go build -o` and the credential-free repository-wide
  `go test -timeout 20m ./...`, using the fresh binary on `PATH`, an isolated
  runtime cache, and short private `TMPDIR`/`GOTMPDIR` paths.
- `RCLONE_CONFIG=/notfound go test -race ./backend/degoo/... -count=1`,
  `go vet ./backend/degoo/...`, and
  `golangci-lint run ./backend/degoo/...` (zero issues).
- The opt-in generated 100,000-entry backend/API memory, persistent-ID,
  and encrypted radix-node fixtures, including the default smaller cases.
- Changed-document Markdown lint, 39 relative file-link targets, and staged
  diff checks.

The interrupted validation run in the shared checkout is not used as final
source validation. No live backend integration suite or mounted large-file
acceptance was run; those require a dedicated generated-data vault and remain
separate acceptance gates.

No live Degoo requests, installed-runtime changes, vault resets, pending-write
deletion, or production-format changes occurred. Remote direct-ID stability,
root publication, physical placement bounds, orphan cleanup, and mounted large
files remain unverified. The encrypted model assumes an atomic root swap and
a direct-ID store; it is not evidence of remote crash-safe publication.

## Scalable storage P1 follow-up — 2026-10-15

The follow-up replaces full-table parent-hint eviction with ordered eviction,
rejects direct-ID metadata with a different response identity, and adds
retained-heap/occupancy measurements plus paginated and encrypted-stack traces.
Delivery: [PR #44](https://github.com/georgeasiminei/rclone-degoo/pull/44),
targeting `staging` and merged on 2026-10-15.
See [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) for measured counters,
reproduction commands, and memory-measurement limits.

The initial full-suite attempt failed only the existing `cmd/degoo` socket
fixtures: the private temporary prefix still made their Unix socket paths too
long. That package passed with a shorter private disk-backed temporary path.
Initial lint reported ineffective nil assignments in the memory fixture; they
were removed because the result slices are unused before GC. Scoped vet and
lint subsequently passed. An initial race run used the encrypted fixture's
bare `128` chunk-size setting (128 KiB), which did not split its small input;
the fixture uses explicit `128B` to exercise multiple encrypted chunks.
The encrypted rename readback then exposed stale receiver metadata after a
successful `Object.Update`. Refreshing that metadata also required preserving
version snapshots for concurrent operations: the existing update/remove
regression caught a race and replacement deletion in the initial fix. The
final implementation synchronizes metadata access and pins each operation's
observed version before path serialization. Review also identified that Move
needed a post-lock source-version check: its regression failed before the
check with stale-ID move/rename requests and passes with zero such mutations,
retaining the replacement at its original path.

Final validation passed:

- Fresh `go build -o` to a temporary binary, with that binary on `PATH` for
  the credential-free repository-wide `go test -timeout 20m ./...` run.
  Both `TMPDIR` and `GOTMPDIR` used the short private disk-backed directory.
- `RCLONE_CONFIG=/notfound go test -race ./backend/degoo/... -count=1`,
  `go vet ./backend/degoo/...`, and
  `golangci-lint run ./backend/degoo/...` (zero issues).
- The opt-in `TestScalabilityDirectory` and
  `TestScalabilityRetainedMetadataMemory` fixtures at 1,000, 10,000, and
  100,000 entries; API snapshot/hint occupancy bounds; the saturated hint
  insertion microbenchmark before and after the eviction change.
- Paginated mutation traces with 1,005 siblings and the encrypted
  chunker/crypt lifecycle with full and ranged reads, overwrite, rename
  through its returned object, and deletion.
- Direct-ID identity regressions (failed before the fix), repeated
  object-update readback (failed before the fix), failed-update preservation,
  stale-source Move rejection (failed before the check), and the existing
  concurrent update/remove regression under the race detector.
- Changed-document Markdown lint, 36 relative file links, and diff checks.

P1-FIXTURES and P1-LOOKUP remain in progress for peak index/multi-parent memory
and live direct-ID behavior. The design comparison and new remote format
remain unstarted.

No live Degoo requests, mount installation, data resets, pending-write deletion,
or remote-format changes are part of this follow-up. The measurements do not
establish live scalability, quotas, the object cap, or new-format readiness.

## Scalable storage P1 baseline — 2026-10-15

Delivery: [PR #43](https://github.com/georgeasiminei/rclone-degoo/pull/43),
targeting `staging` and merged on 2026-10-15.

The first implementation slice adds generated directory fixtures at 1,000,
10,000, and opt-in 100,000 entries, API-call/invalidation traces for file
operations and chunker discovery, and later-page 429/cancellation regressions.
[SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) records the counters,
reproduction commands, measurement limits, and remaining P1 evidence.

Validation passed:

- A fresh `go build -o` to a temporary binary.
- The credential-free repository-wide `go test -timeout 20m ./...`, with the
  freshly built binary on `PATH` and a short, private, disk-backed temporary
  directory for both `TMPDIR` and `GOTMPDIR`.
- `RCLONE_CONFIG=/notfound go test -race ./backend/degoo/... -count=1`,
  `go vet ./backend/degoo/...`, and
  `golangci-lint run ./backend/degoo/...` (zero lint issues).
- `RCLONE_CONFIG=/notfound RCLONE_DEGOO_SCALE_TESTS=1 go test
  ./backend/degoo -run '^TestScalabilityDirectory$' -count=1 -v`, including
  complete 100,000-child enumeration, cold/warm lookup, direct ID, and refresh.
- All 30 combinations in `BenchmarkScalabilityDirectory`, using
  `-benchtime=1x -benchmem -count=1`, with returned page ceilings of 1,000 and
  100. The client still requests 1,000; the smaller ceiling simulates
  under-filled provider responses. Allocations/op are not peak index memory.
- Scoped Markdown lint, relative file-link checks, and `git diff --check`.

An initial repository-wide run failed only in `cmd/degoo` socket tests because
its temporary directory prefix made Unix socket paths too long. The command
package and subsequent full repository suite passed with a shorter private
temporary directory. An initial benchmark
also overlapped heavy validation and hit the existing metadata deadline in
several 100,000-entry cases; the isolated final benchmark and explicit large
correctness run passed. The large test remains opt-in so routine race checks
do not depend on uninstrumented large-directory timing.

P1-RATELIMIT's controlled cases are complete. P1-FIXTURES and P1-LOOKUP remain
in progress because peak retained index memory, paginated mutations, and the
encrypted stack still need measurements. No new remote format, provider quota,
large live directory, or live object cap has been validated. No Degoo requests,
installed-runtime changes, vault resets, or pending-write deletions occurred.

## Updates across helper inventory changes — 2026-10-14

An installed updater without `stats.py` in its helper inventory rejected the
first statistics package during manifest comparison. The published archive,
binary hash, and commit matched; only the payload hash differed because the
old updater omitted the added helper. The mismatch was reproduced with the
published staging artifact before any runtime preparation or installation.
Its target installer produced the expected manifest hash.

The updater now parses each tree's literal payload inventory without importing
or executing that tree's installer. Both archive completeness and canonical
payload hashing use those declarations. Existing checksum, source commit,
binary validation, and post-install matching remain in place.

Validation passed:

- The inventory-drift regression failed before the fix. Added- and
  removed-helper fixtures pass afterward, and a changed or missing new helper
  prevents an installed-payload match. Invalid literal inventories are refused.
- The full Python helper suite passed (428 tests), the packaging self-check
  passed, and the fixed updater validated the exact published artifact against
  its update manifest, archive checksum/size, binary hash, and commit.
- The published package's own updater upgraded an isolated old-inventory
  installation using `--archive`; generated pending bytes and vault
  configuration remained byte-for-byte unchanged. Runtime stop/start actions
  were mocked, so this is not evidence of a live Degoo service update.
- `TestVaultNativeUpdatePreparation` passed with both local FUSE mount
  implementations. Pending generated encrypted data survived preparation and
  remount, then completed upload to the local fixture.
- Scoped Markdown lint and `git diff --check` passed.

Validation made no Degoo requests and did not stop or replace the installed
production mount. An interrupted logical-file upload retries from the start
after restart; byte-offset or persistent per-chunk resume is unsupported.
Active multi-chunk interruption against Degoo remains a live acceptance gate.

## Local performance statistics — 2026-10-14

`degoo-rc stats` reads aggregate counters from the private mount socket and
supports cache bars, human-readable sizes, JSON, and live local refreshes.
The VFS snapshot separates dirty, clean, evictable, and open/grace-period
pinned cached ranges. Existing upload/download bodies collect recent rates
and cumulative encrypted payload bytes, including retries and verification
reads. Request and metadata-cache counters, local disk space, uptime, and
runtime memory are also displayed. Ordinary `status` and `info` show binary
units in text reports; `status --json` retains exact byte values.

Validation passed:

- The full credential-free Go suite with the freshly built binary on `PATH`
  and `RCLONE_CONFIG=/notfound` (186 packages passed), plus all 424 Python
  helper tests, including 29 focused statistics/CLI/completion checks.
- Focused Go tests for Degoo commands, backend/API/auth, VFS and its cache,
  downloaders, and writeback queue. A cache regression first reproduced stale
  usage and missing classifications, then passed with sparse-range accounting.
- A local socket regression reads statistics repeatedly while a wrapped
  filesystem refuses remote metadata calls; no such calls occur and pending
  writes remain available. Error replies do not expose internal details.
- Upload HTTP fixtures verify payload-only accounting on successful and failed
  attempts, in-flight counts, rolling-rate expiry, concurrent snapshots, and
  received-body download accounting. No Degoo service is contacted.
- Race detection for `cmd/degoo`, `backend/degoo/api`, and `vfs/vfscache`;
  scoped golangci-lint; a fresh Go build; package self-check and a real archive
  build; scoped Markdown lint and `git diff --check`.
- Both opt-in localhost FUSE tests (`TestVaultNativeMount` and
  `TestVaultUploadStatusNativeMount`) passed with generated encrypted data.

These checks did not change the installed service or vault and did not make
Degoo requests. Existing running mounts require a normal restart with the new
binary before they expose runtime statistics; retain pending cache data.

## Bash command completion — 2026-10-13

`degoo-rc completion bash` generates completion for command names, help topics,
service actions, and benchmark modes. It leaves normal filename completion
available for other arguments. Activation is an explicit `source` command;
shell profiles and customer installations were not changed during validation.

Validation passed:

- Seven completion regressions, including the installed PATH launcher,
  help and invalid-shell handling, command candidates, and filename fallback.
- Interactive Bash in an isolated home: `up` plus Tab completed `update`,
  `u` plus two Tabs listed `update` and `uninstall`, and a local filename
  containing spaces completed correctly.
- The full Python helper suite, a fresh Go build, and the full offline Go
  suite with `RCLONE_CONFIG=/notfound`. The first Go run picked up an older
  system rclone lacking `test makefiles`; the archive test and full suite
  passed with the freshly built binary on `PATH`.
- Package self-check and a real archive build, including completion help
  verification, changed Python and generated Bash syntax checks, scoped
  Markdown lint, and `git diff --check`.

Other shells are unsupported by this completion command. No Degoo network
access or vault operation was needed for these checks.

## Lightweight update checks and directory preflight — 2026-10-13

The packaging workflow publishes a separate small update artifact containing
the archive checksum and a manifest with archive size, binary and managed
payload hashes, and source provenance. The updater checks those fields and
hashes installed program files directly before downloading the archive. A
matching payload skips the large download and all runtime changes; different
payloads are bound to the manifest again after full archive verification.
Missing metadata uses the visible legacy comparison path, while invalid
metadata refuses the update. Slow phases report elapsed time every ten seconds.

For actual changes to an active mount, the updater detects a working directory
inside the mount before preparation; metadata preflight also checks it before
the full download. It checks the current process, inherited shell directory,
and accessible same-owner ancestors. It prints a shell-quoted `cd` command for
the invoking shell, whose directory a child process cannot change. Permission
restrictions can limit ancestor inspection; normal unmount still refuses busy
mounts. No-op updates remain valid from within the mount.

Validation passed:

- All 386 Python helper tests, including metadata-only downloads, legacy
  fallback, malformed metadata rejection, manifest/archive binding, progress
  reporting, directory preflight before preparation, and no-op behavior.
- Eight directory-helper tests cover aliases, subdirectories, sibling paths,
  deleted cwd links, bounded ancestry, and a real subprocess whose parent keeps
  its directory while the child moves outside it. All paths are disposable.
- Packager self-checks verify the generated manifest against finalized archive
  bytes and an independent payload hash, rejected-build output preservation,
  staged installation, and repeatable archive/checksum/manifest outputs.
- A real-binary archive install/update roundtrip in a disposable home preserved
  configuration and pending-cache fixture bytes. Changing an installed helper
  was detected despite an unchanged saved build identity. A mocked GitHub
  metadata download using the generated hashes then skipped the archive and
  preserved the complete installation snapshot, including with `--delete-cache`.
  Clean commit provenance for that mock was a generated fixture, not a claim
  that the uncommitted build came from staging.
- A fresh Go build, `make quicktest`, changed-document Markdown lint, and
  `git diff --check`. The Go suite used an isolated VFS cache with the fresh
  executable and a temporary alias to the installed FUSE 2 helper on `PATH`.

The new separate artifact cannot be tested against a staging workflow run until
this change is merged and packaged. Workflow artifact paths were inspected;
`actionlint` was unavailable. No customer mount or Degoo account was used.

## Unattended client updates — 2026-10-13

`degoo-rc update` now resolves the latest successful staging package, verifies
its checksum and build provenance, preserves configuration and cache by
default, and restores the prior active runtime mode. The optional
`--delete-cache` path refuses pending writes and unknown or incomplete cache
metadata. Installer failure restores the previous program files; restart
failure returns an error with the new build and retained state available for
recovery. An abrupt interruption can leave a program snapshot requiring
explicit recovery; automated crash rollback has not been established.

Validation passed:

- All 351 Python helper tests, including 18 updater tests and the installer
  identity and inherited-lock regressions. Focused updater tests also passed
  after preserving a manual mount's original working directory. Binary help
  checks also prove they cannot create configuration in the real home.
- A fresh `go build`, package self-check, Python syntax checks, scoped Markdown
  lint, and `git diff --check`.
- A real package built with the fresh binary, upgraded into a disposable
  contained installation holding an older binary. Repeating the update with
  `--delete-cache` was a no-op. No customer installation or Degoo vault was
  used for this check.
- Read-only live GitHub workflow lookup, artifact download, and checksum
  verification against the latest successful staging package.
- The full offline Go suite with `RCLONE_CONFIG=/notfound`. The first run used
  an older system `rclone` lacking `test makefiles`, and lacked `fusermount3`.
  The archive test and native mount/Docker tests passed with the fresh binary
  and a temporary PATH alias to the installed root-owned FUSE 2 helper;
  the complete suite was then rerun in that environment.

Offline updater regressions cover checksum and archive-path rejection, no-op
behavior without prior build identity, actual staged installation with inherited
cache-lock ownership, byte-preserved configuration and pending data, rollback,
service ownership conflicts, inactive-service handling, manual restart
arguments and private logging, busy mounts and cache-lock conflicts, clean
cache deletion without changing the held lock inode, orphan-data refusal, and
cleanup/startup failure reporting. Runtime control is mocked in these updater
checks. A live service-managed or manual Degoo mount update, provider behavior,
and power-loss recovery remain unverified.

## Pending-write upgrade protection — 2026-10-13

An active mount prepares for upgrade through its private Unix socket. During
preparation, new opens and mutations return busy while existing file and
directory handles can finish. The default 30-second `--writer-timeout` covers
handle drainage and syncing cached data, recovery metadata, and their parent
directories to local storage. Timeout, cache errors, or a busy normal FUSE
unmount leave the old runtime running. Systemd is stopped only after normal
unmount succeeds. Preparation is cancelled on failure or expires after a
two-minute lease if the updater disappears.

Default upgrades retain pending local uploads for the restarted mount.
`--wait-for-uploads DURATION` optionally requires both an empty writeback queue
and no dirty cache items within a separate deadline before shutdown. This is
distinct from local persistence. No-op updates bypass preparation and cache
deletion. A mounted older binary without the preparation protocol must be
stopped explicitly for its first upgrade.

Validation includes:

- Go endpoint and VFS tests cover handle drainage, blocked admission, timeout
  and client-disconnect recovery, token ownership, lease renewal and expiry,
  dirty cache detection outside the upload queue, and optional upload drainage.
- Cache tests cover closed dirty files, nested directory syncing, missing dirty
  data, atomic metadata replacement, and bounded waits for locked cache items.
  An operating-system sync already in progress can finish after timeout; failed
  preparation releases admission and never authorizes shutdown.
- Python updater tests cover preparation and renewal before shutdown, failed
  preparation without stopping the runtime, normal-unmount failure without
  stopping systemd, cancellation after unsuccessful shutdown, and lost-response
  lease guidance. All 364 Python helper tests passed, including 31 updater tests.
- An opt-in native FUSE test uses a generated encrypted local vault, closes an
  application writer and directory during preparation, retains a pending cached
  write across unmount and restart, and verifies its eventual encrypted-remote
  publication. Both `mount` and `mount2` passed. This fixture uses no Degoo
  account or customer files.
- A fresh build, package self-check, scoped Go race checks and lint passed.
  The optional `cmount` adapter compiled with locally extracted FUSE 2 headers;
  native `cmount` behavior was not exercised.
- The full offline Go suite passed with the fresh executable, isolated cache,
  and temporary FUSE-helper alias. An initial S3 cached-write failure did not
  reproduce in focused runs, the entire S3 package with a fresh cache, or the
  final full suite. A run overlapping test-source edits had a compilation error;
  the final suite was repeated with source files fixed in place.
- A disposable inactive installation passed an actual archive update and
  unchanged-package `--delete-cache` check. Configuration and pending-cache
  fixture bytes, the cache-lock inode, and the complete no-op install snapshot
  were preserved.

A live service-managed Degoo update and power-loss recovery remain unverified.

## Explicit installation startup errors

Inspection confirmed that the installer already returned status `2` after an
unconfirmed requested mount, but its normal-looking summary did not show a
prominent error. The service was retrying after the immutable external
mountpoint blocked unprivileged FUSE mounting. Clearing that flag and starting
the owned service produced a connected mount, an active running service, and
a successful local health report with no queued uploads. No remote file data
was written during this recovery.

All 250 Python helper tests, the package self-check, scoped Markdown lint,
Python syntax checks, and `git diff --check` passed. Regressions cover explicit
startup errors, service rejection, disconnected and unknown mounts, final
mount-state observation races, read-only immutable-flag inspection, and
successful installation when the user chooses manual startup.

## Incomplete uninstall and recovery

Read-only inspection found an immutable legacy mount directory and a remaining
installation whose service and command links had been removed. Systemd
reported the unit as `not-found` and `inactive`, with an empty fragment path.
Mount-directory blockers are checked before disabling or unlinking services;
final cleanup checks reject success if the root or an owned link remains. A failed
removal exits nonzero and clearly reports incomplete cleanup.

All 244 Python helper tests, the package self-check, scoped Markdown lint,
Python syntax checks, and `git diff --check` passed. Read-only preflights on the
remaining installation produced the immutable-flag recovery command and
accepted the verified absent, inactive service for upgrade.

Fixture regressions cover immutable-directory rejection before link removal,
read-only directory deletion, failed or ineffective deletion, reappearing
owned links, preservation of the external mount directory, and upgrade after
service links disappear. A launcher regression checks error exit propagation
and preservation of an ordinary local file. Further regressions cover unrecorded
command links, interrupted cleanup, and local files in an unused contained
mount directory. No live customer data was deleted to validate this fix.

## Upload status and protected mountpoint follow-up

The follow-up's final offline validation passed:

- `RCLONE_CONFIG=/notfound go test ./...` passed with the freshly built binary
  and contained FUSE helper alias first on `PATH`.
- Targeted backend, Degoo command, both mount implementations, and VFS package
  tests passed, including the VFS upload-status race checks.
- Scoped `golangci-lint` reported zero issues.
- All 228 Python helper tests and the package self-check passed.
- The native FUSE upload-status and underlying mount-directory permission
  smoke test passed with race detection.
- The seven Degoo Markdown documents passed markdownlint, and `git diff
  --check` passed.

These checks cover authentication-first health output and nonzero exit status,
JSON and file-path status, the read-only VFS upload-status attribute, unmounted
local-only files, aggregate queue counts, default and legacy mountpoint
handling, refusal to cover local files, immutable-flag access instructions,
and protected permissions after mount failure or unmount. The VFS document is
a section fragment whose first line is a level-three heading; standalone
markdownlint reports the pre-existing MD041 rule for that heading.

An expired-token refresh initially returned HTTP 401; an earlier probe had
recorded HTTP 403. Authentication was later renewed, and read-only provider
queries confirmed the intended folder. No cloud writes were attempted during
this follow-up. A local vault binding that had used a
folder name where an opaque provider folder ID was required was corrected after
confirming that the prior cache contained only its lock file, with no identity
or cached file data. No actual provider ID or private path is recorded here.
These events do not establish reliable automatic renewal. An immutable flag on
the current mountpoint prevented an ordinary-user FUSE mount because the FUSE
helper requires write access; the mount reports a `sudo chattr -i` instruction
before retrying. The targeted test simulated this access failure and verified
the instruction, and the native FUSE smoke test verified mode-based protection
on its temporary mountpoint. No privileged flag change was made, so final
health of the installed mount remains pending. Earlier live smoke tests used
generated data in a dedicated disposable folder; they do not establish
long-term token renewal by themselves.

New contained installs use `/mnt/degoo`; upgrades preserve their saved
mountpoint. The installer supports `/mnt/degoo` and the legacy in-install
`mount` location, refuses migration while the old underlying directory is
nonempty, and leaves the external directory on uninstall. An immutable
mountpoint can block unprivileged FUSE mounting. After clearing it, the mount
guard protects the directory by removing write permissions after mount and on
failure or unmount. The VFS attribute is disabled by default for generic mounts
and enabled by `rclone degoo mount`.
`uploaded` reports the current VFS version's known remote representation; it is
not a fresh cloud integrity check. Ordinary files under an unmounted
mountpoint are local-only and never enter the upload queue.

## Scoped mount refresh and remote cache staleness

`degoo-rc sync LOCAL_PATH` requests a recursive metadata refresh through the
running mount's private Unix socket. Tests cover remote creation, deletion,
overwrite, nested ancestor refresh, sibling-node preservation, pending local
writes, failed refresh results, socket permissions/cleanup, and invalid
requests. Python socket tests cover relative, absolute, file, missing, and
outside paths; unsafe or missing endpoints; and malformed responses. CLI
tests verify the wrapper enables the socket only for binaries advertising the
new flag, so older binaries remain mountable.

All 201 Python helper tests, the full credential-free Go suite, the binary
build, Degoo command race tests, scoped golangci-lint, package self-check,
changed Markdown lint, and `git diff --check` passed. Local synthetic remotes
exercise the actual VFS and Unix HTTP transport without contacting Degoo.
These checks do not establish live multi-client provider visibility or a
hard propagation deadline. The installed mount was not changed.

## Configuration backups, wizard recovery, and connection checks

The wizard offers retained backups before local installation. Offline tests
cover recovered defaults, preservation of vault secrets and pending files,
refusal of unsafe or changed backups, and exclusive cache ownership. Saved
account credentials are tested before requesting a password; authentication
rejection and provider/network failure follow separate paths. Folder discovery
reports name collisions and requires the original vault configuration to reuse
an existing encrypted folder.

`backup-config` creates a portable private configuration and credentials
backup without copying cache or file data. Checks cover restore-compatible
metadata and settings, source preservation, safe destination handling, and
actual CLI dispatch. Existing pending writes remain on their source computer.

The binary build, full credential-free Go suite, Degoo command race tests,
scoped golangci-lint, and package self-check passed. A synthetic retained backup
was restored with the actual built binary: pending bytes were copied, the cache
identity was rebound offline, the original backup stayed intact, and repeating
the identity rebind succeeded. All 185 Python helper tests passed, including
the complete restored-install flow with provider calls mocked. Markdown lint and
`git diff --check` passed.

These checks did not use live account credentials, change the installed mount,
or establish live provider acceptance. Backup recovery requires a fresh local
installation; it does not merge caches into an existing installation.

## Config-only mount tuning and installer defaults

The installer defaults match the last deployment: 60 GiB cache size and
free-space targets, 750-hour retention, five-second cache cleanup and write-back,
disabled upload readback verification, and 128 MiB plaintext storage chunks.
Read-ahead defaults to 1 MiB, read streams to eight, and directory caching to
30 minutes. Additional read, buffer, attribute, transfer, bandwidth, and idle
timeout settings are available in the private mount JSON without new prompts.
A detected Degoo-enabled binary is validated and reused without a path prompt.

Offline checks passed:

- Full `RCLONE_CONFIG=/notfound go test -timeout 20m ./...`, with the new test
  binary and contained FUSE helper on `PATH`.
- `go build`, `go test -race ./cmd/degoo/...`, and scoped golangci-lint v2.14.0
  with zero lint issues.
- All 147 Python helper tests and the packaging self-check, including current
  and legacy config validation, upgrade preservation, and uninstall inventory.
- Changed-file Markdown lint and `git diff --check`.

Tests cover config defaults, explicit flag precedence, invalid values, active
bandwidth limits, and cancellable timetable updates. The running deployment
was not upgraded, and eight-stream throughput has not been benchmarked against
Degoo. Parallel read ranges can exceed the configured extra read-ahead amount.

## Upload timing and registration recovery

The 2026-10-11 upload-delay investigation found two registration failures in
the installed mount log, followed by VFS retry delays. Its settings already
disabled upload readback verification. The five-second default close delay
alone did not explain the reported interval.

The follow-up validates positive, finite write-back values, preserves the
five-second default for legacy settings, and reports that default in inventory.
Offline regressions cover the added wizard prompt, setting validation and flag
precedence, delayed registration visibility, negative acknowledgements,
blocked requests, and provider `Retry-After` delays. Unconfirmed writes remain
pending; an acknowledgement is never substituted for metadata and publication
checks.

The full `make quicktest BRANCH=degoo-upload-review` passed with the current
test binary and contained FUSE helper first on `PATH`. An initial run without
those prerequisites failed in archive, mount, and Docker mount tests; the
affected packages and then the full suite passed after correcting `PATH`.
The full Python helper suite passed all 136 tests. The final binary build,
`go test -race ./cmd/degoo/... ./backend/degoo/...`, scoped `go vet`, and scoped
golangci-lint v2.14.0 passed with zero lint issues. Changed Markdown lint and
`git diff --check` passed.

Live probes used only generated encrypted data in a separate disposable test
folder. Two direct API uploads registered in about one second, became visible
within another second, and matched encrypted readback hashes. A complete
backend upload then received a false registration acknowledgement and could
not confirm its temporary object. This remains a live provider failure, not a
validated latency improvement. Deletion also returned false, so cleanup of the
generated test folder is unconfirmed; private local recovery state was retained.
No customer file or installed mount configuration was changed.

## Unified `degoo-rc` entrypoint follow-up

The current frontend and packaging checks for the unified command passed:

- All 5 actual CLI tests passed.
- The full Python helper suite passed all 128 tests: 67 install/uninstall,
  56 info/wizard/logs, and 5 CLI tests. Coverage includes actual PATH lookup,
  paths with spaces, and fresh installs without root-level shell wrappers.
- The package self-check passed, including help output for every packaged
  command. Its follow-up assertion requires the archive root to contain only
  `README.md` and `degoo-rc`; it failed on the previous wrapper layout and
  passed after the root wrappers were removed.
- Building the archive with `build/rclone` and checking it from isolated homes
  passed, including native CLI help for every command.
- `sh -n` passed for all entrypoints and helper scripts.

These checks do not change the live Degoo acceptance gates below. Larger live
files, long-term token renewal, pagination beyond 1,000 entries, and interrupted
multi-chunk recovery remain unverified.

## Installer and cleanup wizards

The wizard changes were checked offline on 2026-10-10:

- `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover
  -s contrib/degoo/scripts/python -p 'test_*.py'`: all 63 tests passed.
  Coverage includes default answers with real local installation and setup,
  failed login/setup, directory selection, upgrades from the old script layout,
  retention of pending writes, and refusal to delete after a backup failure.
- `go build` passed. Shell syntax and launcher help checks passed.
- The packaging self-check, Markdown lint, and `git diff --check` passed.
- The Linux amd64 archive was extracted into a temporary home. Its launchers,
  staged installation with the actual built binary, inventory, and local
  cleanup passed without touching the user's installation.
- A full `make quicktest` run failed in mount and Docker mount tests because
  `fusermount3` was absent. The archive test found an incompatible external
  `rclone`; it passed with the freshly built executable first on `PATH`.
  The RC version test rejected the branch slash in the Makefile-generated
  version; `RCLONE_CONFIG=/notfound go test ./fs/rc` passed without that version
  override. The remaining packages passed in the full run.

These checks did not install system packages, use account credentials, or
create a remote vault. Package-manager commands and provider responses were
mocked. Live wizard login, package installation, and FUSE mounting remain
environment-dependent checks.

## Distribution layout repair

The previous handoff archive was rebuilt from an older checkout without the
wizard launchers. Its extracted folder therefore had `INSTALL.txt`, a
top-level binary, and source-shaped `contrib/` and `backend/` directories, but
no unified installer entrypoint. The repair starts from the current `staging`
implementation and makes the archive layout independent of the source layout.
The current package root contains:

- Root: `README.md` and executable `degoo-rc`.
- `bin/rclone`, `scripts/python/`, flat `docs/`, and `licenses/`.
- Internal shell helpers, including `scripts/trash.sh`, remain under `scripts/`.
- `docs/BUILD.json` records the source revision, dirty state, and binary hash.
  A `.tar.gz.sha256` sidecar verifies the archive bytes.

The wizard discovers both packaged `bin/rclone` and a source build at
`build/rclone`. Both regression tests failed before the fix; all 65 installer
and cleanup tests passed afterward. A fresh `go build -o build/rclone .`
passed. Archive checks extract the package before running entrypoint help;
missing `degoo-rc` prevents packaging, and the packaging self-check rejects
unexpected root-level commands.
Install examples use fresh extraction directories to prevent stale files
from surviving an overlay extraction.

The repaired Linux amd64 archive passed a second extracted install, inventory,
and local cleanup with the actual binary in a temporary home. Automatic
binary discovery and all 18 relative Markdown links passed. Changed Markdown
files passed lint. Packaging self-checks verified deterministic archive and
checksum bytes, preservation of an existing archive after a rejected build,
and rejection of outputs that would replace package sources or special files.
The production archive and checksum also passed verification. These checks
performed no account login, remote changes, system package installation, or
live mounting.

## Complete uninstall and fresh installation

The cleanup follow-up on 2026-10-10 exercised the actual installed client:

- The user chose to delete all local state without a retention backup.
  Pending uploads were zero. The initial uninstall exposed that
  `systemctl disable --now` removes a linked unit before trying to stop it,
  then can fail with the unit missing. Links were gone but the install root
  remained. A retry completed deletion; a final user-manager reload confirmed
  `LoadState=not-found`, `ActiveState=inactive`, and an empty `FragmentPath`.
- The complete original install root, including vault/account configuration,
  cache, binaries, scripts, and private logs, was absent. Both service links
  and the kernel mount were absent, no contained rclone process remained,
  and no retention directory was created. Remote files were not modified.
- The fix stops the owned unit before disabling it, handles recorded links
  already removed by a partial teardown, removes exact owned unrecorded
  links, and reloads after link removal. It refuses final root deletion if
  the unit is still loaded, active, or unresolved. Reload failure leaves the
  root and mount directory available for retry.
- A generated temporary client was installed in the real contained root and
  enabled with the real user manager. Its mount command ran only `sleep`,
  with no FUSE mount, account credentials, or network operations. Corrected
  uninstall removed that active service and the entire fixture; independent
  checks again showed the unit not-found/inactive and both links absent.
- All 116 offline Python tests, the package self-check, `go build`,
  `make quicktest BRANCH=degoo-uninstall-handoff`, changed Markdown lint,
  and `git diff --check` passed.
- Wizard tests cover explaining retention before prompts, retrying the `N`
  path entry, no-backup removal, and accurate retention/service summaries.
  The backup is created only when at least one keep option is selected.
- The extracted distributable passed an offline staged installation, status,
  log viewer, inventory, and complete uninstall with a temporary home.
  All relative Markdown links in the archive resolved to packaged files.

The distributable and source installation instructions include post-cleanup
checks and extraction into a new directory for a fresh wizard run. Shared
system dependencies, user lingering, parent systemd directories, and historical
entries in the shared journal remain outside the contained install footprint.

## Upgrade preflight and mount readiness

The follow-up installer checks on 2026-10-10 address the live transcript where
an upgrade failed after collecting settings and a successful service start
briefly appeared as "not mounted":

- Upgrade preflight asks to stop the contained service or manual mount before
  collecting settings. Ownership validation prevents stopping another unit,
  and the kernel mount must be absent before upgrading.
- Startup waits up to 15 seconds for the kernel mount. An unconfirmed mount
  produces diagnostics and a nonzero wizard result. Active systemd state
  alone is insufficient to report a ready mount.
- Summary examples have shell comment labels and executable commands on
  separate lines, including service stop, manual unmount, and the log viewer.
- The mount launcher defaults to NOTICE; later CLI flags can request INFO.
  Historical log entries are preserved.
- A live stop exposed a disconnected FUSE mount that `mountpoint` could not
  inspect (exit 1). The old stop helper incorrectly treated that error as an
  absent mount (exit 32). The helper now distinguishes these cases. Recovery
  detachment requires a disconnected endpoint, no active cache writer, and
  user acceptance; connected busy mounts are not detached automatically.
- All 104 offline Python tests passed, along with the package self-check,
  `go build`, `make quicktest BRANCH=degoo-upgrade-startup`, changed Markdown
  lint, and `git diff --check`.
- A repeated live upgrade using the new preflight and readiness helpers
  preserved all five configuration files, cache file metadata, and existing
  log contents. The restarted mount was connected and the service active,
  with zero pending uploads. The log viewer succeeded; routine cache polling
  appended no messages over a six-second check at the default NOTICE level.
- The built binary accepted a trailing INFO override after NOTICE and emitted
  the expected local-copy INFO message. The reverse order suppressed it.
  This used generated temporary local files, with no cloud requests.
- The extracted distributable passed a staged install with its actual binary,
  NOTICE wrapper inspection, local status and logs, inventory, local uninstall,
  and all 18 relative Markdown links in a temporary home.

## Wizard completion and diagnostics

The completion handoff and diagnostics changes were checked on 2026-10-10:

- The wizard summary appears after service enable, manual setup, missing mount
  dependencies, and partial setup failures. It reports the actual mount table
  state and local service state rather than treating a start request as mount
  readiness. Regression tests cover these paths and shell-quoted commands.
- A PTY regression and a built-binary terminal probe with `--log-file` showed
  the password prompt in the terminal with input echo disabled. The generated
  test password was absent from terminal output and the log. The probe used
  a localhost-only endpoint, rather than logging into a real account.
- Log and status tests cover bounded tails, private regular files, recognized
  credential/URL redaction, follow mode before log creation, explicit recent
  authentication rejection, expired access tokens with/without refresh state,
  and suppression of old failures after account state is updated.
- All 87 offline Python installer/cleanup tests and the packaging self-check
  passed. Changed Markdown lint and `git diff --check` passed.
- `make quicktest BRANCH=degoo-installer-handoff` passed with the built rclone
  and FUSE helper first on `PATH`. The `cmd/degoo` race test and scoped
  golangci-lint v2.14.0 passed with zero lint issues.
- The rebuilt archive passed staged installation, diagnostics, simulated
  authentication failure/recovery, summary output, all 18 relative Markdown
  links, inventory, local cleanup, and checksum verification in a temporary
  home.
- A live upgrade preserved all five account/vault/mount configuration files
  and the cache, and the owned user service restarted with the mount present
  and zero pending uploads. It found legacy wizard log files with mode `0640`,
  which the private log viewer correctly refused. Upgrade now normalizes
  known owned log files without changing their contents; direct wizard
  subprocesses create logs with a private umask. The repeated live upgrade
  preserved the configuration, cache, and all five existing log files;
  `logs.sh` then succeeded, and the restarted mount was present and active.

No account password was collected by the agent, and no customer file was
created, read through the mount, or deleted for these checks. The real service
restart may perform normal metadata requests and token renewal. Rejected
renewal visibility was tested with generated log/authentication fixtures;
long-term live renewal remains unverified.

## Offline checks

The completed offline checks use HTTP fixtures for Degoo and local storage for
the encrypted vault:

- `go build`: passed.
- The initial `make quicktest BRANCH=degoo-vault` passed with the built binary
  and FUSE helper on PATH. The follow-up
  `make quicktest BRANCH=degoo-live-setup` passed after the transient failure
  described below was rerun and the full suite was repeated.
- `RCLONE_CONFIG=/notfound go test -race -count=1 ./backend/degoo/... ./cmd/degoo`:
  passed after the final nested-directory change.
- golangci-lint for `backend/degoo/...`, `backend/chunker`, and `cmd/degoo`
  passed with zero issues, including the final Degoo changes. Markdown lint
  and `git diff --check` passed.
- `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest -v contrib.degoo.test_uninstall`:
  passed all 14 offline cleanup tests.
- The native Linux FUSE smoke test passed with real mount operations, encrypted
  local storage, overwrites, seeks, truncation, rename, deletion, and remount.
- The opt-in 21 GiB logical-file test passed a full encrypted round trip and
  SHA-256 comparison, plus reads above 20 GiB and across chunk boundaries.
  It generated data as a stream and checked encrypted chunk names, headers,
  count, and object-size limits.

An earlier full quicktest follow-up failed only in
`cmd/serve/s3/TestPutObjectFailureCached/minimal/Memory`: its 10-second wait did
not see `test-60/cached.txt` appear on the in-memory backing filesystem, and
the log showed the related VFS cache file missing. The exact subtest passed
when rerun alone, and all four `TestPutObjectFailureCached` subtests passed in
a scoped rerun. This appears transient under full-suite load; the cause is not
confirmed. The subsequent full-suite rerun passed.

The HTTP tests cover pagination beyond 1,000 entries, token refresh,
credential-error redaction, refusal of token-bearing redirects, rejection of
mutation replay after partial GraphQL success, strict range responses,
multipart upload,
registration ambiguity, duplicate names, root confinement, and preservation
of the prior object when replacement fails. Vault tests cover encrypted
contents/names/manifests, wrong secrets, corruption, missing chunks, exclusive
cache use, identity changes, and dirty-cache recovery after graceful restart
and abrupt subprocess exit. Chunker regressions cover failed manifest
publication and rejection of manifests whose transaction chunks are missing.

## Account trash

The account-trash commands and source wrapper are not live-validated. No live
Degoo API request or destructive mutation was run for this work. Required
release checks are:

- Offline command tests must cover pagination, the all-items default for
  `--limit 0`, positive limits, JSON output, and empty trash listings.
- `empty-trash --dry-run` must list all items and make no mutation request.
  The mutating path must send every listed ID in a bulk `SetDeleteFile5`
  request with `IsInRecycleBin=true`, and must prompt exactly twice without
  `--yes`; `--yes` must bypass both prompts.
- The generic rclone cleanup feature and remote-path syntax must remain
  unavailable. No restore operation is provided.
- The wrapper must reject unsupported flag combinations, map `list`/`empty`
  to `trash`/`empty-trash`, choose or accept a safe rclone executable, and
  never read or print auth-file contents.
- Any live `empty-trash` acceptance must use a disposable test account
  containing only generated trash. Because trash is account-wide, it must not
  be run against an account with personal or unrelated trash items.

Local wrapper syntax, help, argument-forwarding, package self-check, Go
tests, Go build, Go vet, and changed-file Markdown linting passed for this
branch. They do not substitute for live trash listing or emptying against a
disposable account.

## Reproduce the opt-in checks

```sh
RCLONE_CONFIG=/notfound go test ./cmd/degoo \
  -run '^TestVaultNativeMount$' -degoo-fuse-test -count=1 -timeout2m
RCLONE_CONFIG=/notfound go test ./cmd/degoo \
  -run '^TestVaultLargeLogicalFile$' -degoo-large-test -count=1 -timeout15m
```

The native test requires `/dev/fuse` and a `fusermount3` helper. This machine
has FUSE 2.9.9's `/bin/fusermount`; validation used a temporary PATH-only
`fusermount3` symlink to that helper. No system packages were changed. The
large-file test needs more than 21 GiB of free temporary storage and took
about five minutes on this machine.

For the full offline suite, put the freshly built `rclone` on PATH. The
Makefile embeds the branch name in a semantic version; branches containing
`/` need a valid label override:

```sh
PATH="$PWD:$PATH" make quicktest BRANCH=degoo-vault
```

## Live gates

The current live probes established these provider behaviors using the
dedicated test folder:

- Account authentication was discovered and exercised without recording the
  account password in the vault configuration.
- Folder creation under the account's device root succeeded for the dedicated
  test folder. Creating under account root ID `0` was rejected, so the backend
  must keep using the explicit dedicated folder parent.
- Live responses use RFC3339 timestamp strings and boolean mutation
  acknowledgements.
- A newly created encrypted-name directory placeholder was observed as a
  zero-size `Cat0`, not `Cat6`. The original placeholder remains in its
  parent; only its internal child marker moves into the returned `newCat2`
  folder.
- A generated 2 MiB logical file was fully published as encrypted chunks and
  an encrypted manifest; the pending-upload count reached zero. After
  unmounting, the old cache was moved aside, and the vault was remounted with
  a new empty private cache. A seek to 1 MiB + 123 followed by a 65,536-byte
  read succeeded, and a full readback SHA-256 matched the generated source.
  This verifies live Degoo CDN range reads and logical-file readback after
  remount. The redundant old cache was removed after this proof.
- Empty-file, overwrite, truncate, and rename operations passed readback after
  a fresh-cache remount. Generated empty and lifecycle-test files were then
  deleted, and a fresh mount's root listing verified their removal. This was
  a smoke check, not a full repeated lifecycle matrix.
- A rename probe exposed a routing edge case: the parent check for the final
  rename upload was true before the operation and false afterward when reusing
  the original same-name `Cat0` ghost. The backend now retires verified-empty
  ghosts automatically, and its tests pass; it does not generally delete
  non-empty ghosts.
- Live cleanup required recycling active items before hard deletion, then
  waiting for recycle visibility. The retired setup ghost and 23 failed
  generated uploads were recycled and successfully hard-deleted.

Nested directory creation, fresh-cache child-file readback, directory rename,
file deletion, and empty-directory removal passed. A final fresh-cache remount
confirmed an empty vault and zero pending uploads. Category 0 placeholder
handling requires a newly acknowledged create and no pre-existing entry.
The uninstall inventory passed against the installation; destructive uninstall
was tested with offline fixtures and was not run on the active installation.

The live access token had a one-hour lifetime. After expiry, the saved refresh
token was rejected with HTTP 403. The status does not establish whether the
refresh token expired, was revoked, or endpoint access was forbidden. The auth
client now reports this safely and can adopt fresh tokens saved by interactive
login. After fresh interactive login, an explicit renewal request succeeded
and saved new private authentication state. A later metadata lookup renewed
the saved token after its real expiry and completed successfully. Successful
long-term unattended operation across repeated renewal cycles remains unverified.

The live provider limit above 20 GiB, pagination beyond 1,000 entries, and
failure recovery under Degoo's consistency model remain unverified. Keep all live
writes inside the dedicated test folder, and do not claim release acceptance
until the remaining gates pass.

The contained local installation and local-only uninstall procedure are
documented in [INSTALLATION.md](INSTALLATION.md) and
[contrib/degoo/README.md](../../contrib/degoo/README.md). Uninstall preserves
remote encrypted objects deliberately.

## Contained installer and persistent mount follow-up

The follow-up adds cached-email login, private JSON mount settings, optional
upload readback verification, setup/location commands, and a systemd user
service. Validation completed:

- `make quicktest BRANCH=degoo-persistent-install` passed with the freshly built
  binary and contained FUSE helper on PATH. Focused race tests and the native
  Linux FUSE test passed. The subsequent prompt-text and lint-only edits passed
  focused command tests.
- Degoo backend/auth/API and command lint passed with zero issues. Markdown
  lint and `git diff --check` passed.
- All 33 offline Python installer/login/setup/service/uninstall tests passed,
  including preservation of secrets, cache and disabled verification during
  upgrade; cached-email login without a second email prompt; service-link
  ownership; and pending-upload refusal before service shutdown.
- Systemd unit verification passed. On the real host, the user service was
  enabled and exercised through stop, start, and restart. Older systemd reports
  the unit's owned symlink as `FragmentPath`; validation accepts that recorded
  link only when it targets the contained service file.
- The live `create-vault` command created a uniquely named test folder beneath
  the device container. Its ID was verified before empty-folder removal.
- A generated 128 KiB file was written through the persistent mount with full
  verification enabled, uploaded to zero pending files, and read back. An
  overwrite with verification disabled also uploaded and matched readback
  after remount. This latter read can use VFS cache and is not an independent
  cloud integrity audit. The generated file was deleted and its absence
  confirmed after another remount. Verification was restored to true.
- The active installation's read-only uninstall inventory passed and recorded
  both owned external service links. Destructive uninstall remains validated
  with fixtures and was not run on the user's live vault.
- The distributable packager self-check passed. Its explicit source allowlist
  excludes account tokens, vault configurations, cached data, and runtime logs.

These checks establish the contained installation and persistence behavior;
they do not remove the long-term renewal and remaining live acceptance gates.

## Information report follow-up

The contained `info.sh` reports local mount/service state, private config paths
and their roles, saved cache settings, pending writes, token expiry hints, and
bounded disk/cache allocation estimates. The default report excludes the cloud
mount and does not contact Degoo. An optional `--check-remote` checks only vault
ancestry, suppresses raw provider errors, and may renew authentication.

- All 44 offline information-report, installer, and uninstall tests passed.
- The installed local report completed in under 0.1 seconds on the live host.
  Mount and service were active, lingering was enabled, and pending uploads
  were zero. Cache allocation and install allocation are reported separately.
- The metadata-only remote check confirmed the configured vault location.
  A saved access token that had genuinely expired was renewed successfully.
- The updated secret-free Linux archive passed its packager self-check and an
  offline install, local report, and complete uninstall using the real binary
  under a temporary home. The archive includes the backend license notice.

## Mounted benchmark command

Offline checks on 2026-10-11 passed:

- All 263 Python helper tests passed, including 13 benchmark tests with actual
  small local `dd` reads and writes. Upload state and mount validation were
  mocked; tests cover exact sizes, cleanup, retention, failed I/O, bounded
  upload waiting, unsafe paths, and concurrent changes preventing deletion.
- The package self-check passed with `benchmark.py` included in the archive
  and installer. Benchmark discovery and both subcommand help pages passed.
- Changed Markdown lint, shell alias syntax, and `git diff --check` passed.
- The local installation received the benchmark helper and command dispatch;
  its help pages passed. The superseded shell benchmark definitions were
  removed from the local aliases file.

No live cloud benchmark was run. These checks establish command behavior and
packaging, not cloud throughput or a cold-cache download. VFS upload state is
used for write completion without an independent remote integrity audit.

## Immutable mount helper review correction

The historical startup and mount-protection records above describe the earlier
implementation. The recorded live recovery mounted only after the immutable
flag was cleared. The native FUSE smoke test used a temporary mutable
mountpoint; neither result established that a normal FUSE helper can mount an
immutable directory. Owner-writable mode `0700` is not enough: Linux rejects
the required write-access check for an immutable directory, and standard
`fusermount3` refuses the mount.

This review adds a manually built, root-owned setuid helper at
`/usr/local/libexec/rclone-degoo/fusermount3`. It is restricted to an empty
`/mnt/degoo` owned by the caller and not writable by group or other users; the
default directory remains owner-private. It uses the caller's user ID for the
kernel mount, preserves the immutable flag, and leaves rclone running as the
ordinary user. It rejects `allow_other` and `allow_root`. The immutable legacy
in-install `mount/` path is not supported; migrate to `/mnt/degoo` to use the
helper. The [helper README](../../contrib/degoo/immutable-mount/README.md) has
the manual build and installation commands. After installation, run
`degoo-rc service enable` to retry setup. For an already enabled service, run
`degoo-rc service restart`.

Native retained-flag acceptance passed on Linux:

- `bin/test-degoo-immutable-mount.sh` compiled the helper and test binary, then
  used a private mount namespace with a generated caller-owned immutable
  `/mnt/degoo` and a root-installed helper isolated to that namespace.
- An ordinary UID 1000 client wrote and read encrypted `localStack` data,
  unmounted and remounted, and read the same data again. `FS_IOC_GETFLAGS`
  through a retained directory descriptor showed the immutable flag before,
  during, and after mounting. An unmounted `os.WriteFile` failed with `EPERM`.
- A closed FUSE communication peer after kernel mount verified error cleanup:
  the helper unmounted the failed mount, retained the immutable flag, and
  unmounted writes still failed with `EPERM`.
- The Go integration test also passed:

  ```sh
  go test ./cmd/degoo -run '^TestMountpointRealImmutableFlagRetained$' \
    -degoo-immutable-test -count=1
  ```

These checks used generated local data and did not contact Degoo or customer
files. The helper was installed only inside the private test namespace; no
host-level helper, mount, or system configuration was changed. This verifies
the local Linux mount and cleanup behavior, not live provider compatibility.

Additional offline checks passed:

- All 262 Python installer and helper tests passed.
- `go vet ./cmd/degoo` and the full `go build` passed.
- The helper C compilation and parser checks passed.
- `python3 bin/package-degoo.py --self-check` passed.

The first full-suite attempt used the host-installed `rclone`, which lacked a
test command expected by the suite, and did not have the FUSE helper alias on
`PATH`. After preparing a directory with the freshly built `rclone` and a
`fusermount3` symlink to `/usr/bin/fusermount`, the targeted archive, Docker,
and S3 packages passed. The full credential-free suite then passed with exit
status 0:

```sh
PATH=/tmp/degoo-review-tools:$PATH \
  RCLONE_CONFIG=/notfound go test ./...
```

`golangci-lint` was unavailable, so Go lint was not run.

## Wizard prerequisite preflight — validated 2026-10-11

The wizard lists runtime prerequisites and checks the configured mountpoint
before offering service startup. A known blocker prevents service enablement
and startup, returns a nonzero result, and preserves the installed client,
vault configuration, and local cache. A valid immutable-mount helper does not
require GCC; the wizard checks and reports compiler and source prerequisites
when helper installation is needed. The wizard does not clear immutable flags
or install or change privileged helper files automatically.

Offline validation passed:

- All 288 Python installer and helper tests passed, including 69 wizard tests
  and 18 mount-prerequisite module tests. They cover actual flag-reading calls,
  missing and misconfigured helpers, unsafe paths, `nosuid`/`noexec` mounts,
  compiler/source prerequisites, preserved local files, owner read/execute
  access, and foreground recovery without systemd.
- The full credential-free Go suite passed with exit status 0:

  ```sh
  PATH=/tmp/degoo-review-tools:$PATH RCLONE_CONFIG=/notfound go test ./...
  ```

- The full binary build to `/tmp/rclone-degoo-prerequisites-check` passed.
- `python3 bin/package-degoo.py --self-check` passed. Python compilation and
  shell syntax checks passed for the setup block and native test driver.
- The extended native namespace driver copied the prerequisite checker into
  its isolated test setup and ran it as an ordinary user. It reported a
  caller-owned mode-0700 immutable `/mnt/degoo` with a valid root-owned,
  mode-4755 helper as ready. Encrypted local write/read, unmount/remount, and
  failed-send cleanup all passed while the immutable flag remained set.
- A read-only SSH check on the user-authorized target server correctly
  reported the helper missing. It made no service, client, helper, or
  configuration changes there. No Degoo cloud access was involved in these
  checks.
- The six changed Degoo Markdown files passed scoped markdownlint, and
  `git diff --check` passed.

## Privileged immutable mount setup — 2026-10-11

The wizard replaces its compiler and root install block with one explicit
`sudo .../degoo-rc mount-setup` command, followed by clearly separated
ordinary-user service or foreground-mount commands. The setup command builds
as the sudo caller and installs only the fixed root-owned helper atomically.
A valid helper is reusable without a compiler. Configuration, cache data,
and the mountpoint's immutable flag are preserved.

Validation completed:

- All 309 contained Python tests passed. The setup tests cover sudo identity,
  safe destination metadata, `nosuid`/`noexec`, missing compiler instructions,
  real unprivileged GCC compilation, idempotence, shell quoting, refused
  privilege changes, symlink preservation, and atomic write failure recovery.
- The full credential-free `go test ./...` suite and a complete `go build`
  passed. The host's FUSE 2 executable was exposed as `fusermount3` for the
  existing mount tests; a locally built rclone was also available on `PATH`.
- `bin/package-degoo.py --self-check` passed, and the complete binary was
  packaged into an archive with launcher validation and a checksum sidecar.
- `contrib/degoo/immutable-mount/test.sh` passed its C parser and helper checks.
- `bin/test-degoo-immutable-mount.sh` passed in a private mount namespace.
  The real `degoo-rc mount-setup` launcher worked with `HOME=/root`, compiled
  as the calling account, installed a root:root mode-4755 helper, and succeeded
  again without rebuilding. It repaired a helper symlink while preserving
  its target and repaired an incorrectly owned world-writable helper file.
  Generated encrypted data was written, read, unmounted, and read after
  remounting while the underlying directory retained its immutable flag.
  Unmounted writes and failed descriptor-send cleanup were also checked.
- Python compilation, shell syntax, seven-file Markdown lint, and
  `git diff --check` passed. Historical validation entries remain unchanged.

These checks use disposable local encrypted data. They do not validate live
Degoo behavior or install the helper on another server. `golangci-lint` was
unavailable on the validation host.

## Encrypted account-password fallback and credential-free backups (2026-10-12)

The account-password fallback uses the vault crypt data key, including the
optional password salt. HTTP fixtures verify rejected renewal followed by
password login, one-shot password submissions and token exchange, persistent
cooldowns, provider `Retry-After`, shared account locking, cancellation, and
login/logout serialization. Credential tests cover ciphertext tampering, a
wrong key, clearing saved credentials, and preservation of vault fields.
Command and backend tests exercise the complete fallback path. Cache identity
tests ensure that changing account credentials does not detach pending writes.

Configuration backups and uninstall-retention backups omit account passwords,
their encrypted form, tokens, and account lock files. Tests verify that the
installed credentials are preserved and that retained pending cache data
remains recoverable after restoring and logging in again.
Legacy-backup and direct-vault import tests verify that saved account
credentials are stripped, old tokens are not restored, and the wizard requires
a fresh login while leaving the source backup intact.

Validation:

- Full Go build passed.
- The credential-free Go suite passed with only `TestMount`,
  `TestDockerPluginMountTCP`, and `TestDockerPluginMountUnix` excluded because
  standard `fusermount3` is unavailable. The initial unfiltered run confirmed
  those environment failures. An archive test passed after selecting the
  freshly built binary; a transient S3 cache timing failure passed on rerun.
- Uncached Degoo backend and command tests passed. Auth and command race tests
  passed; scoped Degoo backend/auth/API/command lint reported zero issues.
- All 314 offline Python tests passed, including backup, restore, and
  uninstall recovery tests. Changed Markdown lint and `git diff --check`
  passed.

This does not establish live password fallback or multi-day provider renewal.
An existing installation needs an interactive login to populate its encrypted
credential. The idle supervisor in [SESSION_RECOVERY.md](SESSION_RECOVERY.md)
is a proposal and is not implemented by this change.

The local client was upgraded with its existing configuration and cache
contents verified unchanged. Its service remains stopped pending the first
interactive login with the updated client.

## Benchmark cleanup after writeback timestamp changes

Offline checks on 2026-10-12 passed:

- A generated 8 MiB write through a native Linux FUSE mount reproduced the
  cleanup refusal with the benchmark from `staging`. The backing stack was
  chunker over crypt over a temporary local directory, with 1 MiB chunks,
  `norename` transactions, full VFS caching, a two-second writeback delay,
  upload-status attributes, and a 100 ms attribute timeout. Device, inode,
  and logical size stayed unchanged; the post-upload modification time differed
  from the pre-close stat. The fixed benchmark completed, verified zero data,
  and deleted the file. This check contacted no Degoo endpoint.
- All 325 Python helper tests passed, including 24 benchmark tests. Regression
  cases cover timestamp-only drift with deletion and retention, same-size
  edits, replacements, read timestamp changes, partial verification blocks,
  nonzero tail data, premature EOF, changes during verification, symlinks,
  and timeout preservation, including expiry during the final read.
- `go build` and the full offline `make quicktest BRANCH=degoo-benchmark`
  passed. The newly built executable was available on a temporary PATH; the
  host's FUSE 2 helper was exposed there as `fusermount3` for existing tests.
- The package self-check, changed-document Markdown lint, and
  `git diff --check` passed.

The conditional mounted content check happens after sampling write throughput
and uses the original total deadline. It confirms generated data through the
mounted view; it is not an independent cloud integrity audit. No live Degoo
throughput benchmark or modification of the user's installed mount was made.

## Wizard password recovery and installed benchmark helper (2026-10-13)

Three regression cases reproduced missing saved account credentials after
normal wizard setup: an early direct vault import, a late existing-folder
import, and a new vault. Early imports are now installed before account login;
new vaults and late imports require a final vault-aware login when credentials
are missing. Tests cover preserving imported encryption settings, ignoring
inherited account credentials, avoiding an extra login on configured upgrades,
and preserving vault/cache bytes without startup when final login fails.

- All 329 offline Python helper tests passed, including 73 wizard tests.
- The package self-check, changed Markdown lint, and `git diff --check` passed.
- The installed benchmark helper was older than the merged timestamp fix.
  It was updated to that source, and all 24 benchmark tests passed against the
  installed copy using disposable local files and simulated upload state.
- The validated wizard helper was installed locally. Configuration contents
  and the running mount process were verified unchanged. The original 2 GiB
  benchmark file was preserved; its VFS upload state reported `uploaded`.

These changes affect Python helpers and documentation only. No live cloud
benchmark or fresh installation over the user's vault was run. The upload
state is a local VFS observation, not an independent remote integrity check.

## API cooldown, listing cache, and writeback retry validation — 2026-10-14

The follow-up source changes passed the final validation available for this
run:

- Build passed. Degoo auth, API, backend, command, and writeback unit tests,
  scoped race tests, `go vet`, and scoped lint passed with no issues.
- The final credential-free `go test ./...` passed after a small
  rate-limit-persistence error-path fix. The command used the freshly built
  rclone first on `PATH` and private XFS-backed temporary directories for both
  `TMPDIR` and `GOTMPDIR`.
- The first full-suite attempt used the host-installed rclone and default
  tmpfs-backed temporary directories. The host binary lacked a command used
  by tests, tmpfs did not support required xattrs, and a non-private
  socket-parent path failed the command's safety check. These were environment
  setup failures; the corrected final run passed.
- Windows API/auth cross-compilation passed. This verifies compilation only;
  the non-Unix fallback does not provide the Linux cross-process `flock`
  behavior.
- API tests cover the five-second `GetFileChildren5` snapshot, 32-parent
  bound, expiry, explicit and mutation invalidation, and generation protection
  against stale in-flight results. Cooldown tests cover preinitialized clients
  re-reading the sidecar, keeping the longest concurrent deadline when a
  shorter 429 races with it, rejecting unsafe or symlink lock files, avoiding
  a token refresh after an in-flight 401 arrives during another process's
  cooldown, and returning the longest deadline when persistence fails.
- Metadata lookup tests verify a ten-second total deadline across all listing
  pages and retries, and that caller cancellation remains effective. A
  paginated fixture blocks its second page and confirms both pages share the
  same lookup deadline.
- Writeback tests verify that wrapped `Retry-After` deadlines schedule pending
  writes in the VFS queue, prevent an early retry, and preserve ordinary
  backoff behavior after an expired deadline.

A rebuilt native Linux FUSE smoke test against a localhost HTTP fixture passed.
A matching root-validation proof allowed the mount to initialize while a
persisted cooldown was active, with zero HTTP requests sent during the hold. A
generated 47-byte dirty write remained pending. Native `ls` received `EIO` in
about 3 ms instead of hanging or reporting an empty remote. After the short
fixture cooldown expired, reading the generated data returned the expected
bytes. Rejected upload authorization preserved the pending write, and normal
unmount completed. This verifies local fail-fast and retention behavior; it
does not exercise Degoo's live service.

The captured live `GetFileChildren5` 429 requested a retry after
`2026-10-14 17:06:16 EEST`; the already-running mount retried until it was
stopped. The follow-up validation made no live upload-recovery claim. Live
provider upload recovery with the current code remains unverified.

### Production cooldown and external-cache migration — 2026-10-14

The captured `GetFileChildren5` response requested retry after `2026-10-14
17:06:16 EEST`; the persisted deadline was conservatively rounded to `17:06:17
EEST`. While the production mount was under that hold, native `ls` returned
`EIO` in 0.0029 seconds. One process-level INET-socket inventory showed zero
sockets, but this snapshot is not packet-level proof that no request was sent
over the entire cooldown. Startup logs contained 941 retry messages scheduling
the next attempts about 16 hours 49 minutes later. These messages describe
local scheduling, not requests reaching Degoo.

The prior mount became stuck and required exceptional recovery. The mount
process was paused and the cache filesystem was flushed; there were no active
writer file descriptors, and the remaining cached-data descriptors were read-
only. The stalled FUSE connection was aborted, then normal unmount and service
stop completed. This was a one-time recovery after confirming there were no
active writers, not a routine cache-migration step. Live Degoo upload recovery
after the recorded deadline remains unverified and is still an acceptance
gate.

The external cache migration is complete. SHA-256 comparison confirmed all
1,886 cache files, totaling 258.013 GiB, matched source and destination before
repair. Two dirty files truncated by `ENOSPC` during the original copy into
the mounted vault were restored from stable originals; their complete SHA-256
values matched. After repair, all 941 physical cache data files were marked
dirty and had complete ranges. The cache was bind-mounted from
`/data.tr/degoo-cache` onto the unchanged recorded `cache_dir`, preserving
cache identity and pending writes. The source directory is mode `0700`; the
empty underlying target is mode `0500`, and the user-service drop-in uses
`ConditionPathIsMountPoint` on the saved cache path. The external filesystem
must be mounted before the service starts.

After full copy verification, restoration, and bind-mount checks, the old local
cache was removed. Root storage recovered approximately 258 GiB and now has
264 GiB available (41% used); the external data volume has 142 GiB available. The
service remained active and unchanged. Its status showed 941 queued and
retrying uploads, zero uploading, and 277,364,333,920 pending bytes. This is
local queue status, not proof of remote completion.

## Request reduction, batching, and encrypted small-object cache — 2026-10-14

The new API, backend, persistent-cache, and command tests cover the in-memory
listing cache bounds and expiry; same-parent coalescing; independent waiter
cancellation; fresh-list bypass and coalescing; scoped invalidation and stale
parent-hint protection; authorization response cardinality and per-item
errors; mixed-parent registration batching; safe aggregate request counters;
and persistent-cache versioning, integrity checks, bounds, unsafe paths, and
clear behavior. The generated encrypted chunk-stack regression changes its
configured chunk size from 1 KiB to 8 KiB and reads both the complete object
and a range crossing the old chunk boundary.

Scoped API/backend/cache unit tests and race tests passed. The credential-free
repository-wide `go test ./...` passed. The final
`go test -race ./cmd/degoo -count=1`,
`go vet ./backend/degoo/... ./cmd/degoo/...`, and
`golangci-lint run ./backend/degoo/... ./cmd/degoo/...` also passed. A first
command race run used a Unix-socket test path longer than the platform limit;
rerunning with a shorter temporary directory passed.
All four updated Markdown documents passed `markdownlint-cli2` and
`git diff --check`.

A native Linux FUSE test against a localhost fixture passed without contacting
Degoo. Two generated pending files shared one write-authorization request;
polling the local stats endpoint caused no fixture API request during its
cooldown; a denied authorization left both writes pending; and normal unmount
detached the mount. This checks local batching, counters, retention, and
shutdown behavior only. No authenticated Degoo request, production upload, or
destructive customer-data test was part of this follow-up. Live provider upload
recovery remains an acceptance gate.

The clean build of source commit `e008796360b6b3dd1a68e17537946e150d956fde`
was then installed on the existing production mount through the private
maintenance endpoint, normal FUSE unmount, atomic binary/manifest replacement,
and service restart. Configuration hashes, cache identity, all 941 data files,
their sizes, and complete dirty ranges matched before and after installation:
277,364,333,920 pending bytes remained available. The active chunk size stayed
at 128 MiB and the cache continued using its migrated backing volume.

The persisted provider deadline remained unchanged at 2026-10-14 17:06:17
EEST. After restart, local statistics reported zero provider HTTP attempts;
a bounded directory listing returned `EIO` promptly during the hold and the
counter remained zero. A point-in-time socket inventory found no INET sockets.
This confirms local restart, queue preservation, and cooldown enforcement;
it does not validate successful batched uploads against Degoo after the hold.

## Authorized local queue reset and update retry — 2026-10-14

The latest staging update was initially refused because an unsafe generated
Python bytecode directory, `scripts/python/__pycache__`, had appeared beneath
the installed program tree. The contained wrapper already sets
`PYTHONDONTWRITEBYTECODE=1`; direct Python imports used for diagnostics had
created the bytecode. The generated directory alone was removed while idle.

For this recovery only, the user explicitly authorized clearing the local VFS
queue of 941 dirty files totaling 277,364,333,920 bytes. This was a data-loss
decision for the pending local versions, not evidence that those writes reached
Degoo. After a normal unmount, the vault configuration was changed to 4 GiB
chunks and the empty cache identity was initialized. The separate 326 GiB Borg
source, account-authentication file, and persisted provider cooldown state were
left untouched; the external cache bind path remained unchanged.

`degoo-rc update` then installed the package from successful staging workflow
38538213903, built from source commit
`9d51a7301809d9829869a15b67652a5d6d3aa9ba`. The service was explicitly started
afterward and was active with the mount connected, zero queued/uploading/retrying
files, and no local cache data files. Local stats reported zero HTTP attempts;
the persisted cooldown deadline remained 2026-10-14 17:06:17 EEST. These are
local recovery checks and do not establish remote completion of the cleared
queue or successful uploads after the cooldown.

A subsequent `degoo-rc update` matched the installed package manifest and
returned without downloading the archive, changing program files, restarting
the service, or altering the cache.

The live retry occurred before the follow-up updater change that reports the
precise relative path and checks the managed program tree before runtime stop.
That source change's tests and validation remain separate; this live event does
not validate the new preflight behavior. The queue clear was specifically
authorized for this recovery and does not change the normal policy that updates
preserve pending cache data.

All 388 offline Python helper tests passed for the updater follow-up. New
regressions exercise a group-writable generated bytecode directory and a
group-writable managed source file: both report the relative path and repair
action before mount preparation or shutdown, without modifying the files.
The snapshot-time ownership and permission checks remain in place. Scoped
Markdown lint and `git diff --check` also passed.

## Authoritative configured cache paths — 2026-10-14

The contained Python helpers now resolve the private vault's `cache_dir`
through one shared validator. Installer and runtime tests cover an external
private directory, a disjoint custom directory inside the install, unsafe
paths, bootstrap fallback, runtime shell resolution without bytecode writes,
upgrade locks, status metadata, and external-cache disk accounting. Lifecycle
tests cover external pending-write retention, configuration backup, local
restore, clean-cache deletion, and refusal of unknown, orphaned, mismatched,
or legacy cache data during destructive cleanup.

All 410 offline Python helper tests and the packaging self-check passed.
The credential-free full Go suite passed with serialized package execution
and an isolated runtime cache. A first parallel run failed an S3 cached
writeback test; its targeted rerun and the final full run passed. The complete
Degoo command race suite, scoped vet and lint, Markdown lint, and
`git diff --check` passed.

The new offline, read-only `degoo verify-cache` command checks the exact cache
identity while a maintenance caller retains its existing lock. Tests verify
that it accepts a matching identity under an active lock without modifying
files and rejects missing, mismatched, or wrong-folder identities. A local
read-only check also succeeded against the existing mount while it held its
cache lock; no provider request was involved.

The direct-path migration exposed an overly strict ancestor-permission check:
the shared volume was writable while the dedicated cache remained mode 0700.
The resolver now follows the native mount's private-cache contract, validating
the owned cache directory and rejecting symlink components without requiring
the enclosing volume to be private. A regression covers a private cache under
a writable shared volume and still rejects a non-private cache directory.

The clean build of commit `64346f2d802ebfc665c1bdf44af213a4672ab1cb` was
packaged and installed locally. After normal unmount, the cache identity was
rebound offline to the direct external path. The obsolete cache bind mount
and its `fstab` entry were removed; the service condition now checks the
underlying volume rather than the former bind target. No cache data was
discarded, and configuration hashes other than the intentional vault-path
change remained identical, including authentication and provider cooldown.

After restart, `vault.json`, the running process's `XDG_CACHE_HOME`, and
`degoo-rc info` all selected the direct external cache directory. The mount
and service were active, with zero pending files and zero provider HTTP
attempts. Repeating the same local-package update returned an unchanged-build
no-op while the external path remained configured. These are local lifecycle
checks; live provider uploads remain unverified during cooldown.

### Package update download progress bar

The package updater (`degoo-rc update`) now displays a live, adaptive progress bar
during package archive downloads instead of repeating elapsed time.

Validation passed:

- All 444 offline Python helper tests and the packaging self-check
  (`bin/package-degoo.py --self-check`) passed.
- Unit tests in `contrib/degoo/scripts/python/test_update.py` cover
  byte/rate/ETA formatting, column-width adaptivity (40, 60, 80, 100, 120
  columns), ETA retention at 80 columns, non-TTY periodic milestone emissions
  and 10-second heartbeats, TTY carriage-return updates and final line completion,
  clean exception unwinding, `ActionsSource._run` process attachment, timeout
  kill, exception cleanup, non-zero exit handling, and `ActionsSource.download`
  binding with validated artifact sizes and progress tracking.
- Markdown check and `git diff --check` passed cleanly with zero whitespace issues.

## Directory bounds, local authority and inactive page codec — 2026-10-15

Delivery: [#51](https://github.com/georgeasiminei/rclone-degoo/pull/51).
Targets `staging`;
merging remains with the user.

This increment was developed in an isolated worktree after PR #49, then rebased
onto PR #50 merged at `6bbe0374a` before integrated checks. The original worktrees
and unrelated stash are preserved. Production code changes cap Degoo's directory
cache and placeholder hints, revalidate provisional ancestors/nested roots, and
preserve provider/auth/cooldown errors through bounded resolution retries.

Two internal components are inactive: a Linux stable-lifetime writer authority
and a bounded authenticated metadata page codec using rclone crypt. They are
not wired into a new mount or publication format. The separate delayed-effect
experiment is test-only and assumes immutable exact remote IDs; it explicitly
shows that an old valid registry backup can select an older root. A checkpoint
failure after rename poisons that instance until disk reopen without replay.
See [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md) for bounds and limitations.

Passed against the integrated source:

- Fresh `go build` to a private temporary binary; installed runtime untouched.
- `go test -race -p 1 ./backend/degoo/... ./lib/dircache -count=1` with
  `RCLONE_CONFIG=/notfound`:
  backend, API, auth, authority, page codec and dircache packages pass.
- `go vet ./backend/degoo/... ./lib/dircache` and
  `golangci-lint run ./backend/degoo/... ./lib/dircache`: zero issues.
- Earlier full-source validation before the rebase passed all 189 test
  packages. The final integrated repository-wide run also passed all 189
  test packages with exit status zero.

The full-suite command uses the existing `CI=true` unreliable-network-test
policy, `RCLONE_CONFIG=/notfound`, a short private `TMPDIR`/`GOTMPDIR`, private
`XDG_CACHE_HOME`, and the fresh binary plus existing fusermount compatibility
link on `PATH`:

```sh
go test -timeout 20m -count=1 ./...
```

The stale-descendant regression failed against the baseline (`old-child` was
returned) and passed after the correction. Additional cases exercise complete
10,005-directory listing with bounded retention, eviction/replacement/inverse
aliases, same-ID owned strings, root-parent resolution under tiny budgets,
root aliasing, hint saturation, mid-resolution hint changes, retry exhaustion,
provider-fault preservation, directory moves and provisional nested roots.
Authority checks include a separate process paused with SIGSTOP while its
non-expiring flock stays held, cancellation, shared authority across distinct
cache locations, permissions/symlinks and replaced-lock refusal. Host-dependent
authority tests skip only on an unsupported machine-ID environment; none skipped
here. Codec cases cover the exact encrypted size boundary, corrupt/truncated/
malformed authenticated pages, wrong key/vault/identity and concurrent seal/open.

Read-only persisted-state checks still find the API hold through **2026-10-15
21:30:29 UTC**, with no separate password-login hold recorded. No provider/auth
request, runtime installation, reset, pending-write discard or reseed occurred.
Live direct-ID continuity, scoped terminal deletion, publication activation,
large-file/new-format mounted recovery and live soak remain unverified.

Final documentation checks passed: Markdown lint on all five changed documents,
66 relative links across the six handoff documents, 18 unique task IDs, and
`git diff --check`. Initial intermediate validation caught an import grouping
issue, corrected before final lint. Rebase conflicts were append-only evidence
and tracker sections; both PR #50 history and this increment were retained.
The integrated build/full suite/scoped race/vet/lint all passed after that rebase.

## Encrypted component follow-up and password-only recovery — 2026-10-15

Delivery: [#52](https://github.com/georgeasiminei/rclone-degoo/pull/52).
Implementation commit: `f743fde44`; targets `staging`, merging remains with
the user.

The user merged PR #51 at `5e945c0d7` while the follow-up was in progress.
After fetching, all pending changes were preserved on a fresh feature branch
from that `origin/staging`. The baseline source trees matched, so no source
reset/replay was needed; older worktrees and the unrelated stash remain intact.
This follow-up has its own PR; the user retains merge control.

Implemented inactive components include the actual authority checkpoint and
bounded allocation ledger, authenticated page references/store port, compressed
radix index, bounded manifest tree, stable-ID namespace, streaming chunk codec
and logical-file composition. They do not change the installed mount or activate
a new remote format. Detailed bounds are in
[SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md). The user explicitly requires
published-data recovery with the encryption password alone after account login;
no saved local config/locator/control file is a recovery prerequisite. Bootstrap
discovery and safe latest-root selection remain unimplemented provider gates.

Passed:

- Fresh full `go build` to a private temporary binary, with no runtime install.
- Credential-free repository-wide `go test -timeout 20m -count=1 ./...`:
  all **195** test packages pass, exit zero. Two earlier full runs also passed;
  the final run includes the subsequent checkpoint-conditional/stale-ledger
  and range-close corrections. Source comments added during validation do not
  change tested behavior.
- Scoped race tests pass across backend, API, auth, allocation, authority,
  data codec, file composition, index, manifest, namespace, page codec and
  dircache. Final changed safety/codec packages were rerun after review fixes.
- `go vet ./backend/degoo/... ./lib/dircache` and matching `golangci-lint`:
  zero issues.
- Real encrypted file/index/namespace graph read after recreating instances,
  synthetic 21 GiB + 17-byte splitting into 22 chunks, 262,145-descriptor
  manifest retained-memory bound, maximum index/manifest lookup depths,
  old-root preservation, malformed data, callback cancellation and fault cases.
- A real authority-file subprocess reopens a durable allocation reservation.
  Stale independent ledgers cannot overwrite that reservation or return an
  incomplete inventory. Local checkpoint comparison is not remote fencing.
- Allocation locator-alias and ignored range-header close-error regressions
  each failed before their correction and pass afterward. An initially
  incomplete real-authority test needed an explicitly private temporary parent;
  production privacy checks were retained.

The full suite uses `CI=true`, `RCLONE_CONFIG=/notfound`, short private
`TMPDIR`/`GOTMPDIR`, a private runtime cache and the fresh binary/FUSE compatibility
link first on `PATH`, as in the prior validation increment. No cloud credentials
are used by these checks.

Markdown lint passes on all six handoff documents; 69 relative links, 18 unique
task IDs and `git diff --check` pass too. A final read-only check at 20:00 UTC
still finds live API access held by the persisted deadline
through **2026-10-15 21:30:29 UTC**. No provider/auth request, service or mount
change, reset, data discard or reseed occurred. Direct-ID continuity/deletion,
password-only bootstrap, remote publication/fencing, sustainable allocator/
inventory/cleanup, reader pins, mount integration, mounted faults/rebuild and
live/soak acceptance remain open. The complete implementation goal is unfinished;
this validation does not turn those gates into passed requirements.

## Cumulative integrity review corrections — 2026-10-18

Delivery remains [PR #52](https://github.com/georgeasiminei/rclone-degoo/pull/52).
The [review](INTEGRITY_REVIEW.md) covers the cumulative changes from before
PR #42 through PR #52's initial head `e5012d922`. Its corrective branch starts
from that PR head after fetching; current `origin/staging` is already an
ancestor. No separate PR, reset or format activation is required.

Failing-before evidence:

- The single-host model let an unresolved root-create intent's child be
  retired and deleted. After local registry reopen and delayed completion,
  that root became current with missing data. Permanent tests fail against
  the unchanged model, then pass when unresolved create references block
  retirement and reconciliation refuses retired/delete-in-flight children.
  Recovery fixtures cover retired-only, unresolved-delete and applied-delete
  overlap without changing the intent or prior complete roots.
- Authenticated inode/link values with duplicate JSON fields decoded
  successfully. Escaped duplicate keys in a nested manifest identity also
  passed. These regressions fail before the namespace scanner is added.
  A separate test confirms Go's ordinary decoder accepts a Unicode long-s
  alias for `size`, while namespace lookup rejects the ambiguous value.

Permanent generated-data coverage:

- The historical `rename`/`md5` lifecycle and installed
  `norename`/`sha1all`/`fail_hard=true` lifecycle share setup and assertions.
  No-rename manifest downloads are expected. Shared handler injection happens
  before the HTTP server starts, avoiding copied auth setup and handler races.
- Failed replacement sources at six offsets and rejection of final manifest
  publication after additional objects were staged preserve exact old-file
  readback after clearing metadata caches. These are error-injection tests,
  not process/power-loss or live provider acceptance.
- Namespace decoding is capped at 8 KiB and depth 16, with per-object decoded
  field-name checks using Unicode case folding. The 120-step seeded model
  compares lookup/list state and retained immutable snapshots.
- Temporary `review_*` test files are removed; all useful cases have permanent
  names. No intentionally failing review test remains in the suite.

Final integrated checks:

- Fresh full build to an owner-private temporary binary: passed. That binary
  is first on `PATH` for the full test run; the installed runtime is untouched.
- `CI=true RCLONE_CONFIG=/notfound go test -timeout 20m -count=1 ./...`:
  all **195** test packages pass, exit zero, on the corrected source tree.
  As in earlier validation, `TMPDIR`/`GOTMPDIR` are short owner-private paths
  and `XDG_CACHE_HOME` is a private temporary runtime cache.
- Focused single-host and encrypted-lifecycle tests pass normally and with
  `-race`; namespace package tests and race tests also pass.
- Final integrated scoped race run with `CI=true RCLONE_CONFIG=/notfound`:
  `go test -race -p 1 ./backend/degoo/... ./lib/dircache -count=1` passed all
  **12** scoped packages, exit zero, with the same private temporary environment
  and corrected source as the full suite.
- `RCLONE_CONFIG=/notfound go vet ./backend/degoo/... ./lib/dircache`: passed.
- `golangci-lint run ./backend/degoo/... ./lib/dircache`, v2.14.0: zero issues.
- Markdown lint passes on all six changed documents. All 83 relative links
  across the seven handoff/review documents resolve, all 18 stable task IDs are
  unique, and formatting/staged diff checks pass. The final Go source
  fingerprint matches the tree used for integrated validation.

No live Degoo/authentication request, installed-runtime or service change,
reset, pending-write discard or user-data mutation was performed. The new
format remains inactive. Password-only remote recovery, verified publication,
allocator lifecycle, inventories, reader pins, cleanup and mounted/live/soak
acceptance remain open. The user merges PR #52 into `staging`.

# Degoo validation status — 2026-10-13

Offline implementation checks are recorded separately from live acceptance.
Live smoke checks passed against a dedicated disposable Degoo folder. A full
2 MiB logical file has been published and verified after a fresh-cache
remount, but the remaining live gates mean this does not establish general
provider compatibility or complete release acceptance.
See [API_EVIDENCE.md](API_EVIDENCE.md) and the remaining checklist in
[USAGE.md](USAGE.md).

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

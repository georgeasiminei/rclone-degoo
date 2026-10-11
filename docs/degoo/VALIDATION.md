# Degoo validation status — 2026-10-11

Offline implementation checks are recorded separately from live acceptance.
Live smoke checks passed against a dedicated disposable Degoo folder. A full
2 MiB logical file has been published and verified after a fresh-cache
remount, but the remaining live gates mean this does not establish general
provider compatibility or complete release acceptance.
See [API_EVIDENCE.md](API_EVIDENCE.md) and the remaining checklist in
[USAGE.md](USAGE.md).

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

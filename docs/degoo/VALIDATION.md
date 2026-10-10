# Degoo validation status — 2026-10-10

Offline implementation checks are recorded separately from live acceptance.
Live smoke checks passed against a dedicated disposable Degoo folder. A full
2 MiB logical file has been published and verified after a fresh-cache
remount, but the remaining live gates mean this does not establish general
provider compatibility or complete release acceptance.
See [API_EVIDENCE.md](API_EVIDENCE.md) and the remaining checklist in
[USAGE.md](USAGE.md).

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
no `install.sh`. The repair starts from the current `staging` implementation
and makes the archive layout independent of the source layout:

- Root: `README.md`, executable `install.sh`, and executable `uninstall.sh`.
- `bin/rclone`, `scripts/python/`, flat `docs/`, and `licenses/`.
- `docs/BUILD.json` records the source revision, dirty state, and binary hash.
  A `.tar.gz.sha256` sidecar verifies the archive bytes.

The wizard now discovers both packaged `bin/rclone` and a source build at
`build/rclone`. Both regression tests failed before the fix; all 65 installer
and cleanup tests passed afterward. A fresh `go build -o build/rclone .`
passed. Archive checks extract the package before running launcher help;
missing required launchers prevent replacing an existing output archive.
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

# Degoo private vault implementation plan

**Status (2026-10-10):** Core implementation and offline validation are
complete; live smoke checks passed against a dedicated disposable Degoo
folder. A 2 MiB logical file has been fully published and verified after
remounting with a fresh cache, including a CDN range read and matching full
SHA-256. Root-level empty-file, overwrite, truncate, and rename readbacks passed
after a fresh-cache remount. Nested creation, readback, rename, and deletion
also passed, followed by an empty fresh-cache remount. Live
limits above 20 GiB, pagination beyond 1,000 entries, and account-token
lifetime are unverified, so the vault is not yet accepted for normal use.

## Implementation decisions and validation plan

Upload timing separates the local close delay from transfer, registration,
verification, and publication. The installer exposes a positive `write_back`
duration and uses five seconds for new installs, matching the last deployment
and rclone's legacy default. Read-ahead (1 MiB), read streams (eight), directory
caching (30 minutes), and additional VFS/network controls live in the private
mount settings without new wizard prompts. The installer reuses a validated
detected binary without prompting for its path. Nonpositive and disabled values
are refused because they bypass background retry scheduling.

Registration recovery performs bounded metadata reads against the same
temporary object without replaying the mutation. Missing, null, or lost
acknowledgements receive a twenty-second visibility window; negative boolean
acknowledgements retain a two-second window. The reads honor cancellation and
provider retry delays. Failure leaves the logical write pending for VFS retry;
the visibility window does not establish provider mutation idempotency or
eliminate orphan storage from later retries.

The original offline implementation checks are complete. A dedicated live test
folder is now available and has been used for limited provider probes. The
original sequence below remains the release acceptance guide; live gates
cannot be inferred from mock results or from the partial live probes.

The contained install and cleanup experience is part of the user handoff. The
unified `degoo-rc` entrypoint provides `install`, `uninstall`, `login`, `setup`,
`mount`, `stop`, `status`, `location`, `folders`, `service`, `info`, `logs`,
`trash`, and `empty-trash`. Source checkouts use
`./contrib/degoo/degoo-rc install`; archive users run
`./degoo-rc install`; an installed user starts with `degoo-rc help`. Existing
installer and helper options remain available, with `degoo-rc COMMAND help`
documenting the detailed parameters. The command dispatches to shell helpers in
`scripts/` and Python implementations in `scripts/python/`. The installer
places `degoo-rc` in the private install root and creates
`~/.local/bin/degoo-rc` as a link, without editing shell profiles. It prints a
`PATH` export when that directory is not available; uninstall removes the link
only when it still points to that installation. The Degoo package in a source
checkout and the extracted archive use `degoo-rc` as their sole installer and
maintenance entrypoint. Fresh installed roots also have only `degoo-rc` as a
root-level Degoo command; in-place upgrades may retain root-level `.sh` shims
already present in an older install.

`degoo-rc install` starts the setup wizard in a terminal; `--binary` retains
the noninteractive install path, and `--no-wizard` supports staging before
login. The wizard checks Python 3.10 or
newer, a FUSE helper, writable `/dev/fuse`, and `mountpoint`. It can offer to
install `fuse3` and `util-linux` with a detected `apt-get`, `dnf`, `pacman`, or
`zypper`; when mount dependencies remain unavailable, it can still configure
the vault but does not enable the service. Go is required only to build from a
checkout, and the wizard can offer that build if it finds no binary.

The wizard first offers a retained backup directory for fresh installation.
Recovered folder and mount settings become defaults; saved account credentials
are checked with an authenticated folder query before prompting for a password.
Original vault settings remain fixed. Retained cache data is copied privately;
an offline `degoo restore-cache` command verifies the original identity and
changes only its cache-path binding. Existing installs use the upgrade flow
rather than merging retained caches. Remote folder-name discovery reports
collisions and requires the matching vault configuration to reuse a folder.
`degoo-rc backup-config` creates the same portable configuration layout without
uninstalling. File data, cache, and pending uploads are excluded. This supports
opening the same remote vault on another computer while pending uploads remain
on their source computer.

`degoo-rc sync LOCAL_PATH` refreshes the mounted directory view through a
private Unix socket exposing only the refresh operation. It clears the raw
Degoo folder-ID mapping and refreshes ancestors before the selected subtree,
using existing VFS forget/refresh behavior to retain pending local writes.
It does not enable a general remote-control server, mirror directories, or
promise a provider visibility deadline. Listing expiry remains lazy; clean
data fingerprints are checked when files reopen.

The folder picker queries account root ID `0` to discover device folders,
suggests a discovered device ID for new vault creation, and defaults the new
child name to `rclone-degoo-vault`. Root ID `0` is not accepted as a vault ID or
creation parent. The uninstaller's `--inventory` mode is read-only; its wizard
can retain config, cache, and logs in a new owner-private directory. Pending
uploads can proceed only when both the matching config and cache are retained.
Cleanup leaves remote data untouched. Explain external backup directories
before retention prompts and distinguish their path input from yes/no
choices. Stop the linked unit before disabling it; reload and verify systemd
after removing its links so local cleanup includes service registration.

The completion summary must appear after either the service or manual-start
choice. It reports the configured mount path and actual kernel mount state,
then gives commands for status, logs, login, and service control. Command
labels are shell comments so examples remain copyable. Upgrade prompts must
stop the owned running service or manual mount before collecting settings;
pending cached writes remain intact. Service startup gets a bounded wait for
the kernel mount to appear and checks that it is connected locally. A
disconnected stale FUSE mount may be detached only when the cache lock proves
no writer is active and the user accepts recovery. Stop helpers distinguish
mountpoint inspection errors from a confirmed absent mount. Default mount
logs use NOTICE to keep routine
cache cleanup from overwhelming actionable messages. Password
entry uses the original terminal output preserved by the logging layer.
`degoo-rc logs` provides recent command logs and a follow mode; `degoo-rc info`
highlights rejected authentication renewal with a login command. Saved token
expiry is
an advisory hint, and automatic renewal may still succeed. Recent failures
from before an updated account-token file must not keep reporting a stale
login-required warning.

Account trash remains a separate, explicit command rather than a backend
cleanup feature. `degoo trash` is read-only and paginated, with `--limit 0`
meaning all items; `degoo empty-trash` is irreversible, lists all account
trash, then sends a bulk `SetDeleteFile5` request with `IsInRecycleBin=true`.
It prompts twice unless `--yes` is supplied, and `--dry-run` lists without
mutating. This is deliberately not exposed through generic rclone cleanup or
remote paths because the operation is account-wide and crypt/chunker would
otherwise propagate a dangerous folder-shaped feature. No restore operation is
provided. The unified `degoo-rc trash` command lists trash and `degoo-rc
empty-trash` invokes deletion. Both pass the private auth path without reading
it; `degoo-rc` dispatches trash operations to the internal `scripts/trash.sh`
helper.

1. Implement the provider request contracts from the pinned Python reference
   in Go, with HTTP mocks for pagination, authentication, ranges, uploads, and
   partial registration failures. See [API_EVIDENCE.md](API_EVIDENCE.md).
2. Keep account tokens in an atomic private file. Prompt for the account
   password and never persist it; the independent vault secret lives in a
   separate versioned private configuration.
3. Construct `VFS -> chunker -> crypt -> Degoo` inside `rclone degoo mount`.
   Require the raw remote to identify a dedicated folder; fix crypt data and
   name encryption settings rather than trusting an arbitrary remote chain.
4. Force full VFS caching, bind the cache to its vault settings, and lock it
   against concurrent use. Add pending-upload status, restart recovery, and
   subprocess-crash recovery tests. Close/fsync promise local persistence,
   while remote publication is asynchronous.
5. Use `norename` transactions and manifests for every file. Repair generic
   chunker behavior that deletes old chunks before publishing new metadata,
   and reject orphan transaction manifests instead of returning them as data.
   Regression tests must reproduce both failures before applying the fixes.
6. Prove encrypted object names, data, and manifests; seek across chunk
   boundaries; reject wrong secrets and damaged/missing chunks. Run an opt-in
   generated 21 GiB encrypted round trip against local storage, then build,
   run the full offline test suite, and lint the changed packages/documents.
7. Commit and push a fork PR targeting `staging`, the integration branch.
   Leave merging and deleting the source branch to the user under the
   repository's Git instructions.

Installer defaults are 128 MiB plaintext chunks (the direct vault command
retains its 1 GiB fallback for an unspecified chunk size) and a provisional
19 GiB encrypted object
safety cap. Configuration validation includes crypt overhead and manifest
size. This does not establish the live provider limit. The vault uses rclone
crypt's existing format and chunker version 2 `simplejson` manifests, with
SHA1 for all files and encrypted manifest contents.

Live probes on 2026-10-10 established that creating the dedicated folder under
the account's device root succeeds, while a create request under account root
ID `0` is rejected. Responses include RFC3339 timestamp strings and boolean
mutation acknowledgements. The newly created encrypted-name placeholder was
observed as a zero-size `Cat0`, not `Cat6`; the original placeholder stays in
its parent while only its child marker moves into the returned `newCat2` folder.
A generated 2 MiB logical file was fully
published as encrypted chunks and an encrypted manifest, with pending uploads
reaching zero. After unmount, validation moved the old cache aside and
remounted with a new empty private cache. A seek to 1 MiB + 123 and 65,536-byte
read succeeded, and the complete readback SHA-256 matched the generated
source, demonstrating live CDN range behavior and logical-file visibility
after remount. The redundant old cache was removed after this proof.

A same-name original `Cat0` ghost caused the parent check for the final rename
upload to be true before the operation and false afterward. The backend now
retires verified-empty ghosts automatically, with tests passing; it does not
generally delete non-empty ghosts. Root-level empty-file, overwrite, truncate,
and rename operations passed readback after a fresh-cache remount. Generated
empty and lifecycle-test files were deleted, and a fresh mount root listing
verified their removal as a smoke check. Live cleanup also required recycling
active items, waiting for recycle visibility, and then hard-deleting them; the
retired setup ghost and 23 failed generated uploads were successfully cleaned
up. Current nested-directory handling is limited to a newly created empty
encrypted-name placeholder when that `Mkdir` is acknowledged and pre-lookup
finds no existing entry. Nested directory creation, child-file fresh-cache
readback, directory rename, file deletion, and empty-directory removal passed.
A final fresh-cache remount confirmed an empty vault and zero pending uploads.
Live support above
20 GiB, pagination beyond 1,000 entries, and account-token lifetime remain
unverified. Remote orphan garbage collection, multi-writer coordination, and
distributed atomic rename remain unsupported.

See [USAGE.md](USAGE.md) for configuration, commands, recovery behavior, and
the live acceptance checklist. [VALIDATION.md](VALIDATION.md) records offline
results, current live observations, and reproducible checks. The contained
installation and local-only cleanup flow are documented in
[INSTALLATION.md](INSTALLATION.md) and
[contrib/degoo/README.md](../../contrib/degoo/README.md); cleanup deliberately
leaves remote encrypted objects untouched.

## Original advisory handoff

This document describes a recommended route, not an API contract or a binding
architecture. [REQUIREMENTS.md](REQUIREMENTS.md) records the required behavior;
the summary below is for orientation. Later agents may choose different
libraries, architecture, or sequencing if they record the evidence and
rationale here before relying on the change.

## Firm human requirements

- Provide a writable Linux mount, through FUSE or equivalent, for a private
  Degoo file vault. Reads return plaintext locally; every file's contents must
  be encrypted before upload and decrypted locally on read.
- The supported user mount must require encryption and fail closed. It must
  never silently mount the raw Degoo backend or fall back to plaintext writes.
- Support logical files larger than 20 GiB by storing them as chunks.
- Keep the vault password in a simple private file configuration. Do not
  require a system keyring or a separate secret manager.
- Keep Degoo account login separate from the vault password. Renew account
  authentication automatically where supported and provide an explicit login
  path when renewal fails. Do not require a keyring for login or token refresh.
- Local VFS/cache contents may be plaintext on the user's trusted Linux
  machine. Local cache encryption, including LUKS, is not a requirement.
- Use only a dedicated test vault with generated test data for live writes.
  Do not read, alter, or delete existing personal Degoo data. Never print or
  dump sensitive file contents during debugging or validation.

## Recommended starting design

Implement a Go Degoo filesystem backend in this rclone fork. Reuse rclone's
Linux mount and VFS, then compose its existing chunker and crypt overlays:

`mount -> VFS -> chunker -> crypt -> Degoo backend`

This order makes crypt encrypt both data chunks and chunker metadata/sidecars
before they reach Degoo. Enable encrypted file and directory names by default
for the vault. Validate the resulting encoded names against Degoo limits.

Keep the Degoo transport backend raw for backend tests and internal use, but
make the supported vault mount entry point validate the complete encrypted
stack and reject raw mounts. A native rclone remote chain may be enough if the
entry point can enforce that rule. Add a small vault wrapper only if needed to
make that enforcement reliable. Never expose a plaintext fallback.

Start with one writer and a durable local pending-write cache. Define flush,
close, and fsync behavior explicitly: do not report remote durability until
all required chunks and encrypted metadata are confirmed. Preserve pending
writes across process restarts and make failed uploads visible and recoverable.
Do not promise multi-writer consistency or remote durability on close/fsync
until evidence supports those guarantees.

Use 1 GiB as an initial advisory chunk size. Prove it against observed Degoo
object limits, request behavior, and mount performance before fixing a default.
Keep chunk size configurable. Future agents may revise the layer order after
testing metadata exposure and encoded-name length, while retaining encrypted
sidecars and fail-closed mounts.

Store vault and auth configuration in private files and directories on Linux.
Owner-only permissions (for example, mode 0600 for files and 0700 for their
directory) are a recommended default. The config may contain the vault
password in plaintext; no separate key file is required. Rclone's config
obscuring is not encryption. Prefer an interactive Degoo password prompt and
private refresh-token storage without persisting the account password. Logout
must remove any stored account credentials while preserving the vault secret.

## Existing rclone capability and new work

Rclone already provides Linux FUSE mounting, VFS caching, the crypt and
chunker overlays, HTTP helpers, pacing, and backend test scaffolding. These
pieces do not provide Degoo connectivity or make an arbitrary raw backend
mount encrypted by themselves.

New work is the Degoo Go backend and its login, token refresh and private-file
storage, GraphQL/API mapping, object transfer, pagination, error handling, and
the vault mount entry point that enforces the encrypted chain. Use the Python
API in [cligoo](https://github.com/georgeasiminei/cligoo) as a behavior and
API-flow reference. Port the needed behavior to Go; do not invoke the Python
CLI as a subprocess.

Follow this fork's current default branch, `master`, from fetched
`origin/master`. This rclone fork has no `development` branch. Use a feature
branch based on the actual fetched default and target this fork unless a
separate instruction asks for upstream contribution. Do not assume or invent
an upstream PR target.

## API evidence to establish first

Treat these as known gaps to verify, not settled provider guarantees:

- The current Python client has no demonstrated ranged-download contract.
  Test the actual CDN response for `Range`, status 206, and a correct
  `Content-Range`; also test expired download URLs and token refresh.
- Upload has separate data-transfer and metadata-registration steps. Track
  their outcomes independently. Determine idempotency and orphan cleanup
  before retrying a transfer whose metadata registration failed.
- Listing and path resolution must paginate beyond the observed 1,000-item
  boundary. Test repeated pages, duplicate names, and stable path resolution.
- Degoo's provider checksum is a seeded SHA1 over the first 1 MiB, not an
  end-to-end integrity proof. The current upload verifier checks for a
  nonzero size and a download URL; it does not compare complete content.
- Classify expired credentials separately from transient failures. Refresh an
  expired token as appropriate; fail fast on a wrong password. Retry rate
  limits and network/server failures with bounded backoff, honoring provider
  retry guidance when present. Never log passwords, tokens, or file contents.

Use authenticated crypt reads and generated-file size/hash comparisons for
integrity evidence. Missing or invalid chunks must fail the whole read; never
return a truncated file as a successful result. Authenticated encryption can
detect content tampering, but it does not prevent remote deletion or rollback.

## Implementation sequence and exit gates

1. **Map the API and build an offline backend harness.** Read the current
   backend conventions and relevant `fs`, `lib/rest`, `fs/fshttp`, `lib/pacer`,
   `lib/dircache`, and VFS interfaces. Record observed Degoo request and
   response behavior from the Python reference. Add mocked tests for auth,
   pagination, path lookup, duplicate names, error classification, and
   transfer/registration partial failure. Do not start with writes to a
   personal account.

2. **Prove login and read-only listing.** Prompt for Degoo login interactively;
   persist refreshable tokens in the private file and refresh them without a
   keyring. Verify wrong-password behavior, token expiry, refresh, logout, and
   permissions. Exercise list and path lookup over more than 1,000 generated
   test entries in mocks, then do read-only live checks only against the
   dedicated test vault.

3. **Prove encrypted object round trips.** Wire the recommended stack and
   upload generated files only to the dedicated test vault. Confirm remote
   file data, names, chunk data, and chunk metadata are encrypted. Compare
   sizes and hashes after local readback; inspect no sensitive contents.
   Startup must fail if crypt or the required vault configuration is absent.

4. **Prove range reads and Linux mount behavior.** Validate CDN range behavior,
   crypt seeks, and chunk-boundary reads, including offsets above 20 GiB using
   generated data. Exercise stat, open, seek, read, and interrupted reads
   through the supported Linux mount. If range support is absent, document and
   resolve that limitation before claiming correct large-file mount behavior.

5. **Add writes and crash recovery.** In the dedicated test vault, exercise
   create, overwrite, truncate, rename, delete, and large-file writes. Interrupt
   transfers between data upload and metadata registration, then restart and
   prove that pending local writes recover, incomplete remote objects are not
   exposed as complete files, and orphans can be identified or cleaned up.
   Inject 429s, network timeouts, and 5xx responses; verify bounded retries
   and that wrong credentials do not loop.

6. **Run the backend suite and realistic vault validation.** Run unit tests,
   the backend's rclone integration suite, and practical Linux mount checks.
   Use a generated, disposable test tree with many entries, long names,
   duplicate names, empty files, and files larger than 20 GiB. Do not run
   destructive checks against existing personal vault contents.

The vault is ready for handoff only when the supported mount rejects raw or
unencrypted configurations; remote file bytes and chunk sidecars are encrypted;
round trips preserve generated contents; pagination is complete; missing
chunks fail hard; range and large-file behavior have evidence; token refresh
works from the private file without a keyring; and partial writes have a
documented recovery path. Report limits, failed probes, and any weaker
consistency guarantees alongside the evidence.

## Primary references

- [Rclone mount](https://rclone.org/commands/rclone_mount/)
- [Rclone crypt](https://rclone.org/crypt/)
- [Rclone chunker](https://rclone.org/chunker/)
- [Backend guide](https://rclone.org/contributing/#writing-a-new-backend)
- [cligoo API reference](https://github.com/georgeasiminei/cligoo)

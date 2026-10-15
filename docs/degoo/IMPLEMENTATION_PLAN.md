# Degoo private vault implementation plan

**Active work (2026-10-15):** Follow
[SCALABLE_STORAGE_PLAN.md](SCALABLE_STORAGE_PLAN.md) for the storage and metadata
redesign, stable task IDs, progress, and the next-session handoff. Its planning
task authorizes documentation only; implementation has not started. The user has
confirmed that current local pending uploads and remote Degoo contents are
disposable pre-live tests. A necessary reset can replace legacy compatibility
or migration work after advance notice; completing the existing upload queue
is not a prerequisite. See the binding
[pre-live policy](REQUIREMENTS.md#pre-live-redesign-and-disposable-test-data).
The sections below retain the previous implementation and validation history;
they do not establish that the proposed redesign is implemented or validated.

**Status (2026-10-14):** The earlier core implementation passed offline
validation, and limited live smoke checks passed against a dedicated
disposable Degoo folder, as recorded below. Follow-up fixes for cross-process
cooldown-state merging and bounded metadata calls passed scoped unit, race,
vet, and lint checks; Windows API/auth cross-compilation also passed. The final
full Go suite passed after a small error-path fix. A rebuilt localhost FUSE
smoke test confirmed fail-fast behavior and pending-write retention under
cooldown. The external cache migration is complete and checksum-verified; its
bind mount preserves the recorded cache path and identity. Live Degoo upload
recovery with the current code remains unverified. A 2 MiB logical file was
previously published and verified after remounting with a fresh cache,
including a CDN range read and matching full SHA-256.
Root-level empty-file, overwrite, truncate, and rename readbacks passed after a
fresh-cache remount. Nested creation, readback, rename, and deletion also
passed, followed by an empty fresh-cache remount. An expired-token refresh
initially returned HTTP 401. Authentication was later renewed, and read-only
provider queries confirmed the intended folder. A local vault binding that had
used a folder name instead of an opaque provider folder ID was corrected after
confirming its cache contained no identity or cached file data. No actual
provider ID or private path is recorded here. This does not establish reliable
automatic renewal. A later fresh installation mounted successfully after
clearing the external mountpoint's immutable flag; local health reported no
errors. Live limits above 20 GiB, pagination beyond 1,000 entries, and upload
recovery under Degoo's consistency model also remain unverified. Native Linux
acceptance now confirms that the administrator-installed helper mounts and
unmounts `/mnt/degoo` while retaining its immutable flag, using generated local
encrypted data. This does not establish live Degoo provider behavior.
The request-reduction, upload-batching, and encrypted small-object-cache
follow-up also passed the credential-free full Go suite, scoped race, vet, and
lint checks, plus a localhost FUSE fixture; no live Degoo request or write was
run for that follow-up. See [VALIDATION.md](VALIDATION.md).

## Implementation decisions and validation plan

### Payload inventories across updater versions (2026-10-14)

The first update adding `stats.py` exposed an updater compatibility bug: the
installed updater hashed the target archive using its imported installer
constants. Its older inventory omitted the new helper, so the archive and
binary checks passed while the payload hash differed from the published
manifest. The failure occurred before mount preparation or shutdown and
preserved both the running upload queue and installed files.

Payload comparison must read each tree's bundled installer inventory instead
of the updater process's imported constants. Parse the three literal helper
lists without executing the installed program; validate their bounds and
filenames, then apply the existing canonical hash algorithm. The target
inventory also defines archive completeness. Keep provenance and checksum
checks and verify the installed tree again after replacement. Regression
fixtures must cross a helper-addition/removal boundary, since comparing
packages built from one imported inventory cannot detect this failure.

An older affected updater can bootstrap from a checksum-verified package's
own `degoo-rc update --archive` command, run as the installing user outside the
mount with its temporary workspace outside the mount. This selects the new
helper inventory while retaining normal preparation, cache preservation,
installation, and runtime restart. It must not replace the installed scripts
manually or delete the cache. Pending logical files return to the writeback
queue after restart; an interrupted file retries from its beginning rather
than resuming a saved chunk or network byte offset.

### Local performance statistics (2026-10-14)

`degoo-rc stats` reads the running mount's private `/stats` endpoint. It offers
a human-readable cache bar, an optional live display, and JSON with exact byte
counters. Reading statistics never triggers Degoo API or storage requests,
token renewal, directory refresh, cache eviction, or upload verification.
Ordinary mount activity continues independently.

The generic VFS cache snapshot sums stored ranges instead of relying on the
last cleaner pass. It separates dirty bytes from clean bytes, then separates
clean bytes into quota-evictable and open/grace-period pinned bytes using the
same eligibility conditions as removal. It reports configured cache targets
and local filesystem space without scanning the cache tree. The snapshot is
per item; concurrent I/O can change the cache while it is collected.

The Degoo client counts encrypted payload bytes consumed by existing upload
HTTP bodies and read from existing download response bodies. Bounded time
buckets retain approximately five seconds of traffic for recent rates.
Retries count again, and verification downloads count as received traffic.
These counters measure payload passing through the HTTP client, not wire
acknowledgements or completed logical-file publication. Existing API request
and metadata-cache counters remain available alongside queue and process
statistics. Pending sizes in ordinary `info` and `status` output use binary
units while JSON retains exact byte counts.

### Provider cooldown and external cache placement (2026-10-13)

The public-source review found no published numeric GraphQL request quota. A
live `GetFileChildren5` request instead returned HTTP 429 with a 65,043-second
`Retry-After`; the evidence and limitations are recorded in
[API_EVIDENCE.md](API_EVIDENCE.md). The backend persists the provider's value
as a request deadline and returns a typed rate-limit error during the active
period without sending another API request. After expiry, the deadline no
longer blocks requests. A throttled listing fails promptly instead of becoming
an empty directory, leaving VFS and its pending local writes intact.

Metadata lookups use bounded, context-aware retries under a ten-second total
deadline that covers all pages. They stop when Degoo supplies `Retry-After`;
retrying inside that interval defeats the cooldown. Background VFS writeback
schedules the pending upload at the provider deadline rather than sleeping in
the request path. Reuse returned download URLs for their short validity
window. During upload publication, use the targeted item lookup first and omit
the additional canonical directory listing only when the exact expected item
metadata matches. Any uncertain publication remains pending for VFS retry.

On Linux, clients coordinate the persisted cooldown through a private sidecar
lock. They re-read state before token retrieval, refresh work, and HTTP calls,
and merge concurrent deadlines by retaining the latest expiry. Lock waits are
bounded to five seconds and honor cancellation. Non-Unix builds use the
existing fallback; Windows API/auth cross-compilation passed, but that check
does not validate cross-process lock behavior there.

`GetFileChildren5` uses a bounded snapshot cache with a five-second lifetime
and at most 32 cached parent listings. Known mutations invalidate only the
affected parent or parents; an unknown parent uses a conservative global
invalidation. Explicit directory-cache flushes clear all snapshots. Generation
checks prevent an older in-flight response from restoring invalidated data.
Fresh-list reads bypass cached responses and cache writes, invalidate only the
requested parent, and coalesce concurrent fresh reads for that parent and
generation. Ordinary concurrent misses share one paginated request; waiters
cancel independently, and the shared request has a 30-second ceiling while
preserving the initiating caller's context values. The backend's enclosing
metadata lookup retains its ten-second deadline. These changes reduce
duplicate listing calls without extending the five-second freshness window or
claiming a numeric Degoo quota.

### Metadata request reduction and encrypted small-object cache (2026-10-14)

The API snapshot cache is deliberately short lived because listings are used
for path resolution and mutation confirmation. The cache coalesces
simultaneous misses for the same parent, while every distinct uncached parent
and every page still requires its own provider request. A generation-aware
item-to-parent hint table lets mutation attempts invalidate known parents
instead of discarding unrelated snapshots. Known parents are invalidated
before and after each attempt, including errors. If an item's parent is
unknown, global invalidation remains the safe fallback. An epoch guard prevents
stale in-flight lookups from overwriting parent hints after a move.

Concurrent uploads now collect write-authorization records for the same parent
for up to 150 milliseconds, with a maximum of 30 records per request.
Registration records can span parents and are batched up to 100 per mutation.
Path-specific locks serialize writes to the same remote file while allowing
independent paths to upload concurrently. Shared requests are bounded to 30
seconds, callers cancel independently, and registration is never automatically
replayed after an ambiguous response. The caller confirms each staged item by
its own ID, name, parent, and size, performs the configured full-payload
readback verification unless `skip_upload_verification` is enabled, then
rechecks the destination immediately before publishing. Unconfirmed writes
remain pending. Confirmation polls start at 250 milliseconds and double to a
three-second cap, honor a longer provider `Retry-After`, and stop on
cancellation; the existing 20-second and two-second registration visibility
windows remain in place. This uses known array-shaped provider operations; it
does not establish the provider's maximum batch size or retry semantics.
The signed storage byte transfer alone does not change folder metadata or
invalidate directory snapshots; the registration mutation performs the
parent-scoped invalidation.

The native encrypted vault has a separate persistent cache for raw ciphertext
objects up to 256 KiB. It is bound to the vault cache namespace and validates
object ID, size, modification and creation times through a hashed version key,
plus exact length and SHA-256 before use. Its 30-minute expiry, 64 MiB encoded
size limit, and 8,192-entry limit keep local storage bounded. A one-minute index
sweep avoids scanning the directory for each small object. Ranges can read
from the cached full object; upload verification bypasses the cache. The cache
does not store decrypted bytes and does not cache GraphQL listing results.
Unsafe paths fail closed, and cache write errors do not fail remote reads.
`DirCacheFlush` clears this cache with the directory snapshots.

Use `rclone degoo stats --sync-socket PATH` with the private socket of a running
mount to inspect aggregate HTTP attempts by operation, listing-cache activity,
and small-object cache hit/miss/fill/eviction counts. It reads the mount's
local private endpoint and makes no Degoo request. Counters do not include
identifiers, names, URLs, request bodies, or credentials. Final offline test
status for this follow-up is recorded in [VALIDATION.md](VALIDATION.md); no
live-provider request or write was used to validate these changes.

The native mount defaults to 1 GiB when `chunk_size` is omitted, while the
current `degoo-rc` setup flow writes 128 MiB into new vault configurations.
For an existing vault, its saved value remains authoritative and is not
rewritten by a binary upgrade. Existing files retain their recorded chunk
layout, and reads use the actual stored chunk sizes; future writes use the
configured size. Changing chunk size changes the local cache identity, so
drain pending uploads to zero, unmount normally, and back up the vault
configuration and cache before changing it. Start the new identity with a
fresh, empty cache namespace and backing directory. The installed helpers
resolve `cache_dir` from the vault configuration; a bind mount is optional.
Check that the encrypted chunk size still fits under
`max_object_size`. A 1 GiB setting remains a recommendation to validate with
the account's limits and workload, not evidence of a provider limit or a
required production change.

### Configured cache location

The vault's `cache_dir` is authoritative for the native mount and installed
Python helpers. One shared resolver validates a dedicated absolute path;
installation and upgrade, runtime environments, status, disk accounting,
locking, backup retention, and cleanup use it. The contained install's
`cache` directory is a default, not a required runtime path. Portable backup
bundles still store a copy under their own `cache/` directory and restoration
chooses a destination cache before rebinding its identity.

A path migration requires a normal unmount, private configuration backup,
complete cache copy with checksum verification, a `cache_dir` edit, and the
existing offline `degoo restore-cache` identity rebind. Pending writes must
remain paired with their metadata. An external cache's unrelated files and
mounts must be protected during cleanup; helpers must refuse unverifiable
contents instead of deleting an arbitrary configured directory. An optional
service condition can require the underlying volume to be mounted.

The initial migration used a bind mount as a workaround for the helpers'
fixed-path validation. Its SHA-256 comparison covered 1,886 files occupying
258.013 GiB; two truncated dirty files were restored from stable originals,
leaving all 941 data files complete and dirty. This is historical local
preservation evidence, not remote upload completion. Operator steps for direct
configured paths are in [USAGE.md](USAGE.md).

### Bash command completion

`degoo-rc completion bash` prints a script sourced with
`source <(degoo-rc completion bash)`. It derives command names from the CLI's
registry and completes service actions, benchmark modes, and help topics.
Further arguments retain Bash's normal completion, including local paths.
Completion generation reads no account or vault state and requires neither
an installed binary nor the `bash-completion` package. Users can add a guarded
source line to `~/.bashrc`; the installer continues to leave shell profiles
untouched. Opening a new terminal or sourcing again refreshes suggestions
after an update. Other shells are outside this change's scope.

### Verified unattended package updates

The contained Linux amd64 client provides `degoo-rc update`. Its current
`ActionsSource` resolver uses authenticated `gh` to select the latest
successful `degoo-package.yml` workflow artifact on `staging`; it does not
read GitHub releases. The resolver boundary can later select release assets,
but that source is not implemented today. `--archive PATH` accepts a local
`.tar.gz` package with its adjacent SHA-256 sidecar and bypasses GitHub.
Manual mounts and installs with no owned service links do not require a
systemd user manager.

The updater verifies the archive checksum, binary hash in `docs/BUILD.json`,
and (for an Actions download) the clean source commit against the workflow run
before changing mount or install state. It then compares the installed binary
and managed helper files. The install manifest records `build.version: 1`,
`build.binary_sha256`, `build.payload_sha256`, and a verified `build.commit`
when available. Updates also record `build.package_sha256` and
`build.source`, including `build.run_id` for Actions downloads.
Documentation-only changes do not alter the managed payload; an identical
payload causes a no-op that leaves the service, configuration, manifest, and
cache untouched, including when `--delete-cache` or wait options were supplied.

For Actions runs, the workflow also publishes a small
`rclone-degoo-linux-amd64-update` artifact containing the archive SHA-256
sidecar and a `.tar.gz.update.json` manifest. The updater downloads this first
and computes the hash of the installed binary and managed files on disk; it
does not trust the saved install manifest to decide whether a change is needed.
If the payload matches, it skips the full archive download and all runtime or
cache changes. Otherwise it verifies the downloaded archive's checksum, size,
binary and payload hashes, and clean commit against the lightweight manifest
and workflow run. Invalid metadata is an error. Runs without the separate
artifact, including older runs, use a visible full-archive fallback for
comparison and verification.

The updater reports phase changes and repeats elapsed time every ten seconds
while a phase remains active. It does not invent percentage estimates. Before
downloading a changed full archive, it refuses an active-mount update when the
updater or its calling shell is working inside the mount. The same check runs
before mount preparation for local archives. A no-op remains allowed from the
mount. The child process cannot change its parent shell's current directory, so
the user must `cd` to the path named in the error, then retry. No prompt or
forced/lazy unmount is used.

For a mounted vault, the updater first calls the running client through its
owner-private control socket to enter generic VFS maintenance. The barrier
refuses new opens and file or directory mutations with `EBUSY`, while allowing
existing operations to finish. It waits for in-flight operations and all
tracked file and directory handles, including read handles, to close, then
flushes cached file data and metadata to stable storage. `--writer-timeout`
defaults to `30s` and is capped at `1h`; the same deadline bounds handle drain
and local flush. The updater never prints the opaque preparation token.

Local flush is separate from remote upload completion. The default
`--wait-for-uploads off` preserves closed pending uploads in the cache for the
mount's retry queue after restart. A positive duration waits for uploads to
reach zero before shutdown, with a maximum of `1d`. A writer timeout, upload
timeout, local flush failure, or unsupported older mounted client fails before
the updater stops the runtime. Preparation failure releases the barrier; an
abandoned successful preparation is bounded by a 120-second lease. Already
current packages return before preparation and do not change runtime or cache
state, even when wait options are supplied.

Only after successful preparation does the updater request a normal FUSE
unmount and verify that the kernel mount is gone; it asks systemd to stop an
owned service afterward. A busy normal unmount keeps the service active and
releases the barrier. The updater then holds the vault cache lock through
replacement. Service enablement is retained; only a previously active service
or recognized manual mount is resumed, and the updater confirms a connected
mount. Manual mounts resume in the background with their prior arguments and
working directory. Inactive installations remain inactive. Busy or ambiguous
mounts and conflicts in the owned service's links or loaded unit block the
update. Pending writes and unverifiable cache state block `--delete-cache`; an
ordinary update preserves the cache.

A running mount without the preparation endpoint is not stopped automatically.
The user must unmount it normally and rerun the update while it is inactive; an
installation old enough to lack the updater command requires a one-time archive
upgrade after stopping the mount.

Cache removal is opt-in and runs only after a different payload was installed.
The held lock inode remains in place. Deletion requires the known cache
identity, paired VFS data and metadata, and complete metadata proving clean
entries with a fingerprint; pending, unknown, unpaired, and unexpected entries
are preserved by refusal. Installer failure restores the previous managed
program snapshot. A restart failure returns nonzero and leaves the installed
build and configuration available for recovery. An abrupt termination can
leave the snapshot at `tmp/update-backup`, which blocks another update and
requires explicit recovery; automatic crash rollback is not provided. Offline
tests cover archive validation, no-op behavior, and install rollback. They
also cover pending-cache refusal and cache-lock preservation. They do not
establish live Degoo service
behavior.

### Explicit installation startup errors

When automatic startup is selected, a rejected service request or an
unconfirmed connected mount ends installation with an explicit error and a
nonzero exit status. When an immutable `/mnt/degoo` has no valid helper, the
Step 6 preflight stops before the service-start prompt and returns a
nonzero result while preserving configuration and the local cache. It prints
one administrator command from the private install root:

```sh
sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup
```

This command builds as the invoking account and installs only the fixed
root-owned helper. It preserves the immutable flag and never mounts or starts
the service as root. If GCC is missing, the wizard prints the detected
distribution's package command and the user can rerun `mount-setup` after
installing it. Once setup finishes, continue as the ordinary user. With
systemd, run `degoo-rc service enable` and then `degoo-rc info`. Without
systemd, add `--foreground` to `mount-setup`, then run `degoo-rc mount` as the
ordinary user and check `degoo-rc info` from another terminal. An already
enabled service can be retried with
`degoo-rc service restart`. The
[helper README](../../contrib/degoo/immutable-mount/README.md) documents the
helper's contract and advanced manual installation.

### Action-specific installer prerequisite preflight

Before a wizard action uses the selected binary, mountpoint, or service path,
check the prerequisites for that action. List required and optional items and
distinguish absent tools or devices from installed but unusable or
misconfigured ones. If the detected distribution and system context support a
repair, print safe copy-pastable commands, explain when administrator
privileges are required, and give verification and retry commands. Include
actionable diagnostics for Python, source-build Go, the FUSE helper,
`mountpoint` from `util-linux`, `/dev/fuse` availability and access, and the
optional systemd user manager as applicable. A missing immutable-helper
compiler or source file also needs a specific explanation and recovery steps.

Check an immutable selected mountpoint and the manually installed helper before
offering or enabling service startup. The helper is required only for an
immutable `/mnt/degoo`; validate its root ownership, protected parent and file
permissions, setuid mode, and whether the containing filesystem is mounted
`nosuid` or `noexec`. Print one explicit `sudo ... mount-setup` command from
the current private install root, with the ordinary-user service or
foreground-mount commands clearly separated. If GCC is missing, print the
detected distribution's compiler package command. Point to the bundled
[helper README](../../contrib/degoo/immutable-mount/README.md) for its contract
and optional manual installation. These checks must never clear the immutable
flag or automatically install, replace, or modify a privileged helper. Do not
enable or start a service when a known required blocker remains. Preserve the
completed configuration and pending cache so the user can repair the
prerequisite and retry; report readiness only after the requested mount is
confirmed. The wizard runs this preflight before its final service-start
choice. The new command's validation results will be recorded in
[VALIDATION.md](VALIDATION.md).

### Complete local cleanup and interrupted-uninstall recovery

Uninstall checks mount-directory blockers before disabling service links and
verifies that the installation directory and owned links are absent before reporting
success. Failed or unverifiable cleanup exits nonzero with an incomplete
cleanup message and retry instructions. Immutable legacy mount directories
require the user to clear the flag; cleanup never invokes sudo automatically.
The install wizard can recover missing service links after systemd confirms
that the unit is absent and inactive, or proves a loaded unit belongs to this
installation before stopping it. Pending-upload retention checks still apply.

### Health reporting, file upload state, and mountpoint protection

The contained `degoo-rc status` command checks authentication first, then
mount/service health and local upload state. It returns a nonzero exit code for
unhealthy state and supports JSON output. With a selected file or directory it
reports `pending`, `uploading`, `uploaded`, or `unknown` from the mounted VFS
read-only `user.rclone.upload-status` attribute. `uploaded` describes the
current VFS version after writeback and does not trigger a fresh remote audit.
The generic `--vfs-upload-status` option defaults to false; `rclone degoo
mount` enables it. When the mount is absent, ordinary files below the
mountpoint are reported as local-only and are never sent to Degoo. The private
Unix HTTP socket's `GET /status` response exposes aggregate queued, uploading,
and retrying counts, without per-file names or content.

New contained installs default to `/mnt/degoo`; upgrades retain their recorded
mount path. Installer helpers allow only `/mnt/degoo` and the legacy contained
`mount` path. Migration to `/mnt/degoo` requires the previous underlying mount
directory to be empty, and uninstall leaves the external directory in place.
The mount creates an owner-private directory when possible, refuses to cover
local files, and protects the underlying directory after mounting and on
failure or unmount. If setup cannot create it, diagnostics print privileged
`mkdir`, `chown`, `chmod`, and `chattr +i` commands. An immutable flag may stay
set across mounts at `/mnt/degoo`. Standard `fusermount3` rejects the required
write-access check for an immutable directory even when it is owner-writable.
Mounting with the flag retained therefore requires the documented manually
installed helper at `/usr/local/libexec/rclone-degoo/fusermount3`. It can mount
only an empty `/mnt/degoo` owned by the caller and not writable by group or
other users; the default directory remains owner-private. It leaves the flag
unchanged and runs rclone as the ordinary user. It rejects `allow_other` and
`allow_root`. Mount launch selects this helper only after checking its root
ownership, protected permissions, and setuid bit; ordinary mutable mountpoints
continue to use the system helper. No privileged helper is installed
automatically. If the immutable mountpoint lacks a valid helper, diagnostics
print build/install instructions and the service command to retry. The
immutable legacy in-install `mount/` location is not supported by this helper;
migrate to `/mnt/degoo` to use it, after ensuring the old underlying mount
directory is empty. Run `degoo-rc install --mountpoint /mnt/degoo` to change the
saved path while preserving the cache. That legacy-path limitation remains a
compatibility exception to the earlier mountpoint requirement.

Status output places authentication errors first. Private `status.log` and
`diagnostics.log` capture the relevant helper and Degoo diagnostics; displayed
command errors omit Cobra usage blocks and redact recognized credentials.
The final repository-wide test suite, scoped race and lint checks, Python
helper tests, package self-check, native FUSE upload-status smoke test, and
changed-document lint passed; details are recorded in [VALIDATION.md](VALIDATION.md).
Earlier validation mounted successfully only after clearing the immutable
flag. Native Linux acceptance now establishes the retained-flag mount path
with generated local encrypted data; no Degoo network request or customer data
was involved. The ordinary FUSE helper still cannot pass its write-access
check for an immutable directory.

Upload timing separates the local close delay from transfer, registration,
verification, and publication. The installer exposes a positive `write_back`
duration and uses five seconds for new installs, matching the last deployment
and rclone's legacy default. Read-ahead (1 MiB), read streams (eight), directory
caching (30 minutes), and additional VFS/network controls live in the private
mount settings without new wizard prompts. The installer reuses a validated
detected binary without prompting for its path. Nonpositive and disabled values
are refused because they bypass background retry scheduling.

The contained `degoo-rc benchmark read|write` command measures mounted
throughput using generated zero data for writes and the selected file for
reads. Writes create a new path, then time through VFS `uploaded` state,
including `fsync`, close, and writeback; reads request direct I/O unless
`--buffered` is selected. Both modes delete the file after success by default,
while `--no-delete` retains it and failures preserve it. A read may still come
from rclone's disk cache, and VFS `uploaded` state is not an independent cloud
integrity proof. Detailed options and examples are in [USAGE.md](USAGE.md).

Write cleanup checks the original device, inode, type, and logical size. The
pre-close timestamp may differ from the post-upload timestamp because FUSE,
the VFS cache, and Degoo do not provide a stable modification-time version
identifier. When only this timestamp differs, the benchmark validates the
generated zero data locally before accepting success. This preserves detection
of same-size content edits without rejecting normal writeback. Validation is
outside throughput timing, within the total timeout, and does not establish
independent remote integrity. Read cleanup retains its timestamp comparison.

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
unified `degoo-rc` entrypoint provides `install`, `uninstall`, `backup-config`,
`login`, `setup`, `mount`, `stop`, `status`, `update`, `benchmark`, `sync`,
`location`,
`folders`, `service`, `info`, `logs`, `trash`, and `empty-trash`. Source
checkouts use
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
the vault but does not enable the service. The action-specific checks and
repair instructions described above extend this initial prerequisite handling.
Go is required only to build from a checkout, and the wizard can offer that
build if it finds no binary.

The wizard first offers a retained backup directory for fresh installation.
An early vault import is installed before account login so the fresh account
password is encrypted with the imported data key. New vaults and imports
selected later require a final login after setup when the encrypted account
credential is missing. Startup is offered only after that credential is saved;
failure preserves completed configuration and pending cache data. Ordinary
upgrades reuse existing saved credentials. Imported account credentials and
tokens remain excluded from restoration.
Recovered folder and mount settings become defaults; restoration requires a
fresh account login and ignores credentials present in older backups.
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
2. Keep account tokens in an atomic private file. When a vault configuration
   is available, encrypt the account credentials with its data-encryption key
   for automatic password login after rejected renewal. Persist a login
   cooldown separately from credentials and retain the independent vault secret.
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
obscuring is not encryption. Prompt interactively for the Degoo password and
encrypt saved account credentials with the vault cipher as authorized on
2026-10-12. Keep refresh tokens separately in private storage. Logout
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

Follow the fork's integration branch, `staging`, from freshly fetched
`origin/staging`, as specified in [AGENTS.md](../../AGENTS.md). Use a new feature
branch and open its PR against this fork's `staging`; the user merges it.
This supersedes the original default-branch advice. Upstream contribution is
a separate decision.

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
  limits by persisting the provider deadline and making no API requests before
  expiry. Retry other transient network/server failures with bounded,
  context-aware backoff. Never log passwords, tokens, or file contents.

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
   Inject 429s, network timeouts, and 5xx responses; verify persisted cooldown
   fast-fail, no requests before expiry, bounded context-aware retries for
   other transient failures, and that wrong credentials do not loop. Verify
   that throttled listings fail rather than appearing as empty directories.

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

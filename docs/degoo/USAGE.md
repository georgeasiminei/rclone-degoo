# Encrypted Degoo vault on Linux

This fork provides a `degoo` transport backend and a `rclone degoo mount`
entry point. The entry point always constructs:

```text
Linux FUSE -> VFS full cache -> chunker -> crypt -> Degoo
```

The implementation is experimental. Offline tests exercise the request shapes
and vault behavior. The latest request-reduction, upload-batching, and
encrypted small-object cache changes passed the credential-free Go suite,
scoped race, vet, and lint checks, plus a localhost FUSE test; they were not
tested against Degoo. A disposable live vault passed encrypted publication,
fresh-cache readback, and CDN range reads. An expired-token refresh initially
returned HTTP 401; authentication was later renewed and read-only provider
queries confirmed the intended folder. The local vault binding was corrected
after confirming the prior cache held no identity or cached file data. These
events do not establish reliable automatic renewal. The production mount
remained active during the recorded cooldown and failed listing operations
promptly; live upload recovery after the deadline remains unverified. Larger
live files and the provider object-size limit remain acceptance gates; see
[VALIDATION.md](VALIDATION.md) for the tested scope.

Follow-up fixes for cross-process cooldown-state merging and bounded metadata
calls passed scoped checks and a rebuilt localhost FUSE smoke test. The final
full Go suite passed after a small error-path fix. Live Degoo upload recovery
with the current code remains unverified.
The initial external cache migration was SHA-256 verified and used a bind
mount to retain the recorded cache path. Startup was guarded so the
service only runs when that path is a mount point; see [VALIDATION.md](VALIDATION.md).

The install wizard asks for a mount directory, with `/mnt/degoo` as the fresh
default and the saved path as the upgrade default. Choose an empty dedicated
absolute path or use `degoo-rc install --mountpoint PATH`. The choice is saved
in `install.json` and propagated to the launchers and maintenance commands.
Custom directories use the ordinary mount guard; the privileged helper for an
immutable directory supports only `/mnt/degoo`.

The contained installer and maintenance commands use one `degoo-rc` entrypoint.
Start it from a source checkout with `./contrib/degoo/degoo-rc install`,
from the archive root with `./degoo-rc install`, or use `degoo-rc help` after
installation. Run `degoo-rc COMMAND help` for detailed options. The installer
places the command at `~/.local/share/rclone-degoo/degoo-rc` and links it from
`~/.local/bin/degoo-rc`; it does not edit shell profiles and prints a `PATH`
export if that directory is missing from `PATH`. Uninstall removes the link
only when it still points to this install. A fresh installed root has
`degoo-rc` as its only root-level Degoo command. Upgrades may retain root-level
`.sh` shims already present in an older install. The Degoo package in a source
checkout and the extracted archive use `degoo-rc` as their sole installer and
maintenance entrypoint. Examples here use `degoo-rc`.

The command set is `install`, `update`, `uninstall`, `backup-config`, `login`, `setup`,
`mount`, `stop`, `status`, `stats`, `benchmark`, `sync`, `location`, `folders`,
`service`, `info`, `logs`, `trash`, `empty-trash`, and `completion`.
Service actions are `install`, `enable`, `disable`, `start`, `stop`, `restart`,
and `status`.
Existing command options remain available. `trash` lists account trash; `empty-trash`
separately performs account-wide deletion.

Declining service startup in the wizard registers the user unit without
enabling or starting it. After editing configuration, run
`degoo-rc service start` without sudo. On an older installation with no
registered unit, first run `degoo-rc service install`. Use `service enable`
when automatic startup is
wanted. The generated service belongs to the user's systemd manager; copying
it into `/etc/systemd/system` makes it run as root and fail private ownership
checks. See [service setup](INSTALLATION.md#login-mount-and-recovery).

For a fresh, unused vault, choose its encryption settings before the first
write. After manually changing the vault password or salt following account
login, run `degoo-rc login` again: the saved account password is encrypted with
those settings and must be saved under the chosen key. Valid tokens can keep
mounting while masking unusable saved credentials. Existing encrypted data
requires its original password and salt; editing these fields is not a rekey
operation. A populated cache location requires the documented cache migration,
not merely changing `cache_dir`.

For the recommended contained installer, persistent user service, cached-email
login, mount settings, and complete local cleanup, see
[INSTALLATION.md](INSTALLATION.md). The manual steps below are an alternative
for users who manage their own paths and services.

## Recovering an update refused for an unsafe program path

Before stopping a service or mount, `degoo-rc update` checks the installed
program files and reports the exact unsafe path relative to the installation
root. Inspect that path and its parent directories before changing anything;
for example, `stat -c '%U:%G %a %n' PATH` shows ownership and mode, and
`namei -l PATH` shows each path component. Use the installation root reported
by the command or the `Install:` path shown by `degoo-rc info`.

If the reported path is a generated Python `__pycache__` directory, schedule a
maintenance window, stop the contained service or mount normally, and make
sure no installer, update, or helper process is running. Then remove only that
generated bytecode directory and retry the update. A normal stop preserves
pending cache data. Do not remove Python source or other managed program files.
If a source or executable is named, restore that exact path from a verified
package or use the repair instructions in the error; do not apply a blanket
`chmod` or `chown` to the installation. Vault, account, and mount
configuration files have separate private modes and must not be loosened to
fix a program-tree check.

An ordinary update preserves the VFS cache and pending uploads. `--delete-cache`
is an explicit, guarded option, not a routine update repair. Deleting queued
cache data is an exceptional recovery action and should occur only when the
user has explicitly authorized that data loss for a specific recovery plan.

## Recovering after deleting the remote vault

The local installation stores the dedicated remote folder's ID. Deleting that
folder in Degoo leaves the local binding pointing to an unavailable object;
neither `update` nor `login` selects a replacement folder. A new folder with
the same name has a different ID. In the observed 2026-10-18 incident,
`GetOverlay4` returned `GraphQLException` for the removed root while account
listing still succeeded. That error class alone does not prove deletion;
account or provider problems can also prevent root access.

Stop a failing service with `degoo-rc service stop`, then inspect
`degoo-rc info` and `degoo-rc logs`. Keep the cache and matching configuration
until local recovery needs have been assessed. A clean cache entry may hold
only downloaded ranges, so a cache with no pending uploads is not a complete
backup of deleted remote files. Recovery of remote deletions depends on
provider trash or another backup; this client does not guarantee it.

For an intentional fresh start, use the documented
[uninstall and reinstall flow](INSTALLATION.md#uninstall), retaining both
configuration and cache if any local data may be needed. Retention needs
enough separate disk space. Create a new dedicated remote folder and use a
fresh cache through the installation wizard, then copy the source data again
with rsync. Do not hand-edit the old folder ID or reuse its cache against a
new vault. The installer deliberately refuses a different folder ID for an
existing installation, including through `setup`; a folder newly created by
`setup` can remain unbound if the follow-up install refuses that change.

Updated updaters check candidate root access before stopping a mounted vault.
If access fails, resolve the remote binding or account problem first. This
does not restore already deleted data, and a provider failure can still occur
between the check and restart.

## Updating with pending uploads and an older updater

An ordinary `degoo-rc update` keeps pending uploads in the local cache, flushes
their data and recovery metadata, normally unmounts, installs the package, and
restarts a previously active mount. No queue-drain or resume flag is required.
Do not select `--delete-cache` or `--wait-for-uploads` when the intention is to
restart with pending work. Open application handles can delay preparation;
`--writer-timeout 2m` allows more time for those handles to close. Preparation
failure leaves the runtime and installation intact.

The restarted mount queues dirty logical files again. An interrupted logical
file can upload again from its beginning: there is no persistent transfer
offset or per-chunk resume journal. Completed files remain completed. The
local FUSE recovery tests exercise generated pending writes; interruption of
an active multi-chunk upload against Degoo remains a live acceptance gate.

An older updater can report `the downloaded package does not match its update
manifest` when a valid release adds a helper absent from its own file list,
such as `stats.py`. This failure happens before mount preparation and leaves
the existing upload running. The corrected updater reads each tree's own
inventory. To bootstrap an affected installation, use the verified package's
own updater rather than replacing installed scripts manually.

Replace `RUN_ID` below with the successful staging workflow run shown by the
updater. Run as the installing user, outside the mounted vault. These commands
download from GitHub, check the archive's matching checksum, and run the
bundled updater against that archive; temporary files stay outside the mount.
The local archive mode verifies its checksum and binary/package contents; it
does not repeat the GitHub metadata/run comparison. Use the artifact from the
same verified successful run and keep its checksum sidecar beside it.

```sh
cd /tmp
update_dir=$(mktemp -d /tmp/rclone-degoo-update.XXXXXX)
gh run download RUN_ID --repo georgeasiminei/rclone-degoo \
  --name rclone-degoo-linux-amd64 --dir "$update_dir" &&
  (cd "$update_dir" && sha256sum -c rclone-degoo-linux-amd64.tar.gz.sha256) &&
  tar -xzf "$update_dir/rclone-degoo-linux-amd64.tar.gz" -C "$update_dir" &&
  TMPDIR=/tmp "$update_dir/rclone-degoo/degoo-rc" update \
    --archive "$update_dir/rclone-degoo-linux-amd64.tar.gz"
```

## Tab completion in Bash

Enable completion in the current terminal:

```sh
source <(degoo-rc completion bash)
```

Type `degoo-rc up` and press Tab to complete `update`. Type `degoo-rc u`
and press Tab twice to list `update` and `uninstall`. Completion also suggests
service actions, benchmark modes, and command names after `degoo-rc help`.
Other arguments keep Bash's normal completion, including local file paths.

For new terminals, add this line to `~/.bashrc`:

```sh
if command -v degoo-rc >/dev/null 2>&1; then source <(degoo-rc completion bash); fi
```

The installer does not edit your shell profile. The guard makes the line harmless
after uninstall; you can remove it yourself. This command supports Bash only.
Completion lists come from the client when sourced; open a new terminal or run
the `source` command again after updating. From a source checkout, use
`source <(./contrib/degoo/degoo-rc completion bash)` instead.

## Setup

Build this fork with `go build`. Linux FUSE and `fusermount3` must be installed
to run an ordinary mount. If the mountpoint has `chattr +i`, standard
`fusermount3` cannot mount it even when it is owner-writable; retaining that
flag at `/mnt/degoo` requires the administrator-installed helper described in
[immutable-mount/README.md](../../contrib/degoo/immutable-mount/README.md).
Choose a dedicated Degoo folder and obtain its folder ID without changing
existing personal files. Use a separate private directory for account tokens
and the vault configuration:

```sh
install -d -m 700 ~/.config/rclone-degoo
install -d -m 700 ~/.cache/rclone-degoo
./rclone degoo login ~/.config/rclone-degoo/auth.json --email you@example.com
```

Login prompts for the account password. It saves access and refresh tokens in
an atomic, permission-protected file. This standalone command saves tokens only.
After creating the vault configuration, add `--vault-config /private/vault.json`
to save encrypted account credentials for automatic login after rejected
renewal. Contained `degoo-rc login` supplies this option when its vault exists.
The install wizard ensures the encrypted account credential is saved before
offering to start the mount. An import supplied early is installed before
login; a vault created or selected later requires another hidden password
prompt after setup. Ordinary upgrades keep existing saved credentials.
Accounts that cannot use email/password login need a Degoo-supported password
login or separately provisioned refresh token; browser-token extraction is
not provided.

The `account_credentials` field in `vault.json` holds a versioned encrypted
email/password record, encoded as base64. It uses rclone crypt's authenticated
data format and the same vault password and optional salt as file encryption.
The account password is never stored as plaintext or merely obscured.
The vault key is stored locally in the same permission-protected configuration;
this protects against accidental plaintext disclosure, not someone able to
read the complete configuration. Configuration backups omit this credential,
including its encrypted form. After restoring a backup, log in once to enable
password fallback again. Configuration backups retain the vault decryption
key and omit account access and refresh tokens as well as the account password.

Automatic recovery first tries token renewal. A rejected renewal or absent
refresh token can trigger password login. Network errors and temporary server
errors do not trigger password login. Failed attempts retain tokens and pending
local writes, with a persistent cooldown to avoid login storms; explicit login
bypasses that cooldown. `degoo-rc info` reports whether the credential is saved
and whether automatic login remains incomplete. A successful login writes new
tokens that the running mount can reload on its next request.

For standalone logout with a saved password, supply the same `--vault-config`
to remove the encrypted credential as well as tokens. The command also detects
a sibling `vault.json` by default. Stop the mount before logout because it may
hold a usable access token in memory.

The proposed next recovery layer, including checks while the mount is idle,
is described in [Session recovery proposal](SESSION_RECOVERY.md).

Add a raw remote to your usual rclone configuration, replacing the paths and
folder ID below. `root_folder_id` is mandatory and must identify a dedicated
folder, rather than account root `0`. The backend scopes mutations beneath
this ID.

```ini
[degoo-test]
type = degoo
root_folder_id = YOUR_DISPOSABLE_FOLDER_ID
auth_file = /absolute/private/path/auth.json
```

Create a vault configuration, with a password independent of the account
password, then mount it:

```sh
./rclone degoo init ~/.config/rclone-degoo/vault.json \
  --remote degoo-test: --cache-dir ~/.cache/rclone-degoo
mkdir -p ~/degoo-vault
./rclone degoo mount ~/.config/rclone-degoo/vault.json ~/degoo-vault
```

The command runs in the foreground. Use a service manager for unattended
operation. Standard mount and VFS flags are available; full caching is always
enabled. HTTP request/response dumps are refused because the API carries
tokens in JSON. A mount is refused if its cache cannot initialize. The generic
`rclone mount` command does not construct or enforce the private vault stack;
use `rclone degoo mount` for this vault.

`vault.json` uses this versioned format. Sizes can be strings accepted by
rclone or integer byte counts:

```json
{
  "version": 1,
  "remote": "degoo-test:",
  "password": "YOUR_INDEPENDENT_VAULT_PASSWORD",
  "cache_dir": "/absolute/private/cache/directory",
  "chunk_size": "1Gi",
  "max_object_size": "19Gi"
}
```

An optional `password_salt` supplies crypt's independent password salt. Back up
the vault configuration securely: lost passwords make the remote data
unreadable. Both config and token files must be regular owner-private files
inside owner-private directories. The vault configuration deliberately stores
the password as plaintext on the trusted local machine. Host compromise can
expose it and the plaintext cache.

The salt is optional. With no custom salt, use the same password and keep that
setting absent on other machines. If a custom salt was used, it must also be
preserved exactly. Mounting the same folder with different encryption settings
does not migrate or decrypt its existing files.

Pass `--settings /private/path/mount.json` for persistent cache, read, network,
and verification settings; the file must be owner-private, just like the vault
configuration.
Explicit mount flags override corresponding settings. `--verify-upload=false`
skips full SHA-256 download verification before publication while retaining
metadata and publication checks. It defaults to true and is independent of
the encryption password and cache identity.

## Remote changes and cache staleness

Cache coherence describes keeping cached data consistent with its source;
cache staleness is the delay before an external change becomes visible.
The contained mount defaults to `dir_cache_time: "30m"` and disables remote
change polling because Degoo has no change-notification implementation.
Each directory is refreshed on access after its listing expires. For an
actively accessed directory, a server-visible change normally appears within
the remaining 30-minute interval plus the next successful listing request.
An idle directory does not actively fetch changes when the timer expires.

This is not a strict end-to-end deadline. The other client must first finish
publishing its upload, Degoo must expose the new listing, and the request must
succeed. Open file and directory handles can retain an older view until they
are reopened. Degoo's separate folder-ID map has no expiry, so external folder
replacement or renaming can require an explicit refresh or mount restart.
`cache_poll_interval: "5s"` only schedules local cache cleanup;
`cache_max_age: "750h"` controls retention of unused clean cached files.
Neither setting determines remote change visibility. `attr_timeout: "1s"`
controls an additional kernel attribute cache.

`cache_max_size: "60G"` and `cache_min_free_space: "60G"` are soft targets
for evicting clean cached data, not hard limits on disk use. Dirty entries
must remain until their logical write is fully uploaded and published. Uploading
some encrypted chunks does not make an unfinished logical file safe to evict;
the VFS cache entry remains dirty until the whole backend write succeeds. If
the cache contains only dirty entries, these targets cannot reclaim its space.
The `750h` maximum age makes unused clean content eligible during cleanup; it
does not mean that content is deleted immediately or exactly 30 days later.

Refresh the current mounted directory tree without waiting for the listing TTL:

```sh
cd /mnt/degoo/photos
degoo-rc sync .
# An absolute mounted file or directory path also works:
degoo-rc sync /mnt/degoo/photos/example.jpg
```

The command refreshes remote directory metadata recursively. A file path
refreshes its containing directory; a missing path refreshes its nearest
existing directory inside the mount. The command also refreshes ancestor
listings and clears the raw Degoo folder-ID map to resolve moved or replaced folders.
Paths must belong to the contained mount. Pending local edits stay intact;
this operation does not upload or mirror directories, predownload all file
contents, or discard the disk cache. Clean cached content is checked against
the refreshed remote fingerprint when reopened; vault manifests include SHA1
for detecting changed logical content. A dirty local version retains priority
and can later upload over a remote version, so refresh is not conflict merging.

The running mount must support the private refresh endpoint. After upgrading
an older client, restart the mount or service before using `sync`. A successful
refresh reports the provider's current listing, not completion of another
client's upload or immediate replacement of an application's open buffers.
See rclone's [directory cache documentation](https://rclone.org/commands/rclone_mount/#vfs-directory-cache)
for the underlying expiry behavior.

## Persistence and recovery

`fsync` syncs the local VFS cache. Close queues an asynchronous upload. Neither
operation promises that Degoo has already received the file. Keep the cache
directory across restarts and login failures. Dirty files are reloaded by VFS
and uploaded when the same vault is mounted again with working credentials.
New installs set `write_back` to five seconds, matching rclone's default.
Uploads begin when that delay expires and a transfer slot is available.
Existing settings without this field retain the five-second default. The value
must be positive and finite; `0s`, negative durations, and `off` are refused to
preserve background uploads and retries. Failed attempts double their retry
delay, starting from `write_back`. Upload verification, when enabled, downloads
and hashes each encrypted object before publishing it. New installer settings
use `verify_upload: false` to skip that extra readback while retaining metadata
and publication checks.

The contained health command reports authentication first, followed by mount
and service health and local upload counts. It returns a nonzero exit code when
any health check is unhealthy. Add `--json` for machine-readable output:

```sh
degoo-rc status
degoo-rc status /mnt/degoo/report.pdf
degoo-rc status /mnt/degoo/report.pdf --json
```

The optional file or directory path reports per-file upload state. `pending`
means the current local version is waiting for upload, `uploading` means a
transfer is in progress, `uploaded` means VFS knows the current version has a
remote representation after writeback, and `unknown` means no state can be
proven. The VFS exposes the read-only Linux extended attribute
`user.rclone.upload-status`; generic mounts leave it disabled by default, while
`rclone degoo mount` enables it. `uploaded` is not a new remote read or an
independent integrity audit.

When the mount is absent, ordinary files found under the mountpoint are
reported as `local-only`; they are not pending and will never be uploaded by
the vault. The mount refuses to cover a nonempty underlying directory. Move
those files elsewhere before mounting, then copy them into the mounted view if
they belong in the encrypted vault. If a separate persisted cache record is
available, status reports it separately from the ordinary local file.

Status also reports aggregate queued, uploading, and retrying counts from the
mount's private local socket. It does not return filenames or file contents
from that endpoint. A retrying upload remains pending; inspect `degoo-rc logs`
for its failure. Zero pending files means the local cache has no outstanding
dirty writes; it does not confirm current cloud contents or integrity.

A process lock prevents two mounts from sharing a cache. Use one writer for
each vault; separate machines or cache directories cannot coordinate through
this lock. A local cache binding refuses changes to the remote, secret, salt,
or chunk settings, so pending plaintext cannot be silently uploaded to a
different vault. Preserve the original configuration to recover it.
If the identity file is missing while the cache contains files, mounting also
fails; restore the original identity before reusing that cache.

Stop the mount before logging out:

```sh
fusermount -u ~/degoo-vault
./rclone degoo logout ~/.config/rclone-degoo/auth.json
```

Logout removes only the token file. It leaves the vault password, cache,
configuration, and remote objects intact. A running process may still hold
its access token until it exits; logout does not revoke tokens at Degoo.

An expired or rejected token prompts the auth client to check for a newer
private token file. Logging in again can therefore restore a running mount
without changing its vault secret or discarding pending writes. Rejected
refresh errors report `degoo login required` and a sanitized HTTP status;
passwords and provider response bodies stay out of logs.

## API rate limits and cache placement

On 2026-10-13, a `GetFileChildren5` listing request received HTTP 429 with
`Retry-After: 65043` seconds. From the captured time, the requested retry time
was 2026-10-14 17:06:16 EEST. The official public material reviewed does not
publish a numeric API request quota. Degoo's web viewer also displays a daily
limit message for HTTP 429 from a content-download URL; that is a separate
request path and does not explain the listing response.

The backend persists the provider deadline beside the private auth file. On
Linux, processes coordinate through a private sidecar lock, re-read state
before token retrieval and requests, and retain the longest concurrent
deadline. A call during the cooldown returns a typed rate-limit error without
another request; after expiry, that deadline no longer blocks requests.
Metadata calls have a ten-second total bound across pagination and retries,
honor cancellation, and stop on provider `Retry-After`; a failed listing is an
error, never an empty-directory result. Background writeback keeps pending
uploads in the VFS queue until the provider deadline rather than sleeping in
the request path.

Ordinary `GetFileChildren5` listings use a five-second in-memory cache capped
at 32 parent directories. Concurrent misses for the same parent share one
paginated request, and waiters can cancel independently. A fresh listing used
for mutation confirmation bypasses cached results and cache storage and only
invalidates that parent. Known mutations also invalidate only their known
parent directories before and after the attempt; an unknown parent triggers a
conservative full in-memory cache clear. This keeps the short freshness window
while avoiding duplicate work during bursts. Sync still needs a listing for
each distinct directory and an HTTP request for every page, so this cache does
not eliminate all listing traffic or prove a numeric provider quota.

Concurrent uploads can share storage-authorization requests in batches of up
to 30 files from the same parent and metadata-registration mutations in batches
of up to 100 files across parents. A 150-millisecond window collects concurrent
uploads, with a 30-second ceiling for the shared request. Each upload receives
its own result, and one caller's cancellation does not cancel the others.
Path-specific locks serialize writes to the same file while independent files
can upload concurrently. Registration is not blindly retried after an
ambiguous response; the backend confirms each staged object's ID, name, parent,
and size, verifies the full uploaded content by default, then rechecks the
destination immediately before publication. `skip_upload_verification` can
disable the full readback. Failed confirmation keeps the local write pending.
Confirmation polls begin at 250 milliseconds and double
to three seconds, honor a longer provider `Retry-After`, and can be cancelled.
Existing 20-second and two-second visibility windows are retained. These batch
sizes are local implementation bounds, not a Degoo-published request limit.

The native encrypted vault can persist raw encrypted provider objects up to
256 KiB in `cache_dir/metadata/<vault-cache-name>`. Entries expire after 30
minutes, with maximums of 64 MiB encoded and 8,192 objects. The cache uses a
SHA-256 key derived from object ID, size, modification time, and creation time;
it checks exact length and content SHA-256 before use. Corrupt or stale
entries are fetched again. Open ranges can be served from a cached full object,
while upload verification reads bypass the cache. The cache contains
ciphertext, never decrypted file data, and it does not cache directory
listings or lower the number of `GetFileChildren5` calls. Its index is swept
once per minute. If a cache path is unsafe, the mount fails closed. Cache
write errors do not fail a successful remote read. `DirCacheFlush` clears both
the in-memory listings and the persistent small-object cache.

To inspect a running mount without contacting Degoo:

```sh
degoo-rc stats
degoo-rc stats --watch
degoo-rc stats --watch --interval 5
degoo-rc stats --json
```

The report shows a cache bar, cached bytes and files, dirty bytes awaiting
writeback, clean bytes eligible for eviction, and clean bytes pinned by open
handles or their grace period. Dirty bytes include open writes that are not
yet in the upload queue. Sizes use binary units such as MiB and GiB; JSON keeps
exact bytes. Cache use measures stored VFS byte ranges, excluding filesystem
allocation overhead, metadata, and the separate encrypted small-object cache.
The configured cache size is a soft target and can be exceeded by pending or
open files. Evictable means eligible for quota-driven cleanup; this command
does not remove cache files or wait for their age limit.

It also shows queued/uploading/retrying file counts, recent upload and download
traffic, cumulative encrypted payload bytes, HTTP attempts by operation,
listing-cache activity, small-object cache hit/miss/fill/eviction counters,
cache-filesystem free space, mount uptime, and Go runtime memory statistics.
These counters reset when the mount restarts. Transfer rates use a recent
five-second window and return to zero when idle. Upload bytes count payload
read into HTTP requests, including failed attempts and retries; download bytes
include ordinary encrypted object and verification reads. Multipart framing and
wire acknowledgements are excluded. These rates describe observed traffic,
not confirmed file publication or a network speed test.

Both snapshot and watch modes use only the private local mount socket and
local runtime state. They make no Degoo or storage requests, perform no login
or token renewal, and do not refresh remote directory listings. A separate
watch process does not count toward Degoo's request quota. Normal mount
activity can continue making requests while statistics are displayed.
Use Ctrl+C to stop watching. `--json` and `--watch` are separate modes.
Older running mounts need an updated binary and a normal restart before the
new runtime metrics become available; preserve pending cache data.

For manually managed mounts, the native JSON command accepts an explicit
private statistics socket:

```sh
"$HOME/.local/share/rclone-degoo/bin/rclone" degoo stats \
  --sync-socket "$HOME/.local/share/rclone-degoo/tmp/vfs-sync.sock"
```

Use the executable and socket installed for your mount if they are elsewhere.
Statistics do not show IDs, names, URLs, request bodies, or credentials. Live
upload recovery with the current code has not yet been tested against Degoo.

The cache contains plaintext VFS data, including pending writes. The
`cache_dir` in the private vault JSON is authoritative for the mount and its
installed helpers. The default is `$HOME/.local/share/rclone-degoo/cache`;
a dedicated owner-private absolute directory on another volume is supported
without a bind mount. Update, status, disk reporting, locks, and cache retention
use that configured location. Unsafe paths and conflicting cache identities
are refused.

To move an existing cache:

1. Stop the service or unmount normally and verify that the mount is gone.
   Back up the vault configuration and keep it private (mode `0600`).
2. Mount the destination filesystem, create a dedicated cache directory with
   mode `0700`, and copy the complete cache with ownership and modes preserved.
   Include the identity file, VFS data, recovery metadata, and encrypted
   small-object cache. Do not copy a live cache or merge different vault caches.
3. Compare full file checksums between the source and destination. Keep the old
   cache and vault configuration until the destination has been verified.
4. Change only `cache_dir` in `vault.json` to the new absolute path. Rebind the
   copied cache identity locally using the original private configuration:

   ```sh
   rclone --config /path/to/rclone.conf degoo restore-cache \
     /path/to/original-vault.json /path/to/vault.json \
     --folder-id YOUR_CONFIGURED_VAULT_FOLDER_ID
   ```

   This command is offline. The folder ID must match the installed remote, and
   the configurations must match except for cache location. Preserve the vault
   password, salt, chunk size, and object cap.
5. If the cache is on a separately mounted volume, arrange for that volume to
   be mounted before the service starts. An optional user-service condition
   can require the volume's mountpoint:

   ```ini
   [Unit]
   ConditionPathIsMountPoint=/path/to/cache-volume
   ```

   Use the filesystem mountpoint, not an ordinary cache subdirectory. Reload
   the user manager after changing a drop-in. Remove obsolete cache bind-mount
   entries and conditions when switching from a bind-mounted path to the direct
   path.
6. Start the service or mount, check `degoo-rc status` and `degoo-rc info`, and
   compare the pending queue with the saved state. Remove the old copy only
   after confirming the new cache works.

The initial external-cache migration used a bind mount to work around the
helpers' former fixed-path restriction. Its SHA-256 verification and pending
write restoration are recorded in [VALIDATION.md](VALIDATION.md). A bind mount
remains an optional storage arrangement; it is not required by the helpers.

## Mounted throughput benchmark

`degoo-rc benchmark` measures a mounted file read or write. The default path is
`<mount>/benchmark.bin`; an explicit relative path is resolved from the current
working directory. The selected path must stay inside the mounted vault. Writes
create a new file and refuse to overwrite an existing path. Reads require an
existing, nonempty regular file.

```text
degoo-rc benchmark write [FILENAME] [--size SIZE] [--block-size SIZE]
  [--timeout SECONDS] [--poll-interval SECONDS] [--no-delete]
degoo-rc benchmark read [FILENAME] [--block-size SIZE] [--timeout SECONDS]
  [--buffered] [--drop-caches] [--no-delete]
```

```sh
# Create a generated 2 GiB file and retain it for the read benchmark:
degoo-rc benchmark write --no-delete
degoo-rc benchmark read

# Use a named path and tune write size and block size:
degoo-rc benchmark write /mnt/degoo/benchmark.bin --size 8GiB --block-size 32MiB --timeout 7200 --no-delete

# Try a buffered read if direct I/O is unsupported on this mount:
degoo-rc benchmark read /mnt/degoo/benchmark.bin --buffered

# Show all options for either mode:
degoo-rc benchmark write --help
degoo-rc benchmark read --help
```

Both successful modes delete the selected file by default. `--no-delete` keeps
it after success; every failed or interrupted run preserves the file. `--size`
(write only) defaults to 2GiB. `--block-size` defaults to 16MiB. Both options
accept bytes or binary K/M/G/T suffixes such as `512KiB` and `2GiB`.
`--timeout` defaults to 3600 seconds and bounds the I/O and, for writes, the
upload wait together. Writes check upload state every second by default;
`--poll-interval` changes that interval.

Reads request direct I/O by default to bypass the kernel page cache. Use
`--buffered` to allow kernel-cached reads. `--drop-caches` explicitly invokes
`sudo` to drop Linux system caches before timing; it does not clear rclone's
disk cache. A direct read may still be served by rclone's disk cache, so a
benchmark does not guarantee a cloud download.

Write timing includes the local write, `fsync`, close, writeback delay, and the
wait until VFS reports this file version as `uploaded`. It measures the
mounted write path through VFS completion; that state is not an independent
cloud integrity check. A timeout or other failure leaves the benchmark file
available for inspection with `degoo-rc status` and `degoo-rc logs`.

A writeback can change the modification time shown by the mount without changing
the file contents. If only that timestamp changed, the write benchmark checks
that the file still contains the generated zero data before reporting success
or deleting it. This local content check is excluded from the reported throughput
time but uses the same total timeout. Replaced files, changed sizes, nonzero data,
and changes during the check cause refusal and preserve the file. Reads continue
to refuse cleanup when their source timestamp changes.

## Account trash

The trash commands are account-wide and take the private account-auth file
directly. They do not use the vault configuration or a remote path:

```sh
./rclone degoo trash ~/.config/rclone-degoo/auth.json
./rclone degoo trash ~/.config/rclone-degoo/auth.json --limit 20 --json
./rclone degoo empty-trash ~/.config/rclone-degoo/auth.json --dry-run
./rclone degoo empty-trash ~/.config/rclone-degoo/auth.json --yes
```

`trash` is read-only and paginated. Omitting `--limit`, or passing
`--limit 0`, lists every trash item; a positive limit truncates the listing.
`--json` requests machine-readable output.

`empty-trash` first lists all trash items, then sends their IDs in a bulk
`SetDeleteFile5` request with `IsInRecycleBin=true`. Deletion is irreversible.
The command prompts twice unless `--yes` is supplied. `--dry-run` only lists
what would be deleted and makes no account changes.

The unified command provides the same operations:

```sh
degoo-rc trash --auth ~/.config/rclone-degoo/auth.json --limit 10 --json
degoo-rc empty-trash --auth ~/.config/rclone-degoo/auth.json --dry-run
degoo-rc empty-trash --auth ~/.config/rclone-degoo/auth.json --yes
```

The auth file defaults to the contained installation's `config/auth.json`.
Pass `--auth` to select another private file. The command passes the auth path
through without reading or displaying its contents. `degoo-rc` dispatches the
trash operation to its internal `scripts/trash.sh` helper.

Trash is not confined to one Degoo folder or vault. Listing can expose
account-wide metadata, and emptying can destroy unrelated trash. Generic
rclone cleanup is deliberately unsupported: the operation is account-wide,
and exposing it as a backend feature would let crypt and chunker propagate
a folder-shaped cleanup command across the vault stack. There is no restore
operation.

## Remote format and limits

The vault uses rclone crypt's authenticated data format and standard encrypted
file/directory names. Chunker uses version 2 `simplejson` manifests, SHA1
metadata for every file, and `norename` transactions. Crypt encrypts both the
chunks and their manifests. Authenticated reads reject damaged ciphertext.
Missing chunks fail reads and listings rather than succeeding with truncated
data. Provider deletion and rollback are outside encryption's protection.

The native mount defaults to 1 GiB when `chunk_size` is omitted. The current
`degoo-rc` setup flow saves 128 MiB for a new vault, so check the actual saved
configuration instead of assuming its value from the binary version. An
existing vault keeps the size saved in its configuration; upgrading does not
change it automatically. Existing files retain their recorded chunk layout,
and reads use each stored chunk's actual size; future writes use the configured
size. Changing chunk size changes the local cache identity. Before changing it,
wait for pending uploads to reach zero, unmount normally, and back up the vault
configuration and cache. Start the new identity with a fresh, empty cache
namespace and backing directory. The helpers use its configured `cache_dir`;
a bind mount is optional.
Confirm the configured `max_object_size` can hold the encrypted chunk, which is
slightly larger than the plaintext chunk. The provisional 19 GiB cap includes
crypt headers and authentication tags and leaves a margin below the historical
20 GiB report; it is a configuration safety cap, not a verified provider
limit. Choose a smaller cap if required by your account. No production
chunk-size change is required by this guidance.

Chunker publishes a manifest after all its chunks are uploaded. For `norename`
overwrites, old chunks are retired after successful manifest publication.
Failed uploads leave the cached logical write pending. Crashes can leave
unreferenced remote transaction objects; automatic orphan garbage collection
and distributed atomic rename are not provided. Provider metadata operations
can fail ambiguously and must be reconciled rather than blindly retried.
Upload registration with a missing, null, or lost acknowledgement allows up to
twenty seconds for the exact temporary object's metadata to appear. A `false`
acknowledgement uses a shorter two-second confirmation window. These read
requests obey the window and any provider `Retry-After` delay. A matching object
still must pass the configured verification and publication checks. If it
cannot be confirmed, the write remains pending; a later VFS retry can start a
new upload. No acknowledgement variant is assumed to prove that the provider
performed or rejected the mutation.

Remote sizes, object counts, directory topology, timing, and access patterns
remain visible. Existing native Degoo files are outside the encrypted vault
format; copy them into the mounted vault explicitly if desired.

## Live acceptance still required

Use only generated data in a dedicated disposable vault. Exercise empty
files/directories, rename, delete, remount, pagination beyond 1,000 entries,
token renewal/re-login, and failures between byte upload and registration.
Confirm CDN `206`/`Content-Range` support before relying on seeks. Test a logical
file larger than 20 GiB, inspect the encrypted object sizes, and compare local
hashes after readback. Interrupt a multi-chunk update and verify that recovery
returns a complete old/new version or a clear failure.

The full acceptance checklist is in [REQUIREMENTS.md](REQUIREMENTS.md).

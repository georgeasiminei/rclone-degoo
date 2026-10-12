# Encrypted Degoo vault on Linux

This fork provides a `degoo` transport backend and a `rclone degoo mount`
entry point. The entry point always constructs:

```text
Linux FUSE -> VFS full cache -> chunker -> crypt -> Degoo
```

The implementation is experimental. Offline tests exercise the request shapes
and vault behavior. A disposable live vault passed encrypted publication,
fresh-cache readback, and CDN range reads. An expired-token refresh initially
returned HTTP 401; authentication was later renewed and read-only provider
queries confirmed the intended folder. The local vault binding was corrected
after confirming the prior cache held no identity or cached file data. These
events do not establish reliable automatic renewal, and final mounted health
verification is still pending. Larger live files and the provider object-size
limit remain acceptance gates; see
[VALIDATION.md](VALIDATION.md) for the tested scope.

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

The command set is `install`, `uninstall`, `backup-config`, `login`, `setup`,
`mount`, `stop`, `status`, `benchmark`, `sync`, `location`, `folders`,
`service`, `info`, `logs`, `trash`, and `empty-trash`. Service actions are
`enable`, `disable`, `start`, `stop`, `restart`, and `status`. Existing command
options remain available. `trash` lists account trash; `empty-trash`
separately performs account-wide deletion.

For the recommended contained installer, persistent user service, cached-email
login, mount settings, and complete local cleanup, see
[INSTALLATION.md](INSTALLATION.md). The manual steps below are an alternative
for users who manage their own paths and services.

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

Chunks default to 1 GiB. The provisional maximum encrypted object size is
19 GiB, including crypt headers and authentication tags, leaving a margin
below the historical 20 GiB report. This is a configuration safety cap, not a
verified provider limit. Choose a smaller cap if required by your account.

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

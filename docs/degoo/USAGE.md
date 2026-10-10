# Encrypted Degoo vault on Linux

This fork provides a `degoo` transport backend and a `rclone degoo mount`
entry point. The entry point always constructs:

```text
Linux FUSE -> VFS full cache -> chunker -> crypt -> Degoo
```

The implementation is experimental. Offline tests exercise the request shapes
and vault behavior. A disposable live vault passed encrypted publication,
fresh-cache readback, and CDN range reads. A live access token expired after an
hour and refresh returned HTTP 403; fresh interactive login is required for
that session. Long-term renewal, larger live files, and the provider object-size
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

The command set is `install`, `uninstall`, `login`, `setup`, `mount`, `stop`,
`status`, `location`, `folders`, `service`, `info`, `logs`, `trash`, and
`empty-trash`. Service actions are `enable`, `disable`, `start`, `stop`,
`restart`, and `status`. Existing command options remain available. `trash`
lists account trash; `empty-trash` separately performs account-wide deletion.

For the recommended contained installer, persistent user service, cached-email
login, mount settings, and complete local cleanup, see
[INSTALLATION.md](INSTALLATION.md). The manual steps below are an alternative
for users who manage their own paths and services.

## Setup

Build this fork with `go build`. Linux FUSE and `fusermount3` must be installed
to run the mount. Choose a dedicated Degoo folder and obtain its folder ID
without changing existing personal files. Use a separate private directory
for account tokens and the vault configuration:

```sh
install -d -m 700 ~/.config/rclone-degoo
install -d -m 700 ~/.cache/rclone-degoo
./rclone degoo login ~/.config/rclone-degoo/auth.json --email you@example.com
```

Login prompts for the account password. It saves access and refresh tokens in
an atomic, permission-protected file, and does not save the account password.
Accounts that cannot use email/password login need a Degoo-supported password
login or separately provisioned refresh token; browser-token extraction is
not provided.

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

Pass `--settings /private/path/mount.json` for persistent cache and verification
settings; the file must be owner-private, just like the vault configuration.
Explicit mount flags override corresponding settings. `--verify-upload=false`
skips full SHA-256 download verification before publication while retaining
metadata and publication checks. It defaults to true and is independent of
the encryption password and cache identity.

## Persistence and recovery

`fsync` syncs the local VFS cache. Close queues an asynchronous upload. Neither
operation promises that Degoo has already received the file. Keep the cache
directory across restarts and login failures. Dirty files are reloaded by VFS
and uploaded when the same vault is mounted again with working credentials.
New installs set `write_back` to one second, which starts a closed file's upload
sooner than rclone's five-second default when a transfer slot is available.
Existing settings without this field retain the five-second default. The value
must be positive and finite; `0s`, negative durations, and `off` are refused to
preserve background uploads and retries. Failed attempts double their retry
delay, starting from `write_back`. Upload verification downloads and hashes
each encrypted object before publishing it;
set `verify_upload` to `false` in the private mount settings to skip that extra
readback while retaining metadata and publication checks.

```sh
./rclone degoo status ~/.config/rclone-degoo/vault.json
```

Status counts dirty cached files and their logical byte sizes without reading
Degoo or displaying file contents. Counts can change during uploads. Upload
errors are also reported in the normal rclone logs. Zero pending uploads means
the local cache has no outstanding dirty files; it is not an independent
provider integrity audit.
Pending files may be queued, actively uploading, or waiting to retry a failed
attempt. A registration failure happens after byte transfer; check the logs
before attributing a long pending interval to the close delay or verification.

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

# Degoo vault client for Linux

The prebuilt archive's top-level README is built from
[DISTRIBUTION.md](DISTRIBUTION.md). This page documents the unified
`degoo-rc` command from a source checkout.

This installer provides a private, writable, encrypted Degoo mount. It keeps
the account credentials, vault secret, cache, logs, and command helpers under
`~/.local/share/rclone-degoo`; it does not change a normal rclone installation.
The user service and installer are local to this account. Uninstall never
deletes remote Degoo data.

## Requirements

- Linux and Python 3.10 or newer (needed by the installer and helpers).
- FUSE 3 or a compatible `fusermount` helper, plus readable and writable
  `/dev/fuse`.
- `mountpoint` from `util-linux`.
- A binary built from this fork, or the matching prebuilt Linux archive.
- Optional: a working systemd user manager for the background service.

Go 1.26 or newer is needed only to build the binary from source. The prebuilt
archive does not need Go. The installer checks the system dependencies and can
offer to install missing `fuse3` and `util-linux` packages using detected
`apt-get`, `dnf`, `pacman`, or `zypper`. Package installation may require root
or `sudo`; the shell launcher can also offer to install Python if it is
missing. If you decline mount dependencies, vault setup can still finish but
the service will not be enabled. If `/dev/fuse` is not available, the kernel
or system policy must be fixed separately.

Before each action, the wizard checks the prerequisites for the selected
binary, mountpoint, and service path. It distinguishes missing dependencies
from inaccessible or misconfigured ones, lists optional components such as
systemd, and prints system-specific setup, verification, and retry commands.
This includes FUSE tools, `util-linux`, and `/dev/fuse` access. An immutable
`/mnt/degoo` requires the manually installed root helper; the wizard can show
build and installation instructions from the private install root but never
clears the flag or changes privileged helper files itself. A known blocker
prevents service enablement and readiness reporting, while completed
configuration and pending cache data remain available for retry. See
[installation prerequisite recovery](../../docs/degoo/INSTALLATION.md) for
examples and verification steps.

## Install

To build from the `staging` source branch and start the wizard:

```sh
git clone --branch staging https://github.com/georgeasiminei/rclone-degoo.git
cd rclone-degoo
mkdir -p build
go build -o build/rclone .
./contrib/degoo/degoo-rc install --wizard --binary ./build/rclone
```

Go 1.26 or newer is needed only for this source-build path. In a terminal, no
arguments also starts the guided wizard. To explicitly start the wizard while
specifying a different binary:

```sh
./contrib/degoo/degoo-rc install --wizard --binary /path/to/rclone
```

From the matching prebuilt Linux amd64 archive, put
`rclone-degoo-linux-amd64.tar.gz` in `~/Downloads`. Extract it into a new
temporary directory and use its top-level launcher:

```sh
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf "$HOME/Downloads/rclone-degoo-linux-amd64.tar.gz" -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./degoo-rc install
```

The archive can be shared with other Linux amd64 users; it contains no Degoo
account credentials or vault secret. No GitHub release archive has been
uploaded yet. The wizard checks dependencies, installs or upgrades the
contained client, logs into Degoo, and offers to use an existing dedicated
folder or create a new one. It suggests the first discovered device folder
and names a new vault `rclone-degoo-vault` by default. Root ID `0` is used to
list device folders; it cannot be used as the vault folder or as the parent of
a live create request.
The wizard suggests enabling the user service by default when the systemd user
manager is available. Otherwise, mount manually in the foreground.

To stage an install without opening the wizard, pass `--binary` and
`--no-wizard`:

```sh
./contrib/degoo/degoo-rc install --binary ./build/rclone --no-wizard
```

Then log in and create a dedicated child folder under an actual device ID:

```sh
degoo-rc login
degoo-rc folders --parent-id 0
degoo-rc setup --parent-id DEVICE_ID --name rclone-degoo-vault
degoo-rc service enable
```

The default install root is `~/.local/share/rclone-degoo`. Existing vaults can
be imported using their folder ID and private `vault.json`. The encryption
secret, optional salt, chunk size, and object cap must match the remote vault;
local remote/cache paths are rewritten for this installation. Keep the vault
config backed up privately outside the install directory.

Create a configuration backup without uninstalling or stopping the mount:

```sh
degoo-rc backup-config --directory ~/private-backups/rclone-degoo
```

With no `--directory`, the command chooses a new timestamped private directory.
This backs up configuration and credentials only: the vault key, saved account
login, and settings. File data, cache, and pending uploads are excluded. Let
uploads finish before expecting those changes on another computer. Transfer
the backup privately because it contains the encryption key and account tokens.

To restore this backup or one retained by the uninstall wizard into a fresh
installation on another computer, pass its directory to the installer:

```sh
./contrib/degoo/degoo-rc install --wizard \
  --restore-directory ~/private-backups/rclone-degoo
```

`--restore-directory` requires `--wizard`, and restoration is refused when the
default install root already exists. The wizard confirms the recovered folder
ID and name and uses the saved mount settings as defaults. It preserves the
original vault encryption settings, restores saved account authentication and
email, and copies an optional retained cache into the new install. Cache
identity is rebound locally so it can be used from the new path; the backup
directory remains untouched. Keep the backup until recovery and mount startup
are complete. If you omit the option, the wizard offers an optional backup
directory prompt; leave it blank for a normal installation.

The wizard checks saved account credentials by listing Degoo folders. If that
authenticated request succeeds, it skips the account password prompt. If
Degoo explicitly rejects the saved login, the wizard asks you to log in and
checks the connection again. A provider or network failure stops setup without
being reported as a wrong password. This checks Degoo account authentication,
not whether the vault password can decrypt remote data; the retained
`vault.json` supplies the original vault encryption settings.

## Defaults and use

To refresh another client's remote changes immediately, run `degoo-rc sync .`
from inside the mounted directory, or pass an absolute mounted path. It
refreshes the selected directory tree and preserves pending local writes;
reopen files to see changed content. File paths refresh their containing
directory. This command refreshes metadata without mirroring directories or
predownloading file contents. Upgrade and restart an older mount first.

The default directory listing cache lasts 30 minutes and refreshes on access;
Degoo does not support remote change polling. There is no strict end-to-end
deadline for another client's changes: provider visibility, request failures,
open handles, and a separate folder-ID cache can delay them. The five-second
cache cleanup interval and 750-hour file retention do not control this delay.
See [remote changes and cache staleness](../../docs/degoo/USAGE.md#remote-changes-and-cache-staleness).

New vaults use 128 MiB plaintext chunks and a provisional 19 GiB encrypted object
cap. Live Degoo limits above 20 GiB have not been verified. Initial
`config/mount.json` values are:

| Setting | Default | Description |
| --- | --- | --- |
| `cache_max_size` | `60G` | Soft cache-size target; open and pending files stay cached. |
| `cache_min_free_space` | `60G` | Free disk space to preserve on the cache filesystem when evicting clean data. |
| `cache_max_age` | `750h` | Age after which clean cache data becomes eligible for eviction. |
| `cache_poll_interval` | `5s` | How often the local cache cleaner checks for stale clean files. |
| `write_back` | `5s` | Wait after a file closes before attempting its upload. Must be positive. |
| `verify_upload` | `false` | Enable readback of each encrypted upload and compare its size and SHA-256 before publishing it. |
| `buffer_size` | `16Mi` | RAM read buffer per open file. |
| `read_ahead` | `1Mi` | Additional advance download into the full VFS disk cache. |
| `read_chunk_size` | `128Mi` | Network read-request window, independent of stored vault chunks. |
| `read_chunk_size_limit` | `off` | Maximum adaptive read-window growth when `read_chunk_streams` is zero. |
| `read_chunk_streams` | `8` | Concurrent fixed-size read streams; zero selects adaptive single-stream reads. |
| `dir_cache_time` | `30m` | Directory metadata freshness interval; mount changes appear immediately. |
| `attr_timeout` | `1s` | Kernel file/directory attribute caching interval. |
| `transfers` | `4` | Maximum simultaneous background file uploads. |
| `bwlimit` | `off` | Aggregate bandwidth limit; accepts rclone rates and timetables. |
| `timeout` | `5m` | Network I/O idle timeout, not a total transfer deadline. |

The additional buffering, reading, directory, attribute, concurrency, bandwidth,
and timeout settings are configured only by editing `config/mount.json`; the
wizard adds no prompts for them. Existing values survive wizard upgrades.
Missing fields use the defaults above. Explicit command-line flags take
precedence. Restart the mount or service after editing this private file.
Eight read streams are a starting point for a fast connection, not a throughput
guarantee: each can request a 128 MiB range, so scattered reads may waste
bandwidth. Set `read_chunk_streams` to zero for adaptive single-stream reads,
or reduce streams and `read_chunk_size` when partial reads dominate. The
`read_ahead` value does not cap parallel request sizes.

Cache size and free-space targets are independent. With a 40 GiB cache and
100 GiB free, neither 60 GiB target triggers eviction. With only 50 GiB free,
the cleaner tries to reclaim 10 GiB from clean, closed cached files. Open and
pending files remain protected; these targets cannot guarantee against a full
disk. Cache age is measured since last access, and size or free-space pressure
can evict clean files before that age is reached.

Run `degoo-rc mount` for a foreground mount, or `degoo-rc service enable` to
start the user service. Edit `config/mount.json` and restart the mount or
service to apply settings. Cache bounds are soft targets; open and dirty files
are not evicted. Reduce `write_back` to queue closed files sooner. Setting
`verify_upload` to `false` skips the full download and hash check while keeping
metadata and publication checks enabled.
Existing settings are preserved during ordinary upgrades; an omitted
`write_back` keeps rclone's five-second default. Zero, negative, and `off` values
are refused. Failed uploads use exponential retry delays based on `write_back`.
Keep the cache until `degoo-rc status` reports zero pending uploads unless the
uninstall wizard is making a complete private recovery copy of both config and
cache.

`degoo-rc login` remembers the account email in a private file but never saves the
account password. `degoo-rc login --clear-email` forgets that email without deleting
tokens or vault settings. Token renewal is automatic when supported, but
long-term unattended renewal remains unverified.

`degoo-rc info` prints a local mount, service, cache, pending-upload, and disk report.
`degoo-rc info --check-remote` additionally performs a metadata-only lookup of the
vault and its ancestors; it may refresh authentication but does not download
file data or calculate remote size.

Use `degoo-rc status` for authentication-first health and upload status. An
unhealthy result exits nonzero; `--json` returns machine-readable output. Pass
a file path to report its current upload state:

```sh
degoo-rc status
degoo-rc status /mnt/degoo/report.pdf --json
```

The per-file values are `pending`, `uploading`, `uploaded`, and `unknown`.
`uploaded` means the current VFS version has a known remote representation
after writeback; it is not a fresh cloud integrity check. Generic VFS mounts
leave the read-only `user.rclone.upload-status` attribute disabled by default;
the Degoo mount enables it. A file created below an unmounted mountpoint is
local-only and is never queued for upload. The mount refuses to cover a
nonempty directory.

See [installation and cleanup](../../docs/degoo/INSTALLATION.md) for the full
dependency, mount, cache, upgrade, and recovery guide. See
[the quick start](../../DEGOO.md) and [usage](../../docs/degoo/USAGE.md) for
configuration and commands.

## Account trash

`degoo-rc trash` lists account-wide trash. `degoo-rc empty-trash` is the
separate destructive command. The auth file defaults to the contained
installation's `config/auth.json`; `--auth` can select another private file.
The command passes the auth path through without reading or displaying its
contents:

```sh
degoo-rc trash --auth ~/.config/rclone-degoo/auth.json --limit 10 --json
degoo-rc empty-trash --auth ~/.config/rclone-degoo/auth.json --dry-run
degoo-rc empty-trash --auth ~/.config/rclone-degoo/auth.json --yes
```

Trash is account-wide rather than vault-scoped.
Emptying it is irreversible, prompts twice unless `--yes` is supplied, and can
delete unrelated account trash. Dry-run only lists the candidates. Generic
`rclone cleanup` and restore are deliberately unsupported. Use an isolated
disposable account for destructive validation.

## Find your mount and logs

The wizard ends with the install and mount paths, actual mount state, helper
commands, and log locations. The default mount is `/mnt/degoo`. Open that
directory while it is mounted. New installs create it owner-private; if the
installer cannot prepare it, it prints the privileged setup commands. Upgrades
preserve their saved mount path. To migrate an older install, choose
`--mountpoint /mnt/degoo`; migration is refused while the old underlying
directory contains files. The external mount directory remains after uninstall.
The privileged helper does not support an immutable legacy `mount/` target.
For an install whose saved mountpoint is immutable, run
`degoo-rc install --mountpoint /mnt/degoo` to migrate the saved path while
preserving the cache. The old underlying `mount/` directory must be empty.
Migration does not clear an immutable flag; the root helper supports only
`/mnt/degoo`. An immutable legacy directory remains protected and may need a
separate manual unlock before uninstall cleanup.

You may leave `chattr +i` set on `/mnt/degoo` while mounted and unmounted. The
flag blocks the write-access check required by standard `fusermount3`, even
when the directory is owner-writable. Mounting with the flag retained requires
the administrator-installed helper in
[immutable-mount/README.md](immutable-mount/README.md); the installer never
clears the flag or installs privileged files. The helper is limited to an
empty `/mnt/degoo` owned by the invoking user and not writable by group or
other users; the default directory remains owner-private. It mounts with that
user's ID, rejects `allow_other` and `allow_root`, and leaves rclone running as
the ordinary user. The mount launcher checks the helper's root ownership,
protected permissions, and setuid bit, then selects it only for an immutable
mount. Ordinary mutable mounts continue using the system FUSE helper. When the
immutable flag is absent, the mount protects the underlying directory by
removing write permissions after mounting and on failure or unmount.

After an administrator installs the helper, retry a new service setup with
`degoo-rc service enable`. If the service is already enabled, restart it with
`degoo-rc service restart`. If the systemd user manager is unavailable, run
`degoo-rc mount` for a foreground mount after repairing the blocker.
The installed `degoo-rc` command is available on `PATH`:

```sh
degoo-rc info
degoo-rc logs
degoo-rc logs --follow
```

Press Ctrl+C to stop following logs. `degoo-rc logs` is the viewer; `logs/` is a
directory. The private `status.log` and `diagnostics.log` contain status and
Degoo runtime diagnostics. Displayed command errors omit Cobra usage blocks.
New mounts log notices and errors by default. The upgrade wizard
offers to stop the contained mount and waits briefly for it to start again.
To stop the user service manually:

```sh
degoo-rc service stop
```

If authentication cannot renew, status reports a login-required error with the
login command and returns nonzero. Earlier renewal attempts returned HTTP 403
and HTTP 401; authentication was later renewed, but reliable automatic renewal
is still unverified. Run `degoo-rc login` again. Its
password prompt stays visible in the terminal while logs go to a file, and
password characters remain hidden. Keep the cache and matching vault
configuration while uploads are pending.

## Uninstall

Use the explicit inventory mode to inspect the install without changing it:

```sh
degoo-rc uninstall --inventory
```

Running `degoo-rc uninstall` in a terminal, or with `--wizard`,
starts guided cleanup. It offers to copy vault/account configuration, cache,
and logs to a private directory outside the install; each choice defaults to
No. You can enter a backup path or use the suggested timestamped directory.

A private backup is an owner-only folder outside the install, created only
when you choose to keep something. At its path prompt, press Enter for the
suggested path or type an absolute path; `N` is not a path. To remove all local
state without a backup, choose No for configuration, cache, and logs, then
confirm `DISCARD` and `REMOVE`. Start cleanup from outside the install
(`cd ~` first). Deleting the encryption settings means a fresh setup should
create a new dedicated vault folder with a new name.
Leave the existing folder ID and vault-config import prompts blank.

If uploads are pending, cleanup refuses to discard their cache. To remove the
local install while preserving those changes, retain both the cache and its
matching configuration. If you choose not to retain the vault config, type
`DISCARD` to confirm losing the local secret. Type `REMOVE` to confirm local
cleanup. Restore a retained backup through the wizard into a fresh install by
passing its directory with `--restore-directory` as shown above. The wizard
copies retained state into the new install and leaves the selected backup in
place; it does not require copying the backup contents over an install path.
When pending uploads were retained, the restored cache keeps them available
for the mount's upload queue.

For an ordinary in-place client upgrade, use this command from a source
checkout instead:

```sh
./contrib/degoo/degoo-rc install --binary ./build/rclone --upgrade --no-wizard
```

Cleanup stops the service linked to this install, removes its owned systemd
links, reloads the user manager, and deletes the contained local files. Remote
encrypted files, the external `/mnt/degoo` directory, account-wide sessions,
and lingering settings remain
untouched. Empty shared directories under the user systemd config path may remain.

## Command reference and layout

Use `degoo-rc help` to list commands. Run `degoo-rc COMMAND help` for that
command's options and examples. The interface includes `install`, `uninstall`,
`login`, `setup`, `mount`, `stop`, `status`, `location`, `folders`, `service`,
`info`, `logs`, `trash`, and `empty-trash`. `service` accepts `enable`,
`disable`, `start`, `stop`, `restart`, and `status`. Existing command options
remain available; command help is the reference for their current parameters.

The installer places the entrypoint at
`~/.local/share/rclone-degoo/degoo-rc` and creates
`~/.local/bin/degoo-rc` as a link to it. It does not edit shell profile files.
If `~/.local/bin` is not on `PATH`, it prints the `PATH` export to add for the
current shell or to copy into a profile yourself. Uninstall removes the link
only when it still points to this installation. The installed command dispatches
to private helpers in `scripts/`, with Python implementation files in
`scripts/python/`. A fresh install keeps `degoo-rc` as its only root-level
Degoo command. Upgrades may retain root-level `.sh` shims already present in an
older install. The Degoo package in a source checkout and the extracted archive
use `degoo-rc` as their sole installer and maintenance entrypoint. Use
`degoo-rc` in new commands and examples.

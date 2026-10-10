# Encrypted Degoo drive for Linux

This fork mounts a dedicated Degoo folder as an encrypted, writable Linux
filesystem. File data, names, and chunk manifests are encrypted before upload.
You can list, read, write, rename, move, and delete files within the vault.
Copies download and upload data; server-side copying is unavailable.

## Install

The installer needs Python 3.10 or newer. To mount, Linux also needs FUSE 3 or
a compatible `fusermount` helper, a readable and writable `/dev/fuse`, and
`mountpoint` from `util-linux`. A systemd user manager is optional and is used
only for the background service. The wizard checks mount dependencies and can
offer to install missing `fuse3` and `util-linux` packages through a detected
`apt-get`, `dnf`, `pacman`, or `zypper`. Go 1.26 or newer is needed only to
build from source. No GitHub release archive has been uploaded yet.

To build from the `staging` source branch and start the guided installer:

```sh
git clone --branch staging https://github.com/georgeasiminei/rclone-degoo.git
cd rclone-degoo
mkdir -p build
go build -o build/rclone .
./contrib/degoo/install.sh --wizard --binary ./build/rclone
```

If you have the matching prebuilt Linux amd64 archive, place
`rclone-degoo-linux-amd64.tar.gz` in `~/Downloads`. No archive is currently
available as a GitHub release download. Extract it into a new temporary
directory, then run the packaged launcher:

```sh
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf "$HOME/Downloads/rclone-degoo-linux-amd64.tar.gz" -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./install.sh
```

The archive can be shared with other Linux amd64 users; it includes the
prebuilt client and installer files but no account credentials or vault secret.
The prebuilt path does not require Go. With no arguments in a terminal, either
launcher opens the wizard. For a staged install, use the noninteractive
`--binary` option and add `--no-wizard`:

```sh
./contrib/degoo/install.sh --binary ./build/rclone --no-wizard
```

The private install defaults to `~/.local/share/rclone-degoo`. The wizard
offers to install dependencies, authenticate, and select or create a vault.
It queries root ID `0` to list device folders, then suggests a discovered
device ID for a new vault and `rclone-degoo-vault` as the child folder name.
Live create requests under root ID `0` are rejected, and root `0` is never used
as the vault folder. The wizard can also configure an existing
dedicated folder or import a vault's private configuration.

The first mount can be enabled as a systemd user service when a user manager is
available:

```sh
~/.local/share/rclone-degoo/service.sh enable
~/.local/share/rclone-degoo/info.sh
```

For manual foreground mounting, run `mount.sh`. The service starts at login.
Enable account lingering separately if you need it to start before login and
remain active after logout; the installer does not change lingering.

## Mount and use

The wizard ends with the install directory, mount path, actual mount state,
and commands for status, logs, login, and service control. The default mount
is `~/.local/share/rclone-degoo/mount`; open that folder in your file manager
or use it as an ordinary directory while mounted. All helper scripts are
under `~/.local/share/rclone-degoo/`, regardless of where you extracted the
installer.

```sh
~/.local/share/rclone-degoo/info.sh
~/.local/share/rclone-degoo/logs.sh
~/.local/share/rclone-degoo/logs.sh --follow
```

Press Ctrl+C to stop following logs. `logs.sh` is the viewer; `logs/` is a
directory. New mounts log notices and errors by default. The upgrade wizard
offers to stop the contained mount and waits briefly for it to start again.
To stop the user service manually:

```sh
~/.local/share/rclone-degoo/service.sh stop
```

`logs.sh` shows recent mount, login, setup, and status logs, and `--follow`
watches new lines.
If authentication cannot be renewed, `info.sh` shows a login-required warning.
Run `~/.local/share/rclone-degoo/login.sh` again; the mount can reload the
saved credentials. Keep pending files and their matching vault configuration
until remote uploads complete.

See [installation and cleanup](docs/degoo/INSTALLATION.md) for existing vaults,
dependency setup, service operation, settings, and recovery.

## Account trash

Degoo trash is account-wide, not limited to the configured vault. The explicit
commands below take the account-auth file directly and never appear as a normal
remote path:

```sh
./rclone degoo trash ~/.config/rclone-degoo/auth.json
./rclone degoo empty-trash ~/.config/rclone-degoo/auth.json --dry-run
./rclone degoo empty-trash ~/.config/rclone-degoo/auth.json --yes
```

`trash` lists all recycled items, `--dry-run` previews what emptying would
delete, and `empty-trash` permanently removes them after two confirmations
unless `--yes` is supplied. There is no restore operation.

See [usage](docs/degoo/USAGE.md) and the packaged `trash.sh` helper for details.

## Cache and verification

Edit `~/.local/share/rclone-degoo/config/mount.json` and restart the mount or
service to change cache behavior. Defaults are a 20 GiB cache-size target, 20
GiB minimum free space, 10 minute maximum age for clean cache data, and a 5
second remote-change poll interval. Open or dirty files cannot be evicted to
meet cache targets.

Upload verification is enabled by default. It downloads each encrypted upload
and compares the size and SHA-256 before publication. Set `verify_upload` to
`false`, or pass `--verify-upload=false` to `mount.sh`, to skip the payload
readback. Metadata and publication checks remain enabled. Disabling verification
saves time and bandwidth but leaves corrupted upload contents undetected until
read.

A newly written file is staged in full locally before upload. A 10 TB
collection can fit in the cloud when each file and the pending local writes fit
the cache; one 10 TB write cannot fit in a 20 GiB cache. Keep the cache until
`status.sh` reports zero pending uploads, or retain both cache and matching
configuration when making a private recovery copy during uninstall.

## Uninstall

Inspect local install state without changing it:

```sh
~/.local/share/rclone-degoo/uninstall.sh --inventory
```

In a terminal, running `uninstall.sh` with no arguments or with `--wizard`
starts guided cleanup. The wizard offers to retain configuration, cache, and
logs in a private external backup directory; each choice defaults to No. It
refuses to discard a cache containing pending uploads unless it preserves the
cache together with the matching config. Cleanup removes local files and
service links only; remote encrypted files and Degoo account data are left
untouched.

## Share the software; preserve the vault secret

The prebuilt archive contains the Linux binary, shell launchers, Python helper
sources, license, and documentation. Go is not needed to install or use it.
There is no separate published Degoo release artifact at present. Each user
should create or import their own dedicated vault and log in with their own
Degoo account.

To access an existing vault from another machine, securely transfer
`config/vault.json` and use the same remote folder ID. The password and format
settings must stay unchanged. Use one writable mount per vault; writers on
separate machines are not coordinated.

## Current validation limits

This implementation remains experimental. Live encrypted writes, readback,
range reads, and file/directory operations passed against a dedicated
disposable folder. The tested account's one-hour access token expired and
renewal returned HTTP 403; unattended long-term authentication has not been
established. Larger live files, large directory pagination, and interrupted
multi-chunk recovery also remain acceptance gates. See
[validation](docs/degoo/VALIDATION.md).

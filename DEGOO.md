# Encrypted Degoo drive for Linux

This fork mounts a dedicated Degoo folder as an encrypted, writable Linux
filesystem. File data, names, and chunk manifests are encrypted before upload.
You can list, read, write, rename, move, and delete files within the vault.
Copies download and upload data; server-side copying is unavailable.

## Install and mount

The installer needs Python 3.10 or newer. To mount, Linux also needs FUSE 3 or
a compatible `fusermount` helper, a readable and writable `/dev/fuse`, and
`mountpoint` from `util-linux`. A systemd user manager is optional and is used
only for the background service. The wizard checks mount dependencies and can
offer to install missing `fuse3` and `util-linux` packages through a detected
`apt-get`, `dnf`, `pacman`, or `zypper`. Go 1.26 or newer is needed only to
build from source.

From this repository, build and start the guided installer:

```sh
go build -o rclone .
./contrib/degoo/install.sh --wizard --binary ./rclone
```

From the matching prebuilt Linux archive, extract it and run `./install.sh`.
With no arguments in a terminal, either launcher opens the wizard. For a staged
install, keep the noninteractive `--binary` option and add `--no-wizard`:

```sh
./contrib/degoo/install.sh --binary ./rclone --no-wizard
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

See [installation and cleanup](docs/degoo/INSTALLATION.md) for existing vaults,
dependency setup, service operation, settings, and recovery.

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

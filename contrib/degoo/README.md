# Degoo vault installer for Linux

The prebuilt archive's top-level README is built from
[DISTRIBUTION.md](DISTRIBUTION.md). This page documents the installer from the
source checkout.

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

## Install

To build from the `staging` source branch and start the wizard:

```sh
git clone --branch staging https://github.com/georgeasiminei/rclone-degoo.git
cd rclone-degoo
mkdir -p build
go build -o build/rclone .
./contrib/degoo/install.sh --wizard --binary ./build/rclone
```

Go 1.26 or newer is needed only for this source-build path. In a terminal, no
arguments also starts the guided wizard. To explicitly start the wizard while
specifying a different binary:

```sh
./contrib/degoo/install.sh --wizard --binary /path/to/rclone
```

From the matching prebuilt Linux amd64 archive, put
`rclone-degoo-linux-amd64.tar.gz` in `~/Downloads`. Extract it into a new
temporary directory and use its top-level launcher:

```sh
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf "$HOME/Downloads/rclone-degoo-linux-amd64.tar.gz" -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./install.sh
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
./contrib/degoo/install.sh --binary ./build/rclone --no-wizard
```

Then log in and create a dedicated child folder under an actual device ID:

```sh
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/folders.sh --parent-id 0
~/.local/share/rclone-degoo/setup.sh --parent-id DEVICE_ID --name rclone-degoo-vault
~/.local/share/rclone-degoo/service.sh enable
```

The default install root is `~/.local/share/rclone-degoo`. Existing vaults can
be imported using their folder ID and private `vault.json`. The encryption
secret, optional salt, chunk size, and object cap must match the remote vault;
local remote/cache paths are rewritten for this installation. Keep the vault
config backed up privately outside the install directory.

## Defaults and use

New vaults use 1 GiB plaintext chunks and a provisional 19 GiB encrypted object
cap. Live Degoo limits above 20 GiB have not been verified. Initial
`config/mount.json` values are:

| Setting | Default | Description |
| --- | --- | --- |
| `cache_max_size` | `20G` | Soft cache-size target; open and pending files stay cached. |
| `cache_min_free_space` | `20G` | Free disk space to preserve on the cache filesystem when evicting clean data. |
| `cache_max_age` | `10m` | Age after which clean cache data becomes eligible for eviction. |
| `cache_poll_interval` | `5s` | Interval for checking remote changes. |
| `verify_upload` | `true` | Read back each encrypted upload and compare its size and SHA-256 before publishing it. |

Run `mount.sh` for a foreground mount, or `service.sh enable` to start the user
service. Edit `config/mount.json` and restart the mount or service to apply
settings. Cache bounds are soft targets; open and dirty files are not evicted.
Keep the cache until `status.sh` reports zero pending uploads unless the
uninstall wizard is making a complete private recovery copy of both config and
cache.

`login.sh` remembers the account email in a private file but never saves the
account password. `login.sh --clear-email` forgets that email without deleting
tokens or vault settings. Token renewal is automatic when supported, but
long-term unattended renewal remains unverified.

`info.sh` prints a local mount, service, cache, pending-upload, and disk report.
`info.sh --check-remote` additionally performs a metadata-only lookup of the
vault and its ancestors; it may refresh authentication but does not download
file data or calculate remote size.

See [installation and cleanup](../../docs/degoo/INSTALLATION.md) for the full
dependency, mount, cache, upgrade, and recovery guide. See
[the quick start](../../DEGOO.md) and [usage](../../docs/degoo/USAGE.md) for
configuration and commands.

## Uninstall

Use the explicit inventory mode to inspect the install without changing it:

```sh
~/.local/share/rclone-degoo/uninstall.sh --inventory
```

Running `uninstall.sh` with no arguments in a terminal, or with `--wizard`,
starts guided cleanup. It offers to copy vault/account configuration, cache,
and logs to a private directory outside the install; each choice defaults to
No. You can enter a backup path or use the suggested timestamped directory.
If uploads are pending, cleanup refuses to discard their cache. To remove the
local install while preserving those changes, retain both the cache and its
matching configuration. If you choose not to retain the vault config, type
`DISCARD` to confirm losing the local secret. Type `REMOVE` to confirm local
cleanup. Restore a pending-upload recovery backup to
`~/.local/share/rclone-degoo`, then run the following from a source checkout to
recreate the installed launchers and resume work:

```sh
./contrib/degoo/install.sh \
  --binary ~/.local/share/rclone-degoo/bin/rclone --upgrade --no-wizard
```

Cleanup stops the service linked to this install, removes its owned systemd
links, and deletes only local files selected for removal. Remote encrypted
files, account-wide sessions, and lingering settings remain untouched. Empty
shared directories under the user systemd config path may remain.

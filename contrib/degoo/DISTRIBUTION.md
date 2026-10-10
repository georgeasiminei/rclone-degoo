# Degoo vault installer for Linux

This is the README shipped at the root of the prebuilt archive. The archive
contains the Linux amd64 client, installer, helper scripts, license, and
documentation. It does not contain Degoo account credentials, vault
configuration, cache data, or a vault secret. The same tarball can be shared
with other Linux amd64 users; it is not currently uploaded as a GitHub release
asset.

## Requirements

The installer and helpers need Linux and Python 3.10 or newer. The shell
launcher can offer to install Python if it is unavailable. To mount, the
system also needs FUSE 3 or a compatible `fusermount` helper, readable and
writable `/dev/fuse`, and `mountpoint` from `util-linux`. A systemd user
manager is optional and is used only for the background service. Go 1.26 or
newer is needed only to build from source; it is not needed to install or run
this archive.

The wizard checks for the mount dependencies and can offer to install missing
`fuse3` and `util-linux` packages using detected `apt-get`, `dnf`, `pacman`, or
`zypper` package managers. Package installation may require root or `sudo`.
If mount dependencies remain unavailable, vault setup can still finish but the
service will not be enabled. The installer cannot make `/dev/fuse` available
when it is disabled by the kernel or system policy.

## Install from the archive

Place `rclone-degoo-linux-amd64.tar.gz` and its `.sha256` sidecar in
`~/Downloads`. Verify the archive checksum, then extract it into a new
temporary directory so files from an older package cannot remain alongside
the installer:

```sh
cd "$HOME/Downloads"
sha256sum -c rclone-degoo-linux-amd64.tar.gz.sha256
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf "$HOME/Downloads/rclone-degoo-linux-amd64.tar.gz" -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./install.sh
```

With no options in a terminal, `install.sh` starts the setup wizard. It checks
dependencies, installs or upgrades the contained client, logs into Degoo, and
helps select an existing dedicated folder or create a new one. It suggests a
discovered device folder and names a new vault `rclone-degoo-vault` by default.
Account root ID `0` is used to list device folders; it cannot be the vault
folder or the parent of a live create request. Each user should use their own
Degoo account and create or import their own dedicated vault.

## Install from source

Clone the fork's `staging` branch, build its Degoo-enabled client, and start
the same wizard:

```sh
git clone --branch staging https://github.com/georgeasiminei/rclone-degoo.git
cd rclone-degoo
mkdir -p build
go build -o build/rclone .
./contrib/degoo/install.sh --wizard --binary ./build/rclone
```

For an unattended or staged source install, keep the `--binary` option and add
`--no-wizard`:

```sh
./contrib/degoo/install.sh --binary ./build/rclone --no-wizard
```

Then log in and finish setup using the contained helpers:

```sh
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/folders.sh --parent-id 0
~/.local/share/rclone-degoo/setup.sh --parent-id DEVICE_ID --name rclone-degoo-vault
~/.local/share/rclone-degoo/service.sh enable
```

## Mount, upgrade, and recover

The private install root is `~/.local/share/rclone-degoo`. The wizard offers
to enable a systemd user service when a user manager is available. Otherwise,
use `~/.local/share/rclone-degoo/mount.sh` for a foreground mount. The
`config/mount.json` file holds cache and upload settings. New vaults use 1 GiB
plaintext chunks and a provisional 19 GiB encrypted object cap. The live
provider limit above 20 GiB has not been verified.

Stop the service or mount before upgrading. From the package root, run the
launcher against the packaged binary:

```sh
./install.sh --binary ./bin/rclone --upgrade --no-wizard
```

Upgrade preserves the vault secret, account tokens, email, remote and mount
settings, and cache. It refuses a change to the vault folder ID. `login.sh`
remembers the account email but never saves the account password; token renewal
is automatic when supported, though long-term unattended renewal remains
unverified.

Run `~/.local/share/rclone-degoo/info.sh` to inspect mount, service, cache, and
pending-upload status. Keep the cache until pending uploads reach zero. A
locally saved file may still await remote upload; pending changes and their
matching vault configuration are both needed to resume work after recovery.

## Uninstall

Inspect the install without changing it:

```sh
~/.local/share/rclone-degoo/uninstall.sh --inventory
```

In a terminal, running `uninstall.sh` with no arguments or with `--wizard`
starts guided cleanup. It offers to retain the vault/account configuration,
cache, and logs in a private backup directory; each choice defaults to No. If
uploads are pending, cleanup refuses to discard the cache. To recover those
changes, retain both the cache and its matching configuration. Restore the
backup to `~/.local/share/rclone-degoo`, then run the package launcher with
`--binary ~/.local/share/rclone-degoo/bin/rclone --upgrade --no-wizard` to
recreate the installed launchers.

If you choose not to retain the vault configuration, type `DISCARD` to confirm
losing the local secret. Type `REMOVE` to confirm local cleanup. Uninstall
stops the service linked to this install and removes its owned local files and
systemd links. It never deletes remote encrypted files or Degoo account data.

See [installation and cleanup](docs/INSTALLATION.md) for full setup,
permissions, service, cache, upgrade, and recovery details. See
[usage](docs/USAGE.md) for configuration, commands, and current validation
limits.

## Archive contents

```text
rclone-degoo/
  README.md
  install.sh
  uninstall.sh
  bin/rclone
  scripts/python/*.py
  docs/*.md
  docs/BUILD.json
  licenses/COPYING
  licenses/degoo-NOTICE
```

The six Markdown files in `docs/` are copied from `docs/degoo/` and flattened
to that directory. `docs/BUILD.json` records package provenance and the
included binary's SHA-256. The two license files are the repository's `COPYING`
and the Degoo backend notice. The `.tar.gz.sha256` checksum sidecar is next to
the archive, not inside it.

# Degoo vault installer for Linux

This is the README shipped at the root of the prebuilt archive. The archive
contains the Linux amd64 client, unified `degoo-rc` command, helper scripts,
license, and documentation. It does not contain Degoo account credentials, vault
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
./degoo-rc install
```

With no options in a terminal, `degoo-rc install` starts the setup wizard. It
checks dependencies, installs or upgrades the contained client, logs into Degoo,
and helps select an existing dedicated folder or create a new one. It suggests a
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
./contrib/degoo/degoo-rc install
```

Pass `--binary ./build/rclone` to choose a specific binary.

To mount the same vault on another computer, create a configuration backup on
the configured computer, then transfer it privately:

```sh
degoo-rc backup-config --directory ~/private-backups/rclone-degoo
```

This backs up configuration and credentials only, including the vault key and
account tokens. File data, cache, and pending uploads are excluded. On the new
computer, the installer offers a backup directory as its first prompt, or use:

```sh
./degoo-rc install --wizard --restore-directory ~/private-backups/rclone-degoo
```

Recovered settings become defaults and a successful saved-account connection
check skips the password prompt. Restoration requires a fresh local install.

For an unattended or staged source install, keep the `--binary` option and add
`--no-wizard`:

```sh
./contrib/degoo/degoo-rc install --binary ./build/rclone --no-wizard
```

Then log in and finish setup with the installed command:

```sh
degoo-rc login
degoo-rc folders --parent-id 0
degoo-rc setup --parent-id DEVICE_ID --name rclone-degoo-vault
degoo-rc service enable
```

## Remote changes

To refresh remote changes from another client, run `degoo-rc sync .` while
inside the mounted directory, or pass an absolute mounted file/directory path.
The selected directory tree is refreshed and pending local writes remain
intact. Reopen files to see changed content. This refreshes metadata; it does
not mirror directories or predownload file contents. Older mounts need an
upgrade and restart to enable this command.

Directory cache staleness is normally up to the remaining 30-minute listing
TTL plus the next successful request on access. The five-second cache cleanup
interval does not check remote changes. There is no strict end-to-end maximum
because provider visibility, open handles, and folder-ID mappings can delay
external changes. See `docs/USAGE.md` for details.

## Account trash

The unified command provides separate list and deletion operations for
account-wide Degoo trash:

```sh
degoo-rc trash --auth ~/.config/rclone-degoo/auth.json --limit 10 --json
degoo-rc empty-trash --auth ~/.config/rclone-degoo/auth.json --dry-run
degoo-rc empty-trash --auth ~/.config/rclone-degoo/auth.json --yes
```

Trash is account-wide, not vault-scoped. Emptying it is irreversible and prompts
twice unless `--yes` is supplied. The command does not read or print the auth
file contents. There is no restore operation.

## Mount, upgrade, and recover

At the end of setup, the wizard prints the install directory, mount path,
actual mount state, helper commands, and log locations. The `degoo-rc`
command is installed on `PATH`; the default mount path is
`~/.local/share/rclone-degoo/mount`; open that directory in your file manager
or use it like any other directory while it is mounted.

```sh
degoo-rc info
degoo-rc logs
degoo-rc logs --follow
```

`degoo-rc logs` shows recent mount, login, setup, and status logs. Use
`--follow` to watch new messages; press Ctrl+C to stop following. The `logs/`
path is a directory, while `degoo-rc logs` is the viewer. New mounts log
notices and errors by default; routine cache-cleanup messages require a
foreground mount with `degoo-rc mount --log-level INFO`. If authentication
cannot renew, `degoo-rc info` shows a login-required warning with the login
command. Run `degoo-rc login`
again; the mount can reload the saved credentials. An expired access token
alone does not establish a failure when automatic renewal is still possible.

The private install root is `~/.local/share/rclone-degoo`. The installer puts
the entrypoint at `~/.local/share/rclone-degoo/degoo-rc` and links it from
`~/.local/bin/degoo-rc`. It does not edit shell profiles; when needed, it prints
a `PATH` export for the current shell or for you to add yourself. Uninstall
removes the link when it still points to this installation. The wizard offers
to enable a systemd user service when a user manager is available. Otherwise,
use `degoo-rc mount` for a foreground mount. The
`config/mount.json` file holds cache and upload settings. New vaults use 1 GiB
plaintext chunks and a provisional 19 GiB encrypted object cap. The live
provider limit above 20 GiB has not been verified.

The upgrade wizard offers to stop the contained service or mount before
collecting settings. It waits briefly for the mount after starting the service
and reports the observed state. Command labels in its summary are shell
comments; copy the executable command on the following line.

For a noninteractive upgrade, stop the service and run the launcher against
the packaged binary from the package root:

```sh
degoo-rc service stop
./degoo-rc install --binary ./bin/rclone --upgrade --no-wizard
degoo-rc service start
```

For a manual mount, use `degoo-rc stop` to unmount first.

Upgrade preserves the vault secret, account tokens, email, remote and mount
settings, and cache. It refuses a change to the vault folder ID. `degoo-rc login`
remembers the account email but never saves the account password; token renewal
is automatic when supported, though long-term unattended renewal remains
unverified.

Run `degoo-rc info` to inspect mount, service, cache, and
pending-upload status. Keep the cache until pending uploads reach zero. A
locally saved file may still await remote upload; pending changes and their
matching vault configuration are both needed to resume work after recovery.

## Uninstall

Inspect the install without changing it:

```sh
degoo-rc uninstall --inventory
```

In a terminal, running `degoo-rc uninstall` with no arguments or with `--wizard`
starts guided cleanup. It offers to retain the vault/account configuration,
cache, and logs in a private backup directory; each choice defaults to No.

A private backup is an owner-only folder outside the install, created only
when you choose to keep something. At its path prompt, press Enter for the
suggested path or type an absolute path; `N` is not a path. To remove all local
state without a backup, choose No for configuration, cache, and logs, then
confirm `DISCARD` and `REMOVE`. Start cleanup from outside the install
(`cd ~` first). Deleting the encryption settings means a fresh setup should
create a new dedicated vault folder with a new name.
Leave the existing folder ID and vault-config import prompts blank.

If uploads are pending, cleanup refuses to discard the cache. To recover those
changes, retain both the cache and its matching configuration. Restore the
retained backup into a fresh installation with
`./degoo-rc install --wizard --restore-directory BACKUP_DIRECTORY`.
The wizard copies the saved cache and keeps the original backup untouched.
The separate `backup-config` command excludes cached files and pending uploads.

If you choose not to retain the vault configuration, type `DISCARD` to confirm
losing the local secret. Type `REMOVE` to confirm local cleanup. Uninstall
stops the service linked to this install and removes its owned local files and
systemd links, then reloads the user manager and verifies removal. It never
deletes remote encrypted files or Degoo account data.

See [installation and cleanup](docs/INSTALLATION.md) for full setup,
permissions, service, cache, upgrade, and recovery details. See
[usage](docs/USAGE.md) for configuration, commands, and current validation
limits.

## Archive contents

```text
rclone-degoo/
  README.md
  degoo-rc                 unified Degoo entrypoint
  bin/rclone
  scripts/*.sh             internal shell helpers
  scripts/python/*.py
  docs/*.md
  docs/BUILD.json
  licenses/COPYING
  licenses/degoo-NOTICE
```

The archive root has one Degoo command: `degoo-rc`. It dispatches to shell
helpers in `scripts/` and Python implementations in `scripts/python/`, including
the internal `scripts/trash.sh` helper. Fresh installs also have `degoo-rc` as
their only root-level Degoo command. In-place upgrades may retain root-level
`.sh` shims already present in an older install. New examples use `degoo-rc`.
The command list is available with `degoo-rc help`, and `degoo-rc COMMAND help`
shows that command's options.
`service` accepts `enable`, `disable`, `start`, `stop`, `restart`, and `status`;
existing command options remain available. `trash` lists account trash, while
`empty-trash` is the separate deletion command. The six Markdown files in
`docs/` are copied from `docs/degoo/` and flattened to that directory.
`docs/BUILD.json` records package provenance and the
included binary's SHA-256. The two license files are the repository's `COPYING`
and the Degoo backend notice. The `.tar.gz.sha256` checksum sidecar is next to
the archive, not inside it.

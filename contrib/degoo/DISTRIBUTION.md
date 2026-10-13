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
when it is disabled by the kernel or system policy. Before each dependent
action, the wizard checks the selected path and distinguishes missing tools
from installed but unusable components. It prints detected-system setup,
verification, and retry instructions for FUSE, `util-linux`, `/dev/fuse`, and
the immutable-mount helper when relevant. For an immutable `/mnt/degoo`, the
wizard stops before service startup and prints one `mount-setup` administrator
command. It preserves the flag; the command builds as the invoking user and
installs only the fixed helper. If GCC is missing, the wizard prints the
detected package command. Known blockers prevent service enablement and ready
status, and completed configuration and pending cache data are preserved for
retry. After helper setup, continue as the normal user. With systemd, enable
the service and then check `degoo-rc info`. Without systemd, run the foreground
mount and check `degoo-rc info` from another terminal; append `--foreground`
to `mount-setup` so it prints the correct continuation. See
[installation prerequisite recovery](docs/INSTALLATION.md).

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

This backs up configuration only: the vault key, remembered email, and mount
settings. It excludes account tokens and the encrypted account password in
`account_credentials`. File data, cache, and pending uploads are excluded. Keep
the backup private because it contains the vault encryption key. On the new
computer, the installer offers a backup directory as its first prompt, or use
this command:

```sh
./degoo-rc install --wizard --restore-directory ~/private-backups/rclone-degoo
```

Recovered settings become defaults. The backup does not contain account
authentication, so log in again after restoration. Older backups may include
account tokens or encrypted credentials; restore ignores them and leaves the
source backup untouched. Restoration requires a fresh local install.

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
degoo-rc login
degoo-rc service enable
```

This first login happens before vault setup and saves tokens only. After
`setup` creates `vault.json`, run `degoo-rc login` again to save encrypted
account credentials using the same vault password and optional salt as vault
data. These credentials allow password login after refresh is rejected.

The interactive wizard handles this final login as part of setup. It installs
an early imported vault before login; for a new vault or a file selected later,
it asks you to sign in again after setup and confirms that the encrypted
account credential is saved before offering mount startup.

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
command is installed on `PATH`; the default mount path is `/mnt/degoo`. Open
that directory in your file manager or use it like any other directory while
it is mounted. If the installer cannot prepare the owner-private directory, it
prints the needed privileged `mkdir`, `chown`, `chmod 700`, and `chattr +i`
commands. The mount refuses to hide local files and protects the underlying
directory from unmounted writes. An older install keeps its saved mountpoint
unless `--mountpoint /mnt/degoo` is selected explicitly. That migration is
refused while the old underlying directory contains files. Uninstall leaves
the external mount directory in place. The privileged helper does not support
an immutable legacy `mount/` target. For an install whose saved mountpoint is
immutable, run `degoo-rc install --mountpoint /mnt/degoo` to migrate the saved
path while preserving the cache. The old underlying `mount/` directory must
be empty. Migration does not clear an immutable flag; the root helper supports
only `/mnt/degoo`. An immutable legacy directory remains protected and may
need a separate manual unlock before uninstall cleanup.

You may leave `chattr +i` set on `/mnt/degoo` while mounted and unmounted. The
flag blocks the write-access check required by standard `fusermount3`, even
when the directory is owner-writable. Mounting with the flag retained requires
the administrator-installed helper in
[immutable-mount/README.md](immutable-mount/README.md); the wizard never
clears the flag or installs privileged files automatically. The helper is
limited to an empty `/mnt/degoo` owned by the invoking user and not writable
by group or other users; the default directory remains owner-private. It
mounts with that user's ID, rejects `allow_other` and `allow_root`, and leaves
rclone running as the ordinary user. The mount launcher checks the helper's
root ownership, protected permissions, and setuid bit, then selects it only
for an immutable
mount. Ordinary mutable mounts continue using the system FUSE helper. When the
immutable flag is absent, the mount guard uses the retained directory handle
to protect the underlying directory after mounting and on failure or unmount.
When the wizard reports a missing or invalid helper, run the administrator
command it prints. With the default install root, use:

```sh
sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup
```

After it completes, continue as the normal user, without `sudo`. With systemd,
enable the service and check the result:

```sh
degoo-rc service enable
degoo-rc info
```

If the systemd user manager is unavailable, run the administrator setup
command with `--foreground`:

```sh
sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup --foreground
```

Then start the mount as the normal user in a foreground terminal:

```sh
degoo-rc mount
```

Run `degoo-rc info` from another terminal. If the service was already enabled,
run `degoo-rc service restart` as the normal user.
`mount-setup` preserves the immutable flag and never mounts or starts the
service as root.

```sh
degoo-rc info
degoo-rc logs
degoo-rc logs --follow
```

`degoo-rc logs` shows recent mount, login, setup, status, and Degoo diagnostic
messages. The private `status.log` and `diagnostics.log` contain status and
runtime diagnostics. Use
`--follow` to watch new messages; press Ctrl+C to stop following. The `logs/`
path is a directory, while `degoo-rc logs` is the viewer. New mounts log
notices and errors by default; routine cache-cleanup messages require a
foreground mount with `degoo-rc mount --log-level INFO`. If a refresh token is
rejected and encrypted account credentials are saved, the client attempts
password login; network and temporary server errors do not trigger it. If
authentication still requires attention, `degoo-rc info` shows a login-required
warning with the login command. Run `degoo-rc login` again when needed; the
mount can reload newly saved tokens. An expired access token alone does not
establish a failure when automatic renewal is still possible. Earlier renewal
attempts returned HTTP 403 and HTTP 401; long-term unattended renewal remains
unverified.

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

Upgrade preserves the vault secret, encrypted account credentials, account
tokens, remembered email, remote and mount settings, and cache. It refuses a
change to the vault folder ID. `degoo-rc login` remembers the account email and,
when a vault configuration exists, saves an encrypted account password using
the same vault password and optional salt as vault data. Before vault setup,
login saves tokens only. A rejected refresh can trigger password login; network
and temporary server errors do not. Long-term unattended renewal remains
unverified.

### Update the installed client

The Linux amd64 `degoo-rc update` command downloads the latest successful
`degoo-package.yml` artifact on the fork's `staging` branch. It uses the
authenticated GitHub CLI (`gh`); install it and run `gh auth login` if needed.
To use a local package instead, pass the `.tar.gz` path with its matching
`.tar.gz.sha256` sidecar beside it:

```sh
degoo-rc update
degoo-rc update --archive ~/Downloads/rclone-degoo-linux-amd64.tar.gz
degoo-rc update --delete-cache
```

The local archive option does not use GitHub or `gh`. A systemd user manager
is needed only to inspect or control a service owned by this installation;
manual mounts and installs with no owned service links do not require systemd.
Before stopping an owned mount, the updater verifies the archive checksum and
binary hash in `docs/BUILD.json`. For an Actions download, it also matches the
source commit against the workflow run. It compares the installed binary and
helper payload. An identical payload is a no-op, even with `--delete-cache`,
and leaves the service, configuration, install manifest, and cache untouched.
Documentation
changes alone do not change this payload hash.

The private `install.json` manifest records `build.version: 1`,
`build.binary_sha256`, `build.payload_sha256`, and verified `build.commit` when
available. The payload covers the executable, `degoo-rc`, managed shell and
Python helpers, and immutable-mount files, not documentation. An update also
records `build.package_sha256` and `build.source`; Actions packages include
`build.run_id`. This identifies the installed program separately from vault
configuration and cache.

The updater stops only an owned service or uniquely identified manual mount,
waits until the kernel reports it unmounted, then holds the cache lock while
installing. It keeps service enablement unchanged, restarts only a service that
was active, and confirms that the new mount is connected. A recognized manual
mount resumes in the background with its original arguments and logs to the
private `logs/update-mount.log`; an inactive install stays inactive. Busy or
unidentified mounts and conflicts in the owned service's recorded/matching
links or loaded unit stop the update before program replacement. Pending
uploads or unverifiable cache state block `--delete-cache`; an ordinary update
preserves pending and unknown cache data.

The cache stays in place by default. `--delete-cache` applies only when a new
payload is installed. The updater retains the held cache-lock inode and
requires the known cache identity, paired VFS data and metadata, and metadata
proving every entry clean with a fingerprint. Pending, unknown, unpaired, or
unexpected cache data blocks deletion.

If installation fails, the previous program files are restored from
`~/.local/share/rclone-degoo/tmp/update-backup`. If restart fails, the command
returns nonzero and leaves the new program and configuration available for
recovery. By default the cache is also preserved. Check `degoo-rc info` and
`degoo-rc logs`, then retry with `degoo-rc service start` or `degoo-rc mount`.
An abrupt process or system interruption can leave the backup behind; a later
update refuses to overwrite it. Keep the backup intact and resolve it manually
before retrying, since crash rollback is not automatic.

An older installation without the `update` command needs one ordinary archive
upgrade first. Stop an active service and run the new archive's installer:

```sh
degoo-rc service stop
./degoo-rc install --binary ./bin/rclone --upgrade --no-wizard
degoo-rc service start
```

For a manual mount, use `degoo-rc stop` before upgrading and `degoo-rc mount`
afterward. Future versions may use GitHub releases as the remote package
source; the current remote source is the workflow artifact.

Run `degoo-rc info` to inspect mount, service, cache, and
pending-upload status. Keep the cache until pending uploads reach zero. A
locally saved file may still await remote upload; pending changes and their
matching vault configuration are both needed to resume work after recovery.

`degoo-rc status` reports authentication errors first and returns nonzero when
health checks fail. It accepts an optional file or directory path and supports
`--json`. Per-file states are `pending`, `uploading`, `uploaded`, and `unknown`.
The read-only `user.rclone.upload-status` attribute is enabled for Degoo mounts;
generic VFS mounts default it off. `uploaded` means VFS knows the current
version has a remote representation after writeback, not that status performed
a new remote integrity check. Files created under an unmounted mountpoint are
reported as local-only and are never queued for upload. The private status
socket returns aggregate queued, uploading, and retrying counts only.

## Uninstall

Inspect the install without changing it:

```sh
degoo-rc uninstall --inventory
```

In a terminal, running `degoo-rc uninstall` with no arguments or with `--wizard`
starts guided cleanup. It offers to retain the vault configuration, remembered
email, cache, and logs in a private backup directory; each choice defaults to
No. The retained configuration excludes account tokens and the encrypted
account password, so a fresh login is required after restoration.

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
Older retained backups may contain account tokens or encrypted credentials;
restore ignores them and always requires a fresh login.
The separate `backup-config` command excludes cached files and pending uploads.

If you choose not to retain the vault configuration, type `DISCARD` to confirm
losing the local secret. Type `REMOVE` to confirm local cleanup. Uninstall
stops the service linked to this install and removes its owned local files and
systemd links, then reloads the user manager and verifies removal. It preserves
the external `/mnt/degoo` directory. It never
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

The archive root has one Degoo command: `degoo-rc`, including the `update`
command for unattended Linux amd64 package updates. It dispatches to shell
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

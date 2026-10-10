# Contained Degoo installation

See [USAGE.md](USAGE.md) for the quick start. The installer creates an
isolated Linux client under `~/.local/share/rclone-degoo`; it does not change a
normal rclone installation. Upstream rclone does not include this fork's Degoo
vault commands.

## Requirements

To mount the vault, the system needs:

- Linux and Python 3.10 or newer.
- FUSE 3, or a compatible `fusermount` helper, plus a readable and writable
  `/dev/fuse` device.
- The `mountpoint` command from `util-linux`.
- This fork's rclone binary, either built from this repository or supplied in
  the prebuilt Linux archive.

Go 1.26 or newer is needed only when building from source. It is not needed to
install or run the prebuilt archive. A working systemd user manager is optional
and is needed only for the background user service. Without it, use the
foreground mount command.

The shell launcher offers to install Python 3 if the required version is not
available. The wizard checks mount dependencies and can offer to use a detected
`apt-get`, `dnf`, `pacman`, or `zypper` package manager for missing `fuse3` or
`util-linux`. Installing system packages may require `sudo` or root access. If
you decline or the system manager is unavailable, vault configuration can
still finish, but mounting requires installing the missing dependencies first.
The installer cannot create `/dev/fuse` when the kernel or system policy has
not made it available; ask the system administrator to enable FUSE in that
case. The wizard will not enable the service while mount dependencies are
unavailable.

## Start the installer

From a repository checkout, build the binary and start the wizard:

```sh
mkdir -p build
go build -o build/rclone .
./contrib/degoo/degoo-rc install
```

Go 1.26 or newer is required only for this source-build path. In a terminal,
the source command starts the wizard. To select a binary explicitly:

```sh
./contrib/degoo/degoo-rc install --wizard --binary /path/to/rclone
```

The matching prebuilt Linux amd64 archive does not require Go. Put
`rclone-degoo-linux-amd64.tar.gz` in `~/Downloads`, extract it into a new
temporary directory to avoid mixing it with files from an older extraction,
and run the archive's top-level launcher:

```sh
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf "$HOME/Downloads/rclone-degoo-linux-amd64.tar.gz" -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./degoo-rc install
```

The archive can be shared with other Linux amd64 users. It contains the client,
`degoo-rc`, helper scripts, license, and documentation, but no account
credentials or vault secret. No GitHub release archive has been uploaded yet.
With no arguments in a terminal, `degoo-rc install` starts the wizard. It checks
dependencies, selects or confirms a Degoo folder, creates or imports a vault,
and offers to enable the user service when a systemd user manager is available.
The default install root is `~/.local/share/rclone-degoo`.

For automation or a staged install, the existing installer options remain
available. `--no-wizard` skips the wizard so a client can be staged before
login and vault setup:

```sh
./contrib/degoo/degoo-rc install --binary ./build/rclone --no-wizard
```

After a staged install, log in and finish setup with the installed command:

```sh
degoo-rc login
degoo-rc folders --parent-id 0
degoo-rc setup --parent-id DEVICE_ID --name rclone-degoo-vault
```

## Choose a dedicated vault folder

The wizard lists folders under account root ID `0` to discover the account's
device folders. A live create request under ID `0` is rejected, so the wizard
suggests the first discovered category-1 device folder as the parent, or the
first selectable folder if no category-1 entry is returned. Choose a listed
device/folder or enter its ID. The new dedicated child folder defaults to
`rclone-degoo-vault`. Account root ID `0` is never accepted as the vault folder
or as the create parent.

You may select an existing dedicated folder instead of creating a new one.
Existing native Degoo files are not encrypted-vault files and cannot be read
as such. Setup creates the provider-specific marker for a new vault and will
not repurpose an existing folder with the same name.

The wizard generates a cryptographically random vault encryption password for
a new vault. Keep a private backup of `config/vault.json` outside the install
before saving important data. Degoo account credentials and the vault password
serve separate purposes; account passwords are never stored.

To use an existing compatible encrypted vault on another machine, import its
private vault configuration and select the same dedicated folder. The password,
optional salt, chunk size, and object cap are preserved; local remote and cache
paths are adapted. The password alone is not enough to locate or decrypt a
vault: keep its folder ID and format settings together. Only one machine should
write to a vault at a time.

Provide the existing folder and configuration when starting the wizard:

```sh
./contrib/degoo/degoo-rc install --wizard --binary ./build/rclone \
  --folder-id EXISTING_VAULT_ID --vault-config ~/private-backups/vault.json
```

## Defaults

The installer uses `~/.local/share/rclone-degoo` as the private root and sets
its directories to owner-only access (`0700`) and private config files to
`0600`. The wizard offers to enable the systemd user service by default when a
user manager responds. It does not enable lingering automatically.

For a new vault, the default chunk size is 1 GiB of plaintext and the
provisional maximum encrypted object size is 19 GiB. The provider's live limit
above 20 GiB has not been established. The initial `config/mount.json` values
are:

| Setting | Default | Meaning |
| --- | --- | --- |
| `cache_max_size` | `20G` | Soft target for the amount of cached data. Open and dirty files are not evicted to meet it. |
| `cache_min_free_space` | `20G` | Keep this much free space on the filesystem holding the cache when evicting clean data. |
| `cache_max_age` | `10m` | Maximum age for clean cached data before it becomes eligible for eviction. |
| `cache_poll_interval` | `5s` | Interval for checking remote changes while the vault is mounted. |
| `verify_upload` | `true` | Download each encrypted upload and compare its size and SHA-256 before publication. |

Change these values in `config/mount.json` and restart the mount or service to
apply them. Cache limits are targets: open files and pending writes remain in
the cache. A newly written logical file is staged locally in full even when
remote storage splits it into chunks.

## Files and permissions

```text
~/.local/share/rclone-degoo/
  bin/rclone          dedicated executable
  config/auth.json    access and refresh tokens
  config/login.json   remembered account email, no account password
  config/vault.json   vault secret and format settings
  config/rclone.conf  isolated remote and folder ID
  config/mount.json   cache and upload settings
  cache/              persistent plaintext VFS cache and lock
  mount/              plaintext view while mounted
  tools/              contained FUSE helper alias
  tmp/                temporary upload files
  logs/               private runtime logs
  degoo-rc            unified command entrypoint
  scripts/*.sh        internal shell helpers
  scripts/python/     Python command implementations
  install.json        local identity and owned service links
  rclone-degoo.service user service definition
```

The installer also creates `~/.local/bin/degoo-rc` as a link to the contained
entrypoint. It does not edit the normal rclone config or shell profile files.
If `~/.local/bin` is not on `PATH`, it prints an export command for the current
shell or for you to add yourself. Uninstall removes the link only when it
still points to this installation. A fresh install has `degoo-rc` as its only
root-level command; upgrades retain existing root-level `.sh` shims. Source
and archive roots may include the legacy install, uninstall, and trash shims.
Runtime Python bytecode and temporary upload files stay contained or are
disabled.

## Login, mount, and recovery

The wizard finishes with a summary of the install directory, mount location,
current mount state, helper commands, and log paths. A service start request
does not establish that the mount is ready: the wizard waits briefly for the
kernel's mount table to show the mount. If startup is still incomplete, it
reports that state and gives commands to inspect status and logs. The default
mount path is
`~/.local/share/rclone-degoo/mount`. Open that directory in a file manager or
use it like any other directory while mounted. The installed command dispatches
to helpers in `~/.local/share/rclone-degoo/scripts/`, rather than using files
from the extracted package folder.

Run `degoo-rc login` to authenticate. It remembers the email in
`config/login.json` with owner-only permissions, but never saves the account
password. Use `degoo-rc login --clear-email` to forget the email without
removing existing tokens or vault settings.

The password prompt is shown in the terminal even when command logs are
written to a file. Password characters are not echoed or saved.

Access tokens refresh automatically when nearing expiry. If Degoo rejects a
refresh, the mount reports that login is needed. Run `degoo-rc login` again;
the authentication client can load the newly saved credentials. Long-term
unattended renewal has not been verified: a live one-hour access token expired
and renewal returned HTTP 403, though fresh login and renewal then succeeded.

Use the mount launcher for a foreground session:

```sh
degoo-rc mount
```

Or use the user service:

```sh
degoo-rc service enable
degoo-rc service status
degoo-rc service restart
```

The service starts at login. To keep it running after logout and start it at
boot, check and enable account lingering if needed:

```sh
loginctl show-user "$USER" -p Linger
loginctl enable-linger "$USER"
```

The latter may require administrator authorization. No root mount service is
installed. The service restarts after process failures, but an authentication
failure during ordinary operations can leave an active mount unable to read
Degoo until login succeeds. Keep only one mount process using this cache.

The upgrade wizard offers to stop the contained service or mount before
collecting settings. Stopping preserves cached pending writes. If you cancel
after stopping, run `degoo-rc service start` to restore
the service, or `degoo-rc mount` for a manual session. If a stopped service
leaves a disconnected FUSE mount, the wizard can offer to detach it after
confirming the cache has no active writer. It keeps all cached data. A busy
connected mount must be closed and unmounted normally. For a noninteractive
upgrade,
stop the service first and upgrade from the repository:

```sh
degoo-rc service stop
./contrib/degoo/degoo-rc install --binary /path/to/rclone --upgrade --no-wizard
degoo-rc service start
```

For a manual foreground mount, use `degoo-rc stop` to unmount.

Upgrade preserves the vault secret, account tokens, email, remote settings,
mount settings, and cache; it refuses a change to the folder ID.

## Inspect local status

`degoo-rc info` reports the mount and service state,
configuration locations and roles, cache settings, pending uploads, token
expiry, and local disk use. The default report is local-only and never prints
passwords, salts, email addresses, account tokens, or signed URLs. Its bounded
size scan excludes the cloud mount and does not follow symlinks.

Use `degoo-rc info --check-remote` for an optional bounded metadata lookup of the
vault and ancestor folders. It may refresh account authentication; it does not
download file contents or enumerate the cloud to calculate its size. Zero
pending uploads means there are no local dirty writes, not that remote data
passed an independent integrity audit.

## Logs and authentication errors

Show recent mount, login, setup, and status log messages, or watch new messages:

```sh
degoo-rc logs
degoo-rc logs --follow
```

Press Ctrl+C to leave follow mode. `degoo-rc logs` is the viewer;
`~/.local/share/rclone-degoo/logs/` is a directory of files. New mounts log
notices and errors by default, suppressing routine cache-cleanup messages.
Existing log contents remain available. For a more detailed foreground session:

```sh
degoo-rc mount --log-level INFO
```

Stop the service first before starting a foreground mount. The command hides
recognized credential fields and signed URLs from displayed lines. For service startup
and process exit messages, also inspect the user journal:

```sh
journalctl --user -u rclone-degoo.service -n 100 --no-pager
```

Upgrading an older wizard installation preserves its known log files and sets
their permissions to owner-only. New login and setup logs are private when
created, including commands run directly by the wizard.

`degoo-rc info` displays an explicit login-required warning when recent mount logs
report rejected authentication renewal, or when the saved credentials cannot
provide a usable token. An expired access token with a saved refresh token may
renew automatically on the next cloud request; expiry alone is not reported
as a rejected renewal. A successful new login clears older authentication
failure warnings when the saved credentials are updated.

When login is required, run:

```sh
degoo-rc login
degoo-rc info --check-remote
```

The mount can reload the new saved credentials. An active service can still
have failed cloud requests; service state alone does not establish a healthy
cloud connection. Keep the cache and vault configuration to recover pending
changes. Login and remote checks do not establish that pending uploads have
finished; use `degoo-rc status` to inspect the queue.

## Uninstall

The explicit inventory command inspects the installation without changing it:

```sh
degoo-rc uninstall --inventory
```

In a terminal, running `degoo-rc uninstall` with no arguments starts the cleanup
wizard; use `--wizard` to request it explicitly. The wizard offers to retain the
vault/account configuration, cache, and logs in a private backup directory
outside the install. Each retention choice defaults to No. It asks for
confirmation before discarding a vault secret and before removing the local
install. If the cache has pending uploads, cleanup refuses to discard them;
keep both the cache and its matching configuration in the backup or resume
cleanup after uploads finish. A default backup directory is suggested and
selected state is copied there with private permissions. This is a new external
folder created only when you choose to keep something: the directory is
owner-only (`0700`) and secret files are owner-only (`0600`). At the backup
path prompt, press Enter to accept the default or enter an absolute path.
It is not a yes/no question; entering `N` does not turn retention off.
Choose No for every retention option to create no backup. The backup includes
the binary and install identity when pending uploads are retained, so local
changes can be recovered. If you choose not to retain a vault config, type
`DISCARD` to confirm losing the local secret. Type `REMOVE` to confirm local
cleanup.

To resume uploads, restore the retained backup contents into
`~/.local/share/rclone-degoo`, then run this from a source checkout:

```sh
./contrib/degoo/degoo-rc install \
  --binary ~/.local/share/rclone-degoo/bin/rclone --upgrade --no-wizard
```

The cleanup stops the owned service before disabling it, removes its systemd
links, reloads the user manager, and removes the contained configuration,
cache, logs, binary, and scripts. Only explicitly retained state remains in
the chosen external backup directory. Remote encrypted files and Degoo
account data are untouched. Shared dependencies, parent systemd directories,
and historical entries in the shared system journal are not installation
files and remain. Cleanup does not disable account-wide lingering. If you
enabled lingering solely for
this service, you can disable it separately with
`loginctl disable-linger "$USER"` after considering your other user services.

### Verify removal and reinstall from the archive

Run cleanup from outside the installation directory (`cd ~` first). After
uninstalling, check the local install paths and systemd state:

```sh
for path in \
  "$HOME/.local/share/rclone-degoo" \
  "$HOME/.local/bin/degoo-rc" \
  "$HOME/.config/systemd/user/rclone-degoo.service" \
  "$HOME/.config/systemd/user/default.target.wants/rclone-degoo.service"
do
  if [ -e "$path" ] || [ -L "$path" ]; then
    printf 'REMAINS: %s\n' "$path"
  else
    printf 'Removed: %s\n' "$path"
  fi
done
systemctl --user show rclone-degoo.service \
  -p LoadState -p ActiveState -p FragmentPath
```

For the default installation, the install root and PATH link should be
removed. The unit should report `LoadState=not-found`, `ActiveState=inactive`,
and an empty `FragmentPath`. A custom `XDG_CONFIG_HOME` uses that directory
instead of `~/.config` for service links. The pre-cleanup inventory reports
the link paths. If you selected no retention, no backup is created.

Verify and extract the distributable into a new folder, then run its wizard:

```sh
cd "$HOME/Downloads"
sha256sum -c rclone-degoo-linux-amd64.tar.gz.sha256
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf rclone-degoo-linux-amd64.tar.gz -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./degoo-rc install
```

If you discarded the old vault encryption settings, create a new dedicated
vault folder with a new name. Reusing the old encrypted folder requires its
original encryption settings. Leave the existing folder ID and configuration
import prompts blank to create a new vault.

The noninteractive cleanup options remain available through
`degoo-rc uninstall`; use `degoo-rc uninstall help` for the full parameter
list. Options include `--keep-config`, `--keep-cache`, and `--keep-logs` with `--delete-local`
to select data for retention, or an exact private `--vault-backup` and the
explicit `--discard-vault-secret` choice when deleting the only local copy.
Uninstall refuses active mounts, cache locks, unsafe paths, and pending uploads
unless both cache and matching configuration are retained in a private backup.

## Command reference

Run `degoo-rc help` to list commands and `degoo-rc COMMAND help` for detailed
parameters and examples. Available commands are `install`, `uninstall`,
`login`, `setup`, `mount`, `stop`, `status`, `location`, `folders`, `service`,
`info`, `logs`, `trash`, and `empty-trash`. The `service` actions are `enable`,
`disable`, `start`, `stop`, `restart`, and `status`. Current installer and
helper options remain supported; the command-specific help is authoritative.
`trash` lists account trash. `empty-trash` is a separate account-wide deletion
command with its own confirmation behavior.

Upgrades retain existing root-level `.sh` shims, and source or archive roots
may include the legacy install, uninstall, and trash shims. Fresh installs use
`degoo-rc` as their only root-level command. New commands and examples should
use `degoo-rc`.

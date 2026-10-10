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
./contrib/degoo/install.sh --wizard --binary ./build/rclone
```

Go 1.26 or newer is required only for this source-build path. In a terminal,
running the source launcher with no options also starts the wizard. To pass a
different built binary explicitly:

```sh
./contrib/degoo/install.sh --wizard --binary /path/to/rclone
```

The matching prebuilt Linux amd64 archive does not require Go. Put
`rclone-degoo-linux-amd64.tar.gz` in `~/Downloads`, extract it into a new
temporary directory to avoid mixing it with files from an older extraction,
and run the archive's top-level launcher:

```sh
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf "$HOME/Downloads/rclone-degoo-linux-amd64.tar.gz" -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./install.sh
```

The archive can be shared with other Linux amd64 users. It contains the client,
installer, helper scripts, license, and documentation, but no account
credentials or vault secret. No GitHub release archive has been uploaded yet.
With no arguments in a terminal, `install.sh` starts the wizard. It checks the
dependencies, selects or confirms a Degoo folder, creates or imports a vault,
and offers to enable the user service when a systemd user manager is available.
The default install root is `~/.local/share/rclone-degoo`.

For automation or a staged install, the existing `--binary` command-line path
is still available. `--no-wizard` skips the wizard so a client can be staged
before login and vault setup:

```sh
./contrib/degoo/install.sh --binary ./build/rclone --no-wizard
```

After a staged install, log in and finish setup with the contained helpers:

```sh
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/folders.sh --parent-id 0
~/.local/share/rclone-degoo/setup.sh --parent-id DEVICE_ID --name rclone-degoo-vault
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
./contrib/degoo/install.sh --wizard --binary ./build/rclone \
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
  scripts/python/     contained installer and command helpers
  install.json        local identity and owned service links
  rclone-degoo.service user service definition
  *.sh                convenience launchers for login, mount, status, etc.
```

The installer does not alter the normal rclone config or shell profiles.
Runtime Python bytecode and temporary upload files stay contained or are
disabled.

## Login, mount, and recovery

The wizard finishes with a summary of the install directory, mount location,
current mount state, helper commands, and log paths. A service start request
does not establish that the mount is ready: the summary checks the kernel's
mount table. The default mount path is
`~/.local/share/rclone-degoo/mount`. Open that directory in a file manager or
use it like any other directory while mounted. Installed helper scripts are
in `~/.local/share/rclone-degoo/`, rather than the extracted package folder.

Run `~/.local/share/rclone-degoo/login.sh` to authenticate. The helper remembers
the email in `config/login.json` with owner-only permissions, but never saves
the account password. Use `login.sh --clear-email` to forget the email without
removing existing tokens or vault settings.

The password prompt is shown in the terminal even when command logs are
written to a file. Password characters are not echoed or saved.

Access tokens refresh automatically when nearing expiry. If Degoo rejects a
refresh, the mount reports that login is needed. Run the login helper again;
the authentication client can load the newly saved credentials. Long-term
unattended renewal has not been verified: a live one-hour access token expired
and renewal returned HTTP 403, though fresh login and renewal then succeeded.

Use the mount launcher for a foreground session:

```sh
~/.local/share/rclone-degoo/mount.sh
```

Or use the user service:

```sh
~/.local/share/rclone-degoo/service.sh enable
~/.local/share/rclone-degoo/service.sh status
~/.local/share/rclone-degoo/service.sh restart
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

Before replacing the binary, stop the service or manual mount and upgrade from
the repository:

```sh
./contrib/degoo/install.sh --binary /path/to/rclone --upgrade --no-wizard
```

Upgrade preserves the vault secret, account tokens, email, remote settings,
mount settings, and cache; it refuses a change to the folder ID.

## Inspect local status

`~/.local/share/rclone-degoo/info.sh` reports the mount and service state,
configuration locations and roles, cache settings, pending uploads, token
expiry, and local disk use. The default report is local-only and never prints
passwords, salts, email addresses, account tokens, or signed URLs. Its bounded
size scan excludes the cloud mount and does not follow symlinks.

Use `info.sh --check-remote` for an optional bounded metadata lookup of the
vault and ancestor folders. It may refresh account authentication; it does not
download file contents or enumerate the cloud to calculate its size. Zero
pending uploads means there are no local dirty writes, not that remote data
passed an independent integrity audit.

## Logs and authentication errors

Show recent mount, login, setup, and status log messages, or watch new messages:

```sh
~/.local/share/rclone-degoo/logs.sh
~/.local/share/rclone-degoo/logs.sh --follow
```

Logs live in `~/.local/share/rclone-degoo/logs/`. The helper hides recognized
credential fields and signed URLs from displayed lines. For service startup
and process exit messages, also inspect the user journal:

```sh
journalctl --user -u rclone-degoo.service -n 100 --no-pager
```

Upgrading an older wizard installation preserves its known log files and sets
their permissions to owner-only. New login and setup logs are private when
created, including commands run directly by the wizard.

`info.sh` displays an explicit login-required warning when recent mount logs
report rejected authentication renewal, or when the saved credentials cannot
provide a usable token. An expired access token with a saved refresh token may
renew automatically on the next cloud request; expiry alone is not reported
as a rejected renewal. A successful new login clears older authentication
failure warnings when the saved credentials are updated.

When login is required, run:

```sh
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/info.sh --check-remote
```

The mount can reload the new saved credentials. An active service can still
have failed cloud requests; service state alone does not establish a healthy
cloud connection. Keep the cache and vault configuration to recover pending
changes. Login and remote checks do not establish that pending uploads have
finished; use `status.sh` to inspect the queue.

## Uninstall

The explicit inventory command inspects the installation without changing it:

```sh
~/.local/share/rclone-degoo/uninstall.sh --inventory
```

In a terminal, running `uninstall.sh` with no arguments starts the cleanup
wizard; use `--wizard` to request it explicitly. The wizard offers to retain the
vault/account configuration, cache, and logs in a private backup directory
outside the install. Each retention choice defaults to No. It asks for
confirmation before discarding a vault secret and before removing the local
install. If the cache has pending uploads, cleanup refuses to discard them;
keep both the cache and its matching configuration in the backup or resume
cleanup after uploads finish. A default backup directory is suggested and
selected state is copied there with private permissions. The backup includes
the binary and install identity when pending uploads are retained, so local
changes can be recovered. If you choose not to retain a vault config, type
`DISCARD` to confirm losing the local secret. Type `REMOVE` to confirm local
cleanup.

To resume uploads, restore the retained backup contents into
`~/.local/share/rclone-degoo`, then run this from a source checkout:

```sh
./contrib/degoo/install.sh \
  --binary ~/.local/share/rclone-degoo/bin/rclone --upgrade --no-wizard
```

The cleanup is local only: it removes this installation and its owned systemd
links, but leaves remote encrypted files and Degoo account data untouched. It
does not disable account-wide lingering. If you enabled lingering solely for
this service, you can disable it separately with
`loginctl disable-linger "$USER"` after considering your other user services.

The noninteractive cleanup command remains available through the Python helper
at `contrib/degoo/scripts/python/uninstall.py`. Use `--keep-config`,
`--keep-cache`, and `--keep-logs` with `--delete-local` to select data for
retention. It also supports an exact private `--vault-backup` or the explicit
`--discard-vault-secret` choice when deleting the only local copy. It refuses
active mounts, cache locks, unsafe paths, and pending uploads unless both cache
and matching configuration are retained in a private backup.

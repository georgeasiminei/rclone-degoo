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
case. The wizard checks the prerequisites for the selected mount and service
path before the affected action. It distinguishes a missing command or device
from an installed but inaccessible or misconfigured one, and identifies
optional components such as the systemd user manager. It will not enable or
start a service when a known required mount prerequisite is unavailable.

### Repair missing or unusable mount prerequisites

The wizard prints commands for the detected distribution and selected mount
path, explains when they need administrator privileges, and gives commands to
verify the repair and retry setup. The following package commands are manual
examples if you need to install the generic FUSE and mountpoint tools yourself:

```sh
# Debian or Ubuntu
sudo apt-get install fuse3 util-linux

# Fedora or RHEL-compatible systems
sudo dnf install fuse3 util-linux

# Arch Linux
sudo pacman -S --needed fuse3 util-linux

# openSUSE
sudo zypper install fuse3 util-linux
```

Run the checks as the account that will mount the vault:

```sh
command -v fusermount3 || command -v fusermount
command -v mountpoint
test -c /dev/fuse && test -r /dev/fuse && test -w /dev/fuse
```

If `/dev/fuse` is absent but the running kernel provides the FUSE module, an
administrator can try `sudo modprobe fuse` and repeat the checks. If it remains
absent, or exists but this account cannot read and write it, the kernel,
container, or system device policy must be fixed. Inspect it with `ls -l
/dev/fuse` and `id`; use the wizard's system-specific access instructions or
ask the administrator. Do not make the device world-writable as a workaround.

An immutable `/mnt/degoo` needs the manually installed helper described in
[immutable-mount/README.md](../../contrib/degoo/immutable-mount/README.md).
When the helper is missing or invalid, the wizard stops before the service
prompt and prints one administrator command from the private install root:

```sh
sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup
```

When the systemd user manager is unavailable, run the foreground setup
command:

```sh
sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup --foreground
```

`mount-setup` builds as the account that invoked `sudo`, then installs the
fixed root-owned helper. It does not clear the immutable flag, mount the vault,
or start a service as root. A valid installed helper does not require GCC. If
GCC is missing, the wizard prints the detected distribution's compiler
package command; install it and rerun the same `mount-setup` command. The
wizard checks helper ownership, protected path modes, the setuid bit, and
`nosuid` or `noexec` mount options. The
[helper README](../../contrib/degoo/immutable-mount/README.md) explains the
helper contract and includes optional advanced manual build steps.

After `mount-setup` finishes, continue as your normal user; do not prefix
these commands with `sudo`:

When the systemd user manager is available, enable the service and inspect the
result:

```sh
"$HOME/.local/share/rclone-degoo/degoo-rc" service enable
"$HOME/.local/share/rclone-degoo/degoo-rc" info
```

Without systemd, start the mount as your normal user in one terminal:

```sh
"$HOME/.local/share/rclone-degoo/degoo-rc" mount
```

Run the status check as your normal user from another terminal:

```sh
"$HOME/.local/share/rclone-degoo/degoo-rc" info
```

For an already enabled service, run `degoo-rc service restart` as the normal
user. When a known blocker prevents startup, the wizard prints these
instructions without waiting for input and preserves the saved configuration
and pending cache data for retry. The helper is not needed for a mutable
mountpoint.
The wizard reports setup as ready only after it confirms the requested mount.

## Start the installer

From a repository checkout, build the binary and start the wizard:

```sh
mkdir -p build
go build -o build/rclone .
./contrib/degoo/degoo-rc install
```

Go 1.26 or newer is required only for this source-build path. In a terminal,
the source command starts the wizard. A detected executable beside the launcher,
in the archive's `bin/` directory, or in the source build location is validated
and reused without a binary-path prompt. If it is missing or invalid, the
wizard offers the existing build/path flow. To select a binary explicitly:

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

## Build the distributable archive

GitHub Actions builds the Linux amd64 archive after code changes are merged
into `staging` (or pushed directly to it). Documentation-only updates under
`docs/` or `graphics/`, or to Markdown and reStructuredText files, are skipped.
Changes that include both documentation and code still build. This is the
fork's only workflow; pull requests, tags, and other branches do not run it.

Download the `rclone-degoo-linux-amd64` artifact from a successful
[`degoo-package` run](https://github.com/georgeasiminei/rclone-degoo/actions/workflows/degoo-package.yml).
Extract the artifact ZIP to get `rclone-degoo-linux-amd64.tar.gz` and its
`.sha256` sidecar, then verify the checksum as shown below. These builds are
workflow artifacts rather than GitHub releases.

From the repository root, build the Degoo-enabled binary and create the Linux
amd64 archive with its SHA-256 sidecar:

```sh
mkdir -p build
go build -o build/rclone .
python3 bin/package-degoo.py \
  --binary build/rclone \
  --output dist/rclone-degoo-linux-amd64.tar.gz
```

The package builder checks the binary's Degoo command, assembles the allowlisted
installer files and documentation, then validates the archive in an isolated
temporary home. To run its fixture-based packaging checks separately:

```sh
python3 bin/package-degoo.py --self-check
```

Verify the generated checksum from the output directory:

```sh
cd dist
sha256sum -c rclone-degoo-linux-amd64.tar.gz.sha256
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
At the folder-name step, the wizard lists folders under the selected parent and
reports whether the suggested name already exists. Existing native Degoo files
are not encrypted-vault files and cannot be read as such. If a folder with the
chosen name already exists, use its matching private `vault.json` to open an
encrypted vault or choose another name for a new vault. The wizard will not
overwrite an existing encrypted folder with a newly generated key.

The wizard generates a cryptographically random vault encryption password for
a new vault. Use `degoo-rc backup-config` to keep a private configuration backup
outside the install before saving important data. This removes account
credentials while retaining the vault key. Degoo account credentials and the
vault password
serve separate purposes. Once the vault configuration exists, `degoo-rc login`
saves the account credentials encrypted with the vault data key, enabling
automatic password login after rejected token renewal. Existing installations
need one successful login after upgrading to populate this encrypted field.
If the first wizard login happened before vault creation, run `degoo-rc login`
after setup to enable the fallback.

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

To create a configuration backup while keeping the installation and mount:

```sh
degoo-rc backup-config --directory ~/private-backups/rclone-degoo
```

Omit `--directory` to create a new timestamped private backup at the default
location. This saves the original vault key, folder binding, remembered email,
and mount settings. The Degoo account password, including its encrypted form,
and access/refresh tokens are excluded. Log in again after restoring.
Cache, pending uploads, logs, and binaries are excluded.
Let uploads finish before expecting those changes on another computer.
Transfer this private directory securely; it contains the vault encryption
secret.

To mount the same remote vault on another computer, restore this backup or
state retained by the uninstall wizard with a fresh interactive installation:

```sh
./contrib/degoo/degoo-rc install --wizard \
  --restore-directory ~/private-backups/rclone-degoo
```

`--restore-directory` is available only with `--wizard`. Restore requires the
default install root to be absent; if it exists, the wizard refuses and leaves
both it and the backup untouched. The wizard asks you to confirm the recovered
folder ID and name and use the saved mount settings as defaults. It preserves
the original vault password, salt, chunk size, and object cap. It restores
retained private configuration files and, when present, copies the
saved cache into the new install. Cache identity is rebound locally for the new
path, including when the cache is moved to another machine; the source backup
is left intact. Pending uploads remain in the cache for later retry.

If you do not pass `--restore-directory`, the wizard offers an optional backup
directory prompt; leave it blank for a normal installation. The restore uses
the regular cache-settings step with the saved values as defaults and adds no
separate cache-tuning prompts.

Configuration and uninstall-retention backups exclude account tokens and
passwords. Restoration always requires a fresh login, including restoration
from older backups that contain credentials; those credentials are ignored.
During an ordinary
upgrade the account check can use the existing installed credentials and skip
the password prompt after a successful listing. If Degoo rejects the saved
login, the wizard asks you to log in and tests the connection again.
Provider or network errors stop setup with their connection
failure; they are not reported as a wrong password. This check verifies Degoo
account authentication, not the vault password or the ability to decrypt
remote files. Restoring an existing encrypted folder requires its original
`vault.json`; the wizard cannot replace its encryption settings with a new key.

## Defaults

The installer uses `~/.local/share/rclone-degoo` as the private root and sets
its directories to owner-only access (`0700`) and private config files to
`0600`. The wizard offers to enable the systemd user service by default when a
user manager responds. It does not enable lingering automatically.

New contained installs mount at `/mnt/degoo`. The installer tries to create
that directory with mode `0700`. If it cannot create or secure the path, it
prints privileged commands for the selected mountpoint and current user, such
as:

```sh
sudo mkdir -p -- /mnt/degoo
sudo chown -- "$(id -u):$(id -g)" /mnt/degoo
sudo chmod 700 -- /mnt/degoo
sudo chattr +i -- /mnt/degoo
```

The Linux mount checks that the directory belongs to the user and refuses to
hide files already in it. You may leave `chattr +i` set on `/mnt/degoo` while
the mount is running and while it is stopped; the installer never clears it.
Standard `fusermount3` cannot mount on an immutable directory because the flag
blocks its required write-access check, even if the directory is
owner-writable. Keeping the flag set requires an administrator to build and
install the narrow helper documented in
[immutable-mount/README.md](../../contrib/degoo/immutable-mount/README.md).
When this helper is needed, the mount error points to those commands and the
service command to retry after setup. It does not install privileged files
automatically. The helper accepts only an empty `/mnt/degoo` owned by the
mounting user and not writable by group or other users; the default directory
remains owner-private. It passes that user's ID to the kernel mount, leaves the
immutable flag unchanged, and rejects `allow_other` and `allow_root`; rclone
itself runs as the ordinary user. When the flag is absent, the mount guard
removes write permission through the retained directory handle after mounting
and again on failure or unmount.

The installer helpers accept `/mnt/degoo` or the legacy in-install `mount`
directory. Upgrades preserve the recorded mountpoint by default. To migrate an
older install to `/mnt/degoo`, pass `--mountpoint /mnt/degoo`; migration is
refused while the old underlying `mount/` directory contains files. Move those
files out and retry. The external `/mnt/degoo` directory is preserved during
uninstall. The privileged helper does not support an immutable legacy
`mount/` target. For an install whose saved mountpoint is immutable, run
`degoo-rc install --mountpoint /mnt/degoo` to migrate the saved path while
preserving the cache. The old underlying `mount/` directory must be empty.
This migration does not clear an immutable flag; the root helper supports only
`/mnt/degoo`. If the old legacy directory itself is immutable, it remains
protected and may need a separate manual unlock before uninstall cleanup.

For a new vault, the default chunk size is 128 MiB of plaintext and the
provisional maximum encrypted object size is 19 GiB. The provider's live limit
above 20 GiB has not been established. The initial `config/mount.json` values
are:

| Setting | Default | Meaning |
| --- | --- | --- |
| `cache_max_size` | `60G` | Soft target for the amount of cached data. Open and dirty files are not evicted to meet it. |
| `cache_min_free_space` | `60G` | Keep this much free space on the filesystem holding the cache when evicting clean data. |
| `cache_max_age` | `750h` | Maximum age for clean cached data before it becomes eligible for eviction. |
| `cache_poll_interval` | `5s` | How often the local cache cleaner checks for stale clean files. |
| `write_back` | `5s` | Wait after a file closes before attempting its upload. Must be positive. |
| `verify_upload` | `false` | Enable download of each encrypted upload and compare its size and SHA-256 before publication. |
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

Changes published by another client are subject to directory cache staleness.
The 30-minute directory TTL refreshes on access; the five-second local cache
cleanup interval does not poll remote changes. Provider visibility and folder-ID
mapping can delay external changes further, so there is no strict end-to-end
bound. Use `degoo-rc sync .` inside the mounted directory, or pass an absolute
mounted path, to refresh its directory tree now while preserving pending writes.
File paths refresh their parent directory. Reopen files to see changed content.
After a client upgrade, restart older mounts to enable this command.
See [remote changes and cache staleness](USAGE.md#remote-changes-and-cache-staleness).

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

Change these values in `config/mount.json` and restart the mount or service to
apply them. Set `write_back` to a shorter duration to queue closed files sooner.
Zero, negative, and `off` values are refused so writes keep using the background
upload and retry queue. This setting also controls the initial retry delay;
rclone doubles the delay after each failed attempt. Ordinary upgrades preserve
existing settings. Older files without `write_back` retain rclone's five-second
default; add `"write_back": "1s"` explicitly to shorten it.
Set `verify_upload` to `false` to skip the full download and SHA-256 check;
metadata and publication checks still run. Cache limits are targets: open files
and pending writes remain in the cache. A newly written logical file is staged
locally in full even when remote storage splits it into chunks.

Pending status includes queued, active, and failed uploads awaiting retry.
Inspect `degoo-rc logs` for failures. A failed registration can occur after the
encrypted bytes have reached storage; reducing the close delay or disabling
readback verification does not resolve a provider registration rejection.

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
  mount/              legacy mountpoint when explicitly selected
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
root-level Degoo command. Upgrades may retain root-level `.sh` shims already
present in an older install. The Degoo package in a source checkout and the
extracted archive use `degoo-rc` as their sole installer and maintenance
entrypoint.
Runtime Python bytecode and temporary upload files stay contained or are
disabled.

## Login, mount, and recovery

The wizard finishes with a summary of the install directory, mount location,
current mount state, helper commands, and log paths. A service start request
does not establish that the mount is ready: the wizard waits briefly for the
kernel's mount table to show a connected mount. If requested startup fails or
remains unconfirmed, installation reports an explicit error and returns a
nonzero exit status. Completed configuration remains available for retry.
Startup errors include commands to inspect status and logs. Choosing manual
startup allows installation to finish without mounting. The default
mount path is `/mnt/degoo`. Open that directory in a file manager or use it like
any other directory while mounted. Ordinary files created there while the
mount is absent remain local-only and are never uploaded; a mount refuses to
cover a nonempty directory. The installed command dispatches
to helpers in `~/.local/share/rclone-degoo/scripts/`, rather than using files
from the extracted package folder.

Run `degoo-rc login` to authenticate. It remembers the email in
`config/login.json` with owner-only permissions and saves account credentials
encrypted by the vault key in `config/vault.json` when that file exists.
Use `degoo-rc login --clear-email` to forget the email without
removing existing tokens or vault settings.

The password prompt is shown in the terminal even when command logs are
written to a file. Password characters are not echoed or written to logs.

Access tokens refresh automatically when nearing expiry. If Degoo rejects a
refresh, the mount reports that login is needed. Run `degoo-rc login` again;
the authentication client can load the newly saved credentials. Long-term
unattended renewal has not been verified. Earlier probes returned HTTP 403 and
HTTP 401 during token renewal; authentication was later renewed and read-only
provider queries succeeded. This does not establish reliable automatic
renewal. See [VALIDATION.md](VALIDATION.md) for the current validation state.

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

Use `degoo-rc status` for authentication-first health and upload status. It
returns a nonzero exit status when authentication, mount/service health, or
local upload checks are unhealthy. A file path adds per-file status, and
`--json` returns machine-readable output:

```sh
degoo-rc status
degoo-rc status /mnt/degoo/report.pdf --json
```

For mounted files, status reads the VFS's read-only
`user.rclone.upload-status` attribute: `pending`, `uploading`, `uploaded`, or
`unknown`. `uploaded` means VFS knows the current version has a remote
representation after writeback; it does not freshly audit the remote object.
When the mount is absent, files under `/mnt/degoo` are reported as
`local-only`, separate from any persisted cache record, and are never uploaded.
The private mount socket contributes aggregate queued, uploading, and
retrying counts only.

## Logs and authentication errors

Show recent mount, login, setup, status, and Degoo diagnostic messages, or
watch new messages:

```sh
degoo-rc logs
degoo-rc logs --follow
```

The contained `status.log` records status-command diagnostics and
`diagnostics.log` holds Degoo runtime diagnostics. Failures are shown before
other report details, without Cobra usage blocks. Press Ctrl+C to leave follow
mode. `degoo-rc logs` is the viewer;
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
changes can be recovered. Saved account passwords and authentication tokens
are excluded even when configuration is retained; restoration requires login.
If you choose not to retain a vault config, type
`DISCARD` to confirm losing the local secret. Type `REMOVE` to confirm local
cleanup.

Uninstall reports success only after verifying that the installation directory
and its owned command and service links are absent. A cleanup failure returns
a nonzero exit status and reports that uninstall is incomplete. Selected
backups and the external `/mnt/degoo` mountpoint are preserved intentionally.
Interrupting cleanup also reports incomplete removal once deletion is requested.
An immutable legacy `mount/` directory must be unlocked before deletion; the
error supplies a `sudo chattr -i` command. Retry uninstall after clearing that
flag. The directory check runs before removing service or command links.

If an earlier cleanup removed the service links but left the installation,
the install wizard queries systemd to verify that the unit is absent and
inactive before allowing an upgrade. Missing links alone do not prove the
service has stopped; an unrelated loaded unit is never stopped.

To resume uploads from a retained backup, install the client into a fresh
default install root with the wizard and pass the backup directory:

```sh
./contrib/degoo/degoo-rc install --wizard \
  --restore-directory ~/private-backups/rclone-degoo
```

The restore copies selected private state into the new installation; do not
manually copy the backup over the install path. The original backup remains
available if recovery needs to be retried. `--restore-directory` requires a
fresh install and is rejected when the default root already exists. It cannot
be used with `--no-wizard`.

The cleanup stops the owned service before disabling it, removes its systemd
links, reloads the user manager, and removes the contained configuration,
cache, logs, binary, and scripts. Only explicitly retained state remains in
the chosen external backup directory. Remote encrypted files and Degoo
account data are untouched. The external `/mnt/degoo` directory is also left
in place; it remains protected against unmounted writes. Shared dependencies,
parent systemd directories,
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
to select data for retention, or a private `--vault-backup` with matching vault
encryption settings and the
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

Source checkouts and extracted archives use `degoo-rc` as the sole root-level
Degoo installer and maintenance entrypoint. Fresh installs also use only
`degoo-rc`; in-place upgrades may retain root-level `.sh` shims already present
in an older install. New commands and examples should use `degoo-rc`.

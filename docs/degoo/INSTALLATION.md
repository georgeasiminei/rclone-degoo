# Contained Degoo installation

See [DEGOO.md](../../DEGOO.md) for the shortest installation path. The installer
requires Python 3, Linux FUSE, and this fork's rclone binary. Upstream rclone
cannot mount this vault. Systemd is required only for the persistent service.

## Install a new vault

From this repository, build and install:

```sh
go build -o rclone .
python3 contrib/degoo/install.py --binary ./rclone
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/folders.sh
~/.local/share/rclone-degoo/setup.sh --parent-id DEVICE_ID --name EncryptedVault
```

Choose a device container ID from the folder listing. Account root ID `0`
cannot hold a vault. Setup creates a new dedicated directory with the
provider-specific marker needed by this backend. It refuses to repurpose an
existing directory with the same name. Existing native Degoo files cannot be
read as encrypted vault files.

Setup generates a cryptographically random encryption password. Preserve a
private backup of `config/vault.json` outside the installation before storing
important data. The Degoo account password and vault encryption password have
separate roles; account passwords are never saved.

To install an existing compatible encrypted vault, supply its folder ID and
securely transferred vault configuration:

```sh
python3 contrib/degoo/install.py --binary ./rclone \
  --folder-id EXISTING_VAULT_ID --vault-config ~/private-backups/degoo-vault.json
~/.local/share/rclone-degoo/login.sh
```

The encryption password, optional salt, chunk size, and object cap are
preserved. Local remote and cache paths are adapted to this installation.
The encryption password alone is insufficient to locate a remote vault: you
also need its folder ID and matching format settings. One machine should write
to a vault at a time.

## Files and permissions

```text
~/.local/share/rclone-degoo/
  bin/rclone          dedicated executable
  config/auth.json    access and refresh tokens
  config/login.json   remembered account email; no account password
  config/vault.json   encryption secret and vault format settings
  config/rclone.conf  dedicated remote and folder ID
  config/mount.json   cache limits and upload verification
  cache/              persistent plaintext VFS cache, lock, and identity
  mount/              plaintext filesystem while mounted
  tools/              contained FUSE helper alias
  tmp/                temporary upload files
  logs/               private runtime logs
  install.json        installation inventory
  rclone-degoo.service user service definition
  login.sh            interactive login; --clear-email forgets the email
  folders.sh          list setup parent folders
  setup.sh            create and configure a new dedicated vault
  location.sh         show the vault's remote ancestor folders
  mount.sh            foreground mount launcher
  status.sh           pending-upload status
  stop.sh             stop/unmount the vault
  service.sh          enable, disable, start, stop, restart, or inspect service
  uninstall.py        local cleanup tool
```

Installation directories are owner-private (`0700`); configuration files are
`0600`. `install -d -m 700 DIR` creates a directory and sets these permissions;
it does not install a package. Neither the normal rclone configuration nor
shell profiles are changed. Runtime Python bytecode and temporary upload files
stay contained or are disabled.

## Login and recovery

```sh
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/login.sh --clear-email
```

Login prompts with the remembered email and explains how to forget it. The
password prompt identifies the account. Clearing the email leaves existing
authentication tokens and encryption settings intact. Access tokens refresh
automatically when nearing expiry. If Degoo rejects refresh, the mount reports
that login is required. Run the login script again; the authentication client
can load freshly saved credentials. Restarting the service is also supported.

This account's live renewal returned HTTP 403 after a one-hour access token
expired. Fresh interactive login and renewal of the new session succeeded.
Successful long-term unattended renewal remains unverified. The
service does not retain your account password to work around rejected refresh.

## Mount and persist

For a manual mount, keep this command running in a shell:

```sh
~/.local/share/rclone-degoo/mount.sh
```

The mountpoint is an ordinary empty directory when unmounted; data is stored
in Degoo and the local cache. Use the user service for unattended operation:

```sh
~/.local/share/rclone-degoo/service.sh enable
~/.local/share/rclone-degoo/service.sh status
~/.local/share/rclone-degoo/service.sh restart
```

Enabling starts the service and starts it again when your user manager starts.
To start at boot before login and keep running after logout, check and enable
lingering as needed:

```sh
loginctl show-user "$USER" -p Linger
loginctl enable-linger "$USER"
```

The latter may require administrator authorization. No root mount service is
installed. The installer does not change lingering automatically. Service
links are the only installation artifacts outside the contained directory;
the service file itself stays inside it. The service restarts after process
failures, but an authentication failure during ordinary operations may leave
an active mount that cannot read Degoo until login succeeds.

Use `service.sh disable` to stop the service and remove automatic startup.
Keep only one mount process for this cache. Before replacing a binary, stop the
service or manual mount and use the installer's upgrade operation. Upgrade
preserves credentials, the vault secret, cache, and existing mount settings.

## Cache settings and verification

Edit `config/mount.json`, then restart the service:

```json
{
  "cache_max_size": "20G",
  "cache_min_free_space": "20G",
  "cache_max_age": "10m",
  "cache_poll_interval": "5s",
  "verify_upload": true
}
```

The contained launcher passes this file through `--settings`. Explicit mount
flags take precedence, for example:

```sh
~/.local/share/rclone-degoo/mount.sh --vfs-cache-max-size 40G --verify-upload=false
```

With verification enabled, each registered encrypted upload is downloaded and
compared against its full size and SHA-256 before publication. Disabling it
skips this payload download; metadata, identity, size, and publication checks
remain. It reduces bandwidth and latency while weakening immediate detection
of corrupted uploads. Authenticated encryption still detects ciphertext damage
when reading the affected data. The raw backend also exposes the advanced
`--degoo-skip-upload-verification` flag; use the mount setting for the vault.

Cache targets are soft limits. Open and dirty files cannot be evicted. A newly
written logical file is staged in full locally, even though remote storage
uses smaller chunks. Upload starts after the file is closed. Reads cache the
requested ranges and read-ahead. Keep the cache until `status.sh` reports zero
pending uploads; it can contain the only copy of unuploaded changes.

## Cleanup

First inspect the installation without deleting anything:

```sh
python3 ~/.local/share/rclone-degoo/uninstall.py
```

Preserve a private copy of `config/vault.json` outside the installation, then:

```sh
python3 ~/.local/share/rclone-degoo/uninstall.py \
  --delete-local --vault-backup ~/private-backups/degoo-vault.json
```

The uninstaller verifies the backup, refuses pending uploads and unsafe paths,
stops the owned service, removes its owned links, and removes the contained
installation. It leaves unrelated service files and account settings alone.
The explicit `--discard-vault-secret` alternative permits deletion without a
backup; losing the encryption secret makes the remote files unreadable.

Remote encrypted objects and Degoo sessions remain untouched. Uninstall does
not disable an existing account-wide lingering setting. A lingering setting
you enabled solely for this service can be disabled separately with
`loginctl disable-linger "$USER"`, after considering your other user services.
The checkout and shared Go toolchain/module/build caches are development
resources outside the vault installation and are not removed. Installing a
prebuilt binary avoids those development artifacts.

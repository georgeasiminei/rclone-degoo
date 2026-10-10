# Contained Linux Degoo install

`install.py` installs a supplied Linux rclone executable that includes the
Degoo commands. It does not build rclone or change a normal rclone installation.
The private install lives at `~/.local/share/rclone-degoo`; account tokens,
vault settings, cache, logs, and wrappers stay there.

The home directory and `~/.local` plus `~/.local/share` must be owned by the
current user and not writable by other users. If an existing shared parent is
too permissive, inspect it and restrict its write permissions before installing,
for example with `chmod go-w ~/.local/share`. The installer does not change
shared directory permissions automatically.

## Install and choose a vault

To configure an existing dedicated Degoo folder, run:

```sh
python3 contrib/degoo/install.py \
  --binary /path/to/rclone \
  --folder-id DEDICATED_FOLDER_ID \
  --folder-name 'My encrypted vault'
```

The folder ID must name a dedicated folder, not account root `0`. The installer
creates a private vault password on first setup and writes the remote config for
the fixed `degoo-live-test` remote. A folder name is optional and is only local
install metadata.

For a new vault, first install the client without a folder ID, log in, list the
Degoo device containers, and create a dedicated folder under the chosen device:

```sh
python3 contrib/degoo/install.py --binary /path/to/rclone
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/folders.sh --parent-id 0
~/.local/share/rclone-degoo/setup.sh --parent-id DEVICE_ID --name 'My encrypted vault'
```

`folders.sh` prints JSON folder records with IDs, names, and categories. Pass a
device container ID from that output to `setup.sh`. The setup command creates a
dedicated remote folder, then saves its ID in the local rclone config. If remote
creation succeeds but local setup fails, the helper prints the created ID so
setup can be retried with `--folder-id ID --name NAME`.

To use an existing vault on another machine, provide a private copy of its
`vault.json` during install (or during staged `setup.sh`). The password and
encryption parameters are preserved; the remote name and cache path are adapted
to this installation. Account login is separate and must be completed on each
machine:

```sh
python3 contrib/degoo/install.py \
  --binary /path/to/rclone \
  --folder-id DEDICATED_FOLDER_ID \
  --vault-config ~/private-backups/vault.json
```

Treat `vault.json` as the secret needed to decrypt the remote data. Keep backups
private and outside the install directory.

## Login, mount, and settings

Run `~/.local/share/rclone-degoo/login.sh` to authenticate. The helper remembers
the email address in `config/login.json` with owner-only permissions. It never
saves the account password. If the email needs to be changed, run
`login.sh --clear-email` and then log in again; this only removes the remembered
email and does not log out or delete tokens.

Run `mount.sh` to mount the vault at the contained `mount/` directory in the
foreground, or use `service.sh enable` to start it as a systemd user service.
`status.sh` reports pending local uploads; `location.sh` shows the configured
remote folder ancestry; `stop.sh` unmounts the contained mount. Edit
`config/mount.json` for cache and upload behavior. Defaults are 20 GiB maximum
cache size, 20 GiB minimum free space, 10 minute maximum age, 5 second polling,
and upload verification enabled. Restart the service after changing settings.

The service unit and wrappers are installed inside the private root. Enabling
the service creates only the systemd user links recorded in `install.json`.
`service.sh` supports `enable`, `disable`, `start`, `stop`, `restart`, and
`status`. Stop leaves the service enabled; disable stops it and removes its
enable links. The configured user systemd directories must be owned by the
current user and not writable by others; service setup reports the path and a
`chmod go-w` hint when an existing directory needs attention. A user systemd
manager usually starts at login. To keep it running
through logout and start it at boot, enable lingering for the account with
`loginctl enable-linger "$USER"` if the system administrator or policy permits.

Upgrade the contained binary and helper scripts with:

```sh
python3 contrib/degoo/install.py --binary /path/to/new/rclone --upgrade
```

Stop the mount before upgrading. The installer refuses an active mount or cache
lock and preserves the vault secret, account tokens, email, rclone settings,
mount settings, and cache. It also refuses an upgrade that changes the folder ID.

## Uninstall

The default command inventories the install without changing it. `--root` is
optional and defaults to `~/.local/share/rclone-degoo`:

```sh
~/.local/share/rclone-degoo/uninstall.py
```

The inventory lists files and any external systemd links owned by the install,
but does not print token, vault, status, or log contents. To remove the local
install, first back up `config/vault.json` privately outside the install root.
The cleanup verifies the backup matches exactly:

```sh
~/.local/share/rclone-degoo/uninstall.py \
  --delete-local \
  --vault-backup ~/private-backups/vault.json
```

If deliberately discarding the only local vault secret, use
`--discard-vault-secret` instead. Without a vault secret, an incomplete staged
install can be removed without that option if its cache is empty.

Deletion checks for pending local uploads before stopping an owned service, then
checks again while holding the cache lock. It refuses cleanup if pending uploads
remain, the cache is active, or another mount exists inside the install. Keep the
cache until uploads reach zero; it may contain the only copy of unuploaded
changes as well as plaintext cache data. The cleanup stops and disables only the
service linked to this installation and removes only its recorded external
links. Empty shared directories under the user systemd config path may remain.

The cleanup removes local account tokens, vault configuration, cache, logs, and
wrappers. It deliberately leaves remote encrypted objects in the dedicated
Degoo folder untouched. Remote removal is separate.

Run the offline installer, login, service, setup, and cleanup checks with:

```sh
python3 -m unittest contrib.degoo.test_install contrib.degoo.test_uninstall
```

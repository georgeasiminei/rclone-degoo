# Encrypted Degoo drive for Linux

This fork mounts a dedicated Degoo folder as an encrypted, writable Linux
filesystem. Files, directory names, and chunk manifests are encrypted before
upload. You can list, read, write, rename, move, and delete files within the
vault. Copies download and upload data; server-side copying is unavailable.

The installer contains the binary, credentials, encryption settings, cache,
logs, and scripts in `~/.local/share/rclone-degoo`. A user service can mount the
drive at login or boot. Uninstall removes the installation and its service
links while preserving the remote vault.

## Install and mount

You need Linux, Python 3, FUSE, and this fork's binary. Ordinary upstream rclone
does not include this Degoo implementation. Build with Go 1.26 or newer:

```sh
go build -o rclone .
python3 contrib/degoo/install.py --binary ./rclone
~/.local/share/rclone-degoo/login.sh
~/.local/share/rclone-degoo/folders.sh
```

The folder command lists the account's device containers as JSON. Pick a
container ID, then create your dedicated vault:

```sh
~/.local/share/rclone-degoo/setup.sh --parent-id DEVICE_ID --name EncryptedVault
~/.local/share/rclone-degoo/service.sh enable
ls ~/.local/share/rclone-degoo/mount
~/.local/share/rclone-degoo/info.sh
```

For a service that also starts before login and survives logout, enable user
lingering if it is not already enabled:

```sh
loginctl enable-linger "$USER"
```

That account setting may need administrator authorization. The installer does
not change it automatically or disable an existing setting during uninstall.
For manual foreground mounting, use `mount.sh` instead of enabling the service.
See [installation and cleanup](docs/degoo/INSTALLATION.md) for configuration,
existing vaults, service management, and uninstall.

`info.sh` reports the mount and service state, configuration locations and
settings, pending uploads, authentication expiry, and local disk/cache use.
Its size scan is bounded and excludes the mounted cloud tree. Use
`info.sh --check-remote` for an optional metadata-only credential and location
check; it does not download file contents or total the remote vault size.

## Cache and verification

Edit `~/.local/share/rclone-degoo/config/mount.json` to configure cache limits
and upload verification. Restart the service after changing it.

```json
{
  "cache_max_size": "20G",
  "cache_min_free_space": "20G",
  "cache_max_age": "10m",
  "cache_poll_interval": "5s",
  "verify_upload": true
}
```

Verification downloads each uploaded encrypted object and compares its full
size and SHA-256 before publication. Set `verify_upload` to `false`, or pass
`--verify-upload=false` to `mount.sh`, to skip that readback. Metadata and
publication checks remain enabled. Disabling verification saves download
traffic and time but leaves corrupted upload contents undetected until read.

A newly written file is staged in full locally before upload starts. Cache
limits are eviction targets: open and pending files cannot be evicted. A 10 TB
collection can fit in the cloud when individual files and pending writes fit
locally; a single 10 TB write cannot fit in a 20 GB local cache. Keep the cache
until `status.sh` reports zero pending uploads.

## Share the software; preserve the vault secret

Give other users this repository or a matching Linux binary and the
`contrib/degoo` installer files. Each account creates its own vault and logs in
with its own Degoo credentials. There is no separate published Degoo release
artifact at present.

To access your existing vault from another machine, securely transfer
`config/vault.json` and supply the same remote folder ID. The installer adapts
local paths. The encryption password and optional salt must stay unchanged.
Use one writable mount per vault; writers on separate machines are not
coordinated.

## Current validation limits

Persistence is available, but this implementation remains experimental. Live
encrypted writes, readback, range reads, and file/directory operations passed.
The tested account's one-hour access token expired and renewal returned HTTP
403; unattended long-term authentication has not been established. Larger live
files, large directory pagination, and interrupted multi-chunk recovery also
remain acceptance gates. See [validation](docs/degoo/VALIDATION.md).

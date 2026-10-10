<!-- markdownlint-disable-next-line first-line-heading no-inline-html -->
[<img src="https://rclone.org/img/logo_on_light__horizontal_color.svg" width="50%" alt="rclone logo">](https://rclone.org/#gh-light-mode-only)
<!-- markdownlint-disable-next-line no-inline-html -->
[<img src="https://rclone.org/img/logo_on_dark__horizontal_color.svg" width="50%" alt="rclone logo">](https://rclone.org/#gh-dark-mode-only)

[Website](https://rclone.org) |
[Documentation](https://rclone.org/docs/) |
[Download](https://rclone.org/downloads/) |
[Contributing](CONTRIBUTING.md) |
[Changelog](https://rclone.org/changelog/) |
[Installation](https://rclone.org/install/) |
[Forum](https://forum.rclone.org/)

[![Build Status](https://github.com/rclone/rclone/workflows/build/badge.svg)](https://github.com/rclone/rclone/actions?query=workflow%3Abuild)
[![Go Report Card](https://goreportcard.com/badge/github.com/rclone/rclone)](https://goreportcard.com/report/github.com/rclone/rclone)
[![GoDoc](https://godoc.org/github.com/rclone/rclone?status.svg)](https://godoc.org/github.com/rclone/rclone)
[![Docker Pulls](https://img.shields.io/docker/pulls/rclone/rclone)](https://hub.docker.com/r/rclone/rclone)

# Rclone

Rclone *("rsync for cloud storage")* is a command-line program to sync files and
directories to and from different cloud storage providers.

## Degoo encrypted mount for Linux

This fork includes an encrypted, writable Degoo mount for Linux. Install it
from the `staging` source branch or from the matching prebuilt Linux amd64
archive. The archive is a distributable installer package that can be shared
with other Linux amd64 users; it does not contain account credentials or a
vault secret. No GitHub release archive has been uploaded yet.

### Install from source

```sh
git clone --branch staging https://github.com/georgeasiminei/rclone-degoo.git
cd rclone-degoo
mkdir -p build
go build -o build/rclone .
./contrib/degoo/degoo-rc install
```

Go 1.26 or newer is needed to build from source. The wizard then installs the
contained client, checks mount dependencies, logs into Degoo, and helps select
or create a dedicated vault folder.

### Install from the prebuilt archive

If you have the matching `rclone-degoo-linux-amd64.tar.gz` archive, place it in
`~/Downloads`. No archive is currently available as a GitHub release download.
The checksum sidecar should be alongside the tarball. Verify it, then extract
the tarball into a new temporary directory and start its wizard:

```sh
cd "$HOME/Downloads"
sha256sum -c rclone-degoo-linux-amd64.tar.gz.sha256
extract_dir=$(mktemp -d "$HOME/Downloads/rclone-degoo.XXXXXX")
tar -xzf "$HOME/Downloads/rclone-degoo-linux-amd64.tar.gz" -C "$extract_dir"
cd "$extract_dir/rclone-degoo"
./degoo-rc install
```

The prebuilt archive does not need Go. Both install paths need Python 3.10 or
newer for the installer. Mounting also needs FUSE 3 or a compatible
`fusermount` helper, readable and writable `/dev/fuse`, and `mountpoint` from
`util-linux`. The wizard can offer to install missing `fuse3` and `util-linux`
packages using a detected package manager. A systemd user manager is optional;
without it, mount in the foreground.

The installer puts `degoo-rc` at `~/.local/share/rclone-degoo/degoo-rc` and
links `~/.local/bin/degoo-rc` to it. It does not edit shell profiles; if that
directory is missing from `PATH`, it prints an export command. Use
`degoo-rc help` for the command list and `degoo-rc COMMAND help` for detailed
options. A fresh install has `degoo-rc` as its only root-level command;
upgrades retain existing root-level `.sh` shims, and source/archive roots may
include the legacy install, uninstall, and trash shims. New examples use
`degoo-rc`.

### Use the installed vault

The wizard finishes with the install directory, mount location, current mount
state, helper commands, and log locations. The default mount is
`~/.local/share/rclone-degoo/mount`. Open that folder in your file manager
while it is mounted, or use it like any other directory from the terminal.
Use the installed command, available on `PATH`:

```sh
degoo-rc info
degoo-rc logs
degoo-rc logs --follow
```

Press Ctrl+C to stop following logs. `degoo-rc logs` is the viewer; `logs/` is a
directory. New mounts log notices and errors by default. The upgrade wizard
offers to stop the contained mount and waits briefly for it to start again.
To stop the user service manually:

```sh
degoo-rc service stop
```

`degoo-rc info` shows mount/service state and authentication warnings.
`degoo-rc logs` shows recent mount, login, setup, and status logs; `--follow`
watches new messages. If a token cannot be renewed, run `degoo-rc login` again.
Keep the cache and vault
configuration while uploads are pending.

For a fresh reinstall, run the uninstall wizard from outside the install
(`cd ~` first). Choose No for all three keep options to create no backup;
confirm `DISCARD` (when a vault secret exists) and `REMOVE`. If you keep
anything, the private backup
prompt asks for an absolute directory path; press Enter for the default.
Discarding the encryption settings requires creating a new vault afterward.
The installation guide includes removal checks and fresh archive instructions.

See [the Degoo installation guide](docs/degoo/INSTALLATION.md) and
[the quick start](DEGOO.md). Live support above 20 GiB, long-term token renewal,
and several recovery cases remain unverified; see
[validation status](docs/degoo/VALIDATION.md) before relying on the vault.

## Storage providers

- 1Fichier [:page_facing_up:](https://rclone.org/fichier/)
- Akamai Netstorage [:page_facing_up:](https://rclone.org/netstorage/)
- Alibaba Cloud (Aliyun) Object Storage System (OSS) [:page_facing_up:](https://rclone.org/s3/#alibaba-oss)
- Amazon S3 [:page_facing_up:](https://rclone.org/s3/)
- ArvanCloud Object Storage (AOS) [:page_facing_up:](https://rclone.org/s3/#arvan-cloud-object-storage-aos)
- Bizfly Cloud Simple Storage [:page_facing_up:](https://rclone.org/s3/#bizflycloud)
- Backblaze B2 [:page_facing_up:](https://rclone.org/b2/)
- Box [:page_facing_up:](https://rclone.org/box/)
- Ceph [:page_facing_up:](https://rclone.org/s3/#ceph)
- China Mobile Ecloud Elastic Object Storage (EOS) [:page_facing_up:](https://rclone.org/s3/#china-mobile-ecloud-eos)
- Citrix ShareFile [:page_facing_up:](https://rclone.org/sharefile/)
- Cloudflare R2 [:page_facing_up:](https://rclone.org/s3/#cloudflare-r2)
- Cloudinary [:page_facing_up:](https://rclone.org/cloudinary/)
- Cubbit DS3 [:page_facing_up:](https://rclone.org/s3/#Cubbit)
- DigitalOcean Spaces [:page_facing_up:](https://rclone.org/s3/#digitalocean-spaces)
- Digi Storage [:page_facing_up:](https://rclone.org/koofr/#digi-storage)
- dosya.dev [:page_facing_up:](https://rclone.org/dosya/)
- Dreamhost [:page_facing_up:](https://rclone.org/s3/#dreamhost)
- Drime [:page_facing_up:](https://rclone.org/s3/#drime)
- Dropbox [:page_facing_up:](https://rclone.org/dropbox/)
- Enterprise File Fabric [:page_facing_up:](https://rclone.org/filefabric/)
- Exaba [:page_facing_up:](https://rclone.org/s3/#exaba)
- Fastly Object Storage [:page_facing_up:](https://rclone.org/s3/#fastly)
- Fastmail Files [:page_facing_up:](https://rclone.org/webdav/#fastmail-files)
- FileLu [:page_facing_up:](https://rclone.org/filelu/)
- Filen [:page_facing_up:](https://rclone.org/filen/)
- Files.com [:page_facing_up:](https://rclone.org/filescom/)
- FlashBlade [:page_facing_up:](https://rclone.org/s3/#pure-storage-flashblade)
- FTP [:page_facing_up:](https://rclone.org/ftp/)
- GoFile [:page_facing_up:](https://rclone.org/gofile/)
- Google Cloud Storage [:page_facing_up:](https://rclone.org/googlecloudstorage/)
- Google Drive [:page_facing_up:](https://rclone.org/drive/)
- Google Photos [:page_facing_up:](https://rclone.org/googlephotos/)
- HDFS (Hadoop Distributed Filesystem) [:page_facing_up:](https://rclone.org/hdfs/)
- Hetzner Object Storage [:page_facing_up:](https://rclone.org/s3/#hetzner)
- Hetzner Storage Box [:page_facing_up:](https://rclone.org/sftp/#hetzner-storage-box)
- HiDrive [:page_facing_up:](https://rclone.org/hidrive/)
- Hitachi Content Platform (HCP) [:page_facing_up:](https://rclone.org/s3/#hcp)
- HTTP [:page_facing_up:](https://rclone.org/http/)
- Huawei Cloud Object Storage Service(OBS) [:page_facing_up:](https://rclone.org/s3/#huawei-obs)
- Huawei Drive [:page_facing_up:](https://rclone.org/huaweidrive/)
- iCloud Drive [:page_facing_up:](https://rclone.org/iclouddrive/)
- ImageKit [:page_facing_up:](https://rclone.org/imagekit/)
- Internet Archive [:page_facing_up:](https://rclone.org/internetarchive/)
- Internxt [:page_facing_up:](https://rclone.org/internxt/)
- Jottacloud [:page_facing_up:](https://rclone.org/jottacloud/)
- IBM COS S3 [:page_facing_up:](https://rclone.org/s3/#ibm-cos-s3)
- Impossible Cloud [:page_facing_up:](https://rclone.org/s3/#impossible-cloud)
- Intercolo Object Storage [:page_facing_up:](https://rclone.org/s3/#intercolo)
- IONOS Cloud [:page_facing_up:](https://rclone.org/s3/#ionos)
- Koofr [:page_facing_up:](https://rclone.org/koofr/)
- Leviia Object Storage [:page_facing_up:](https://rclone.org/s3/#leviia)
- Liara Object Storage [:page_facing_up:](https://rclone.org/s3/#liara-object-storage)
- Linkbox [:page_facing_up:](https://rclone.org/linkbox)
- Linode Object Storage [:page_facing_up:](https://rclone.org/s3/#linode)
- Magalu Object Storage [:page_facing_up:](https://rclone.org/s3/#magalu)
- Mail.ru Cloud [:page_facing_up:](https://rclone.org/mailru/)
- Memset Memstore [:page_facing_up:](https://rclone.org/swift/)
- MEGA [:page_facing_up:](https://rclone.org/mega/)
- MEGA S4 Object Storage [:page_facing_up:](https://rclone.org/s3/#mega)
- Memory [:page_facing_up:](https://rclone.org/memory/)
- Microsoft Azure Blob Storage [:page_facing_up:](https://rclone.org/azureblob/)
- Microsoft Azure Files Storage [:page_facing_up:](https://rclone.org/azurefiles/)
- Microsoft OneDrive [:page_facing_up:](https://rclone.org/onedrive/)
- Minio [:page_facing_up:](https://rclone.org/s3/#minio)
- Nextcloud [:page_facing_up:](https://rclone.org/webdav/#nextcloud)
- Blomp Cloud Storage [:page_facing_up:](https://rclone.org/swift/)
- OpenDrive [:page_facing_up:](https://rclone.org/opendrive/)
- OpenStack Swift [:page_facing_up:](https://rclone.org/swift/)
- Oracle Cloud Storage [:page_facing_up:](https://rclone.org/swift/)
- Oracle Object Storage [:page_facing_up:](https://rclone.org/oracleobjectstorage/)
- Outscale [:page_facing_up:](https://rclone.org/s3/#outscale)
- OVHcloud Object Storage (Swift) [:page_facing_up:](https://rclone.org/swift/)
- OVHcloud Object Storage (S3-compatible) [:page_facing_up:](https://rclone.org/s3/#ovhcloud)
- ownCloud [:page_facing_up:](https://rclone.org/webdav/#owncloud)
- pCloud [:page_facing_up:](https://rclone.org/pcloud/)
- Petabox [:page_facing_up:](https://rclone.org/s3/#petabox)
- PikPak [:page_facing_up:](https://rclone.org/pikpak/)
- Pixeldrain [:page_facing_up:](https://rclone.org/pixeldrain/)
- premiumize.me [:page_facing_up:](https://rclone.org/premiumizeme/)
- put.io [:page_facing_up:](https://rclone.org/putio/)
- Proton Drive [:page_facing_up:](https://rclone.org/protondrive/)
- QingStor [:page_facing_up:](https://rclone.org/qingstor/)
- Qiniu Cloud Object Storage (Kodo) [:page_facing_up:](https://rclone.org/s3/#qiniu)
- Rabata Cloud Storage [:page_facing_up:](https://rclone.org/s3/#Rabata)
- Quatrix [:page_facing_up:](https://rclone.org/quatrix/)
- Rackspace Cloud Files [:page_facing_up:](https://rclone.org/swift/)
- RackCorp Object Storage [:page_facing_up:](https://rclone.org/s3/#RackCorp)
- RelAix.Storage [:page_facing_up:](https://rclone.org/s3/#relaix)
- rsync.net [:page_facing_up:](https://rclone.org/sftp/#rsync-net)
- Scaleway [:page_facing_up:](https://rclone.org/s3/#scaleway)
- Scality (RING / ARTESCA) [:page_facing_up:](https://rclone.org/s3/#scality)
- Seafile [:page_facing_up:](https://rclone.org/seafile/)
- Seagate Lyve Cloud [:page_facing_up:](https://rclone.org/s3/#lyve)
- SeaweedFS [:page_facing_up:](https://rclone.org/s3/#seaweedfs)
- Selectel Object Storage [:page_facing_up:](https://rclone.org/s3/#selectel)
- Servercore Object Storage [:page_facing_up:](https://rclone.org/s3/#servercore)
- SFTP [:page_facing_up:](https://rclone.org/sftp/)
- Shade [:page_facing_up:](https://rclone.org/shade/)
- SMB / CIFS [:page_facing_up:](https://rclone.org/smb/)
- Spectra Logic [:page_facing_up:](https://rclone.org/s3/#spectralogic)
- Storj [:page_facing_up:](https://rclone.org/storj/)
- SugarSync [:page_facing_up:](https://rclone.org/sugarsync/)
- Synology C2 Object Storage [:page_facing_up:](https://rclone.org/s3/#synology-c2)
- Tencent Cloud Object Storage (COS) [:page_facing_up:](https://rclone.org/s3/#tencent-cos)
- Uloz.to [:page_facing_up:](https://rclone.org/ulozto/)
- US3 Object Storage [:page_facing_up:](https://rclone.org/s3/#us3)
- Wasabi [:page_facing_up:](https://rclone.org/s3/#wasabi)
- WebDAV [:page_facing_up:](https://rclone.org/webdav/)
- Yandex Disk [:page_facing_up:](https://rclone.org/yandex/)
- Zadara Object Storage [:page_facing_up:](https://rclone.org/s3/#zadara)
- Zero Services (ZERO-Z3) [:page_facing_up:](https://rclone.org/s3/#zero-z3)
- Zoho WorkDrive [:page_facing_up:](https://rclone.org/zoho/)
- Zata.ai [:page_facing_up:](https://rclone.org/s3/#Zata)
- The local filesystem [:page_facing_up:](https://rclone.org/local/)

Please see [the full list of all storage providers and their features](https://rclone.org/overview/)

### Virtual storage providers

These backends adapt or modify other storage providers

- Alias: rename existing remotes [:page_facing_up:](https://rclone.org/alias/)
- Archive: read archive files [:page_facing_up:](https://rclone.org/archive/)
- Cache: cache remotes (DEPRECATED) [:page_facing_up:](https://rclone.org/cache/)
- Chunker: split large files [:page_facing_up:](https://rclone.org/chunker/)
- Combine: combine multiple remotes into a directory tree [:page_facing_up:](https://rclone.org/combine/)
- Compress: compress files [:page_facing_up:](https://rclone.org/compress/)
- Crypt: encrypt files [:page_facing_up:](https://rclone.org/crypt/)
- Hasher: hash files [:page_facing_up:](https://rclone.org/hasher/)
- Union: join multiple remotes to work together [:page_facing_up:](https://rclone.org/union/)

## Features

- MD5/SHA-1 hashes checked at all times for file integrity
- Timestamps preserved on files
- Partial syncs supported on a whole file basis
- [Copy](https://rclone.org/commands/rclone_copy/) mode to just copy new/changed
  files
- [Sync](https://rclone.org/commands/rclone_sync/) (one way) mode to make a directory
  identical
- [Bisync](https://rclone.org/bisync/) (two way) to keep two directories in sync
  bidirectionally
- [Check](https://rclone.org/commands/rclone_check/) mode to check for file hash
  equality
- Can sync to and from network, e.g. two different cloud accounts
- Optional large file chunking ([Chunker](https://rclone.org/chunker/))
- Optional transparent compression ([Compress](https://rclone.org/compress/))
- Optional encryption ([Crypt](https://rclone.org/crypt/))
- Optional FUSE mount ([rclone mount](https://rclone.org/commands/rclone_mount/))
- Multi-threaded downloads to local disk
- Can [serve](https://rclone.org/commands/rclone_serve/) local or remote files
  over HTTP/WebDAV/FTP/SFTP/DLNA

## Installation & documentation

Please see the [rclone website](https://rclone.org/) for:

- [Installation](https://rclone.org/install/)
- [Documentation & configuration](https://rclone.org/docs/)
- [Changelog](https://rclone.org/changelog/)
- [FAQ](https://rclone.org/faq/)
- [Storage providers](https://rclone.org/overview/)
- [Forum](https://forum.rclone.org/)
- ...and more

## Downloads

- <https://rclone.org/downloads/>

## License

This is free software under the terms of the MIT license (check the
[COPYING file](/COPYING) included in this package).

# Contained Degoo installation

Keep a vault installation in one private directory:

```text
~/.local/share/rclone-degoo/
  bin/rclone          dedicated executable
  config/auth.json    access and refresh tokens
  config/vault.json   independent encryption secret and vault settings
  config/rclone.conf  dedicated remote configuration
  cache/              persistent plaintext VFS cache and its lock/identity
  mount/              plaintext filesystem while mounted
  tools/              optional FUSE helper alias
  tmp/                temporary files used by the mount
  logs/               private runtime logs and cleanup inventory
  install.json        inventory and dedicated remote-folder identifiers
  mount.pid           last launched mount process ID
  mount.sh            foreground mount launcher
  status.sh           pending-upload status
  stop.sh             unmount launcher
  uninstall.py        local cleanup tool
```

The installation directory and its private subdirectories use mode `0700`;
secret files use `0600`. `install -d -m 700 DIR` creates a directory if needed
and sets these owner-only permissions. It does not install a package.

Use a dedicated `rclone.conf` through `--config`, rather than adding the vault
to the account's general rclone config. Tokens and the encryption secret have
separate roles. A generated encryption password must come from the operating
system's cryptographic random generator, not a memorable or reused account
password. Keep it out of command arguments and logs.

## Operation

The mount runs as the current account and needs no system service or shell
profile changes. A mountpoint is an ordinary empty directory when unmounted;
its file contents live in Degoo and in the local cache, not underneath that
directory. Standard mount flags can be passed to the launcher.

```sh
~/.local/share/rclone-degoo/mount.sh
~/.local/share/rclone-degoo/status.sh
~/.local/share/rclone-degoo/stop.sh
```

The mount command stays in the foreground. If launched in a separate session,
`mount.pid` records its process ID; it is not a boot-time service. Logs stay
inside the installation directory. The contained launcher sets `TMPDIR` to
its private `tmp/` directory so upload spools also stay inside the install.
On a host with only a compatible FUSE 2 helper, the tested `fusermount3` alias
also stays inside `tools/`; no system package needs to be replaced.

## Cleanup

Run the cleanup inventory before removing anything:

```sh
python3 ~/.local/share/rclone-degoo/uninstall.py \
  --root ~/.local/share/rclone-degoo
```

The source and tests for the cleanup tool are in
[contrib/degoo](../../contrib/degoo/README.md). Local removal is explicit. It
refuses pending uploads and mounted or unsafe paths, unmounts only the vault's
own mountpoint, and removes the isolated installation rather than changing
shared configuration. Preserve a verified copy of `config/vault.json` outside
the installation if you want to retain access to the encrypted remote files.

Removing local state does not revoke tokens at Degoo or delete the remote
folder. The dedicated test folder and any leftover placeholders must be
removed separately in Degoo if remote cleanup is desired. Automatic removal
of remote data is deliberately absent from the local uninstall operation.

The source checkout, shared Go toolchain/module/build caches, and this coding
agent's own runtime are development resources outside the contained vault.
They are not removed by vault uninstall. A prebuilt binary avoids creating
Go development caches when installing on another machine.

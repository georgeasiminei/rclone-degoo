# Immutable /mnt/degoo FUSE mount helper

This small Linux helper lets rclone mount an empty /mnt/degoo directory while
its immutable filesystem flag remains set. The directory must be caller-owned,
have owner read and execute permission, and have no group or world write access.
The privileged mount path is available only for that exact target, after the
helper checks it through an opened directory descriptor. The directory inode
is not changed.

For a privileged mount, the helper requires:

- /mnt to be a root-owned directory without group or world write access.
- /mnt/degoo to be owned by the caller's real UID, have owner read and execute
  permission, and have no group or world write permission.
- /mnt/degoo to be empty, immutable, and not already a FUSE mount.
- The `_FUSE_COMMFD` environment variable to identify a Unix socket whose peer
  UID matches the caller's real UID.

It pins /mnt/degoo with `openat(..., O_NOFOLLOW)` and mounts through
`/proc/self/fd/<dirfd>`. The kernel mount uses the caller's real UID and GID,
`MS_NODEV|MS_NOSUID`, and the fixed subtype `fuse.rclone-degoo`. The accepted
`fsname` and requested `subtype` values are labels only and are ignored. The
helper accepts `ro`, `default_permissions`, and an optional decimal `max_read`,
which it caps at 1 MiB. It rejects `allow_other`, `allow_root`, and every
other mount option.

Unmount requests and mounts outside this immutable case are delegated after
dropping the helper's effective and saved IDs to a fixed system helper path:
/usr/bin/fusermount3, /bin/fusermount3, /usr/bin/fusermount, then
/bin/fusermount. The helper compares executable device and inode before
delegation so it cannot execute itself recursively. The original argument
vector is passed unchanged. For a delegated mount it retains only the
`_FUSE_COMMFD` value, `PATH`, and `LANG` in the environment.

## Recommended setup through `degoo-rc`

When the install wizard reports that this helper is missing or invalid, use
the single administrator command it prints. For the default install root:

```sh
sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup
```

From a source checkout or extracted archive, run `sudo ./degoo-rc mount-setup`
from its root directory instead.

If GCC is missing, the wizard prints a compiler package command for the
detected distribution. Install that package, then rerun `mount-setup`. A valid
installed helper does not require GCC. The archive includes the
`scripts/python/mount_setup.py` command module and C helper source at
`immutable-mount/helper.c`. The contained install keeps both under its private
root; setup reads the source and builds in a temporary workspace as the
invoking user.

`mount-setup` builds as the account that invoked `sudo`, then installs the
root-owned helper at the fixed path. It preserves the immutable flag and does
not mount the vault or start its service as root. Use the printed command when
the install root differs from the default above.

After setup completes, continue as the normal user, without `sudo`. With a
systemd user manager:

```sh
degoo-rc service enable
degoo-rc info
```

Without a systemd user manager, use this administrator setup command so it
prints the foreground continuation:

```sh
sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup --foreground
```

From a source checkout or extracted archive, use
`sudo ./degoo-rc mount-setup --foreground` from its root directory. Then start
the foreground mount as the normal user in one terminal and inspect it from
another:

```sh
degoo-rc mount
```

```sh
degoo-rc info
```

If the service is already enabled, run `degoo-rc service restart` as the
normal user. The installer stops before asking about service startup when the
helper is a known blocker and prints these commands immediately.

## Advanced manual build and install

Build with GCC or a compatible compiler:

```sh
gcc -O2 -D_FORTIFY_SOURCE=2 -fstack-protector-strong -fPIE -pie \
  -Wl,-z,relro,-z,now -Wall -Wextra -Werror -std=c11 \
  -o /tmp/rclone-degoo-fusermount3 helper.c
```

Install only if the rclone launcher will put this fixed directory at the front
of PATH for the immutable mount operation:

```sh
sudo install -o root -g root -m 0755 -d /usr/local/libexec/rclone-degoo
sudo install -o root -g root -m 4755 /tmp/rclone-degoo-fusermount3 \
  /usr/local/libexec/rclone-degoo/fusermount3
```

The containing directory must remain root-owned and mode 0755; the helper
must remain root-owned and mode 4755. The build and test commands do not install
the helper or change the active `PATH`. Uninstalling
the helper means removing that one installed file and its now-empty directory.

The helper has no shell commands, configuration file, or user-specified
identity options. Keep the privileged mountpoint fixed at /mnt/degoo.

## Tests

From the repository root, run `contrib/degoo/immutable-mount/test.sh`. It
compiles the parser test binary and production helper in a temporary directory
with warnings treated as errors. The tests exercise valid
bazil/go-fuse option forms, escaped labels, the max_read cap, and rejection
of privileged identity overrides and unsupported access options. They do not
mount a filesystem or modify /mnt/degoo.

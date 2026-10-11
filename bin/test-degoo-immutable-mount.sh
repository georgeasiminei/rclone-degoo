#!/bin/bash
# Test generated encrypted data in a private mount namespace, preserving host mounts.
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
test_build_dir=$(mktemp -d)
trap 'rm -rf -- "$test_build_dir"' EXIT
go test -c -o "$test_build_dir/degoo.test" ./cmd/degoo
cp -- contrib/degoo/scripts/python/mount_prerequisites.py "$test_build_dir/mount_prerequisites.py"
cp -- contrib/degoo/scripts/python/mount_setup.py "$test_build_dir/mount_setup.py"
cp -- contrib/degoo/immutable-mount/helper.c "$test_build_dir/helper.c"
cp -- contrib/degoo/degoo-rc "$test_build_dir/degoo-rc"
cp -- contrib/degoo/scripts/python/cli.py "$test_build_dir/cli.py"
cp -- contrib/degoo/scripts/python/install.py "$test_build_dir/install.py"
test_uid=$(id -u)
test_gid=$(id -g)
if [[ "$test_uid" == 0 ]]; then
    echo 'Run this test as an ordinary user with sudo access.' >&2
    exit 1
fi
sudo -n unshare --mount --propagation private /bin/bash -s -- \
    "$test_build_dir" "$test_uid" "$test_gid" <<'NAMESPACE'
set -euo pipefail
build_dir=$1
client_uid=$2
client_gid=$3
fixture=$(mktemp -d /tmp/rclone-degoo-immutable.XXXXXX)
cleanup() {
    chattr -i -- "$fixture/mnt/degoo" 2>/dev/null || true
    rm -rf -- "$fixture"
}
trap cleanup EXIT
chmod 755 "$fixture"
mkdir -m 755 "$fixture/mnt" "$fixture/tools"
mkdir -m 700 "$fixture/mnt/degoo"
chown "$client_uid:$client_gid" "$fixture/mnt/degoo"
chattr +i -- "$fixture/mnt/degoo"
# Remove only the private namespace's clone of an existing mount; the host keeps it.
if awk '$5 == "/mnt/degoo" { found=1 } END { exit !found }' /proc/self/mountinfo; then
    umount -l --no-canonicalize -- /mnt/degoo
fi
mount --bind "$fixture/mnt" /mnt
mount -t tmpfs -o nodev,nosuid,mode=0755 tmpfs /usr/local
mkdir -m 755 -p /usr/local/libexec/rclone-degoo
# The helper must be on a suid-enabled mount; only this namespace sees it.
mount -t tmpfs -o nodev,mode=0755 tmpfs /usr/local/libexec/rclone-degoo
install -m 755 "$build_dir/degoo.test" "$fixture/degoo.test"
install -m 644 "$build_dir/mount_prerequisites.py" "$fixture/mount_prerequisites.py"
mkdir -m 755 -p "$fixture/scripts/python"
for module in cli install mount_setup mount_prerequisites; do
    install -m 644 "$build_dir/$module.py" "$fixture/scripts/python/$module.py"
done
install -m 755 "$build_dir/degoo-rc" "$fixture/degoo-rc"
mkdir -m 755 "$fixture/immutable-mount"
install -m 644 "$build_dir/helper.c" "$fixture/immutable-mount/helper.c"
env HOME=/root SUDO_UID="$client_uid" SUDO_GID="$client_gid" SUDO_USER="$(id -nu "$client_uid")" \
    "$fixture/degoo-rc" mount-setup
# Repeating explicit setup must also leave a working root-owned helper.
env HOME=/root SUDO_UID="$client_uid" SUDO_GID="$client_gid" SUDO_USER="$(id -nu "$client_uid")" \
    "$fixture/degoo-rc" mount-setup
# Repair an invalid leaf without following or modifying its symlink target.
printf '%s\n' 'generated symlink target: preserve' > "$fixture/keep.txt"
ln -sfn "$fixture/keep.txt" /usr/local/libexec/rclone-degoo/fusermount3
env HOME=/root SUDO_UID="$client_uid" SUDO_GID="$client_gid" SUDO_USER="$(id -nu "$client_uid")" \
    "$fixture/degoo-rc" mount-setup
test "$(cat "$fixture/keep.txt")" = 'generated symlink target: preserve'
test ! -L /usr/local/libexec/rclone-degoo/fusermount3
# Repair a regular leaf with incorrect ownership and writable permissions.
chown "$client_uid:$client_gid" /usr/local/libexec/rclone-degoo/fusermount3
chmod 0777 /usr/local/libexec/rclone-degoo/fusermount3
env HOME=/root SUDO_UID="$client_uid" SUDO_GID="$client_gid" SUDO_USER="$(id -nu "$client_uid")" \
    "$fixture/degoo-rc" mount-setup
test "$(stat -c '%u:%g:%a' /usr/local/libexec/rclone-degoo/fusermount3)" = '0:0:4755'
for helper in /usr/bin/fusermount3 /usr/bin/fusermount; do
    if [[ -x "$helper" ]]; then
        ln -s "$helper" "$fixture/tools/fusermount3"
        break
    fi
done
test -L "$fixture/tools/fusermount3"
sudo -u "#$client_uid" -g "#$client_gid" -- python3 - "$fixture" <<'PYTHON'
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import mount_prerequisites

issues = mount_prerequisites.check(Path(sys.argv[1]), Path("/mnt/degoo"))
assert not issues, issues
print("real immutable mountpoint and helper prerequisite check passed")
PYTHON
sudo -u "#$client_uid" -g "#$client_gid" -- env \
    RCLONE_CONFIG=/notfound PATH="$fixture/tools:/usr/bin:/bin" \
    "$fixture/degoo.test" -test.run '^TestVaultNativeImmutableMount$' \
    -degoo-immutable-fuse-test -test.timeout=60s -test.v
sudo -u "#$client_uid" -g "#$client_gid" -- python3 - <<'PYTHON'
import errno
import fcntl
import os
import socket
import struct
import subprocess

local, remote = socket.socketpair()
local.close()
result = subprocess.run(
    ["/usr/local/libexec/rclone-degoo/fusermount3", "-o", "subtype=rclone", "--", "/mnt/degoo"],
    env={"_FUSE_COMMFD": str(remote.fileno())},
    pass_fds=(remote.fileno(),), capture_output=True, timeout=10,
)
remote.close()
assert result.returncode != 0, "helper accepted a closed communication peer"
assert b"could not send the FUSE descriptor" in result.stderr, result.stderr.decode()
with open("/proc/self/mountinfo") as mounts:
    assert not any(line.split()[4] == "/mnt/degoo" for line in mounts), "failed helper left a mount"
fd = os.open("/mnt/degoo", os.O_RDONLY | os.O_DIRECTORY)
flags = bytearray(struct.calcsize("@L"))
request = (2 << 30) | (len(flags) << 16) | (ord("f") << 8) | 1
fcntl.ioctl(fd, request, flags, True)
os.close(fd)
assert struct.unpack_from("@L", flags)[0] & 0x10, "helper cleared the immutable flag"
try:
    os.open("/mnt/degoo/local-only", os.O_CREAT | os.O_WRONLY | os.O_EXCL, 0o600)
except OSError as error:
    assert error.errno == errno.EPERM, error
else:
    raise AssertionError("unmounted fixture allowed a local write")
print("immutable helper failed-send cleanup passed")
PYTHON
NAMESPACE

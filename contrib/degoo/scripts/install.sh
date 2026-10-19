#!/bin/sh
# Bootstrap the guided installer, including Python when it is missing.
set -eu
BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
export PYTHONDONTWRITEBYTECODE=1
printf '%s\n' 'Dependencies: Linux, Python 3.10+, FUSE (fusermount3 or compatible helper),' \
    'util-linux (mountpoint), and an rclone build with Degoo support.' \
    'Optional: systemd user services for automatic mounting. Go is needed only to build rclone.'
if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import sys; sys.exit(sys.version_info < (3, 10))'; then
    printf '%s' 'Install Python 3 using the system package manager? [Y/n]: '
    read -r answer || exit 130
    case "$answer" in ''|y|Y|yes|YES) ;; *) exit 1 ;; esac
    if [ "$(id -u)" = 0 ]; then
        PRIVILEGE=''
    elif command -v sudo >/dev/null 2>&1; then
        PRIVILEGE='sudo'
    else
        printf '%s\n' 'Install Python 3.10+ through your administrator, then run this installer again.' >&2
        exit 1
    fi
    if command -v apt-get >/dev/null 2>&1; then
        $PRIVILEGE apt-get update
        $PRIVILEGE apt-get install python3
    elif command -v dnf >/dev/null 2>&1; then
        $PRIVILEGE dnf install python3
    elif command -v pacman >/dev/null 2>&1; then
        $PRIVILEGE pacman -S --needed python
    elif command -v zypper >/dev/null 2>&1; then
        $PRIVILEGE zypper install python3
    else
        printf '%s\n' 'No supported package manager found. Install Python 3.10+ and retry.' >&2
        exit 1
    fi
fi
python3 -c 'import sys; sys.exit(sys.version_info < (3, 10))' || {
    printf '%s\n' 'Python 3.10 or newer is required.' >&2
    exit 1
}
if [ "$#" -eq 0 ]; then
    set -- --wizard
fi
exec python3 "$BASE/python/install.py" "$@"

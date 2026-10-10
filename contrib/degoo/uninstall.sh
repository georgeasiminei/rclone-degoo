#!/bin/sh
# Compatibility entrypoint; use degoo-rc uninstall.
set -eu
BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$BASE/scripts/uninstall.sh" "$@"

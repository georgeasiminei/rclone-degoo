#!/bin/sh
# Compatibility entrypoint; use degoo-rc install.
set -eu
BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$BASE/scripts/install.sh" "$@"

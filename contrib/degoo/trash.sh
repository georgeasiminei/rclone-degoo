#!/bin/sh
# Compatibility entrypoint; use degoo-rc trash.
set -eu
BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$BASE/scripts/trash.sh" "$@"

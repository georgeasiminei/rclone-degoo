#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

cc_bin=cc
common_flags="-O2 -D_FORTIFY_SOURCE=2 -fstack-protector-strong -fPIE -pie -Wl,-z,relro,-z,now -Wall -Wextra -Werror -std=c11"

# shellcheck disable=SC2086
"$cc_bin" $common_flags -DUNIT_TEST -o "$tmp_dir/parser-tests" "$script_dir/helper.c"
"$tmp_dir/parser-tests"

# Build the actual helper too so production-only code is checked by the compiler.
# shellcheck disable=SC2086
"$cc_bin" $common_flags -o "$tmp_dir/fusermount3" "$script_dir/helper.c"

if "$tmp_dir/fusermount3" -o allow_other -- /mnt/degoo >"$tmp_dir/output" 2>&1; then
	printf '%s\n' "helper unexpectedly accepted allow_other" >&2
	exit 1
fi
if ! grep -Fq "allow_other is not permitted" "$tmp_dir/output"; then
	cat "$tmp_dir/output" >&2
	printf '%s\n' "helper did not report the allow_other rejection" >&2
	exit 1
fi

printf '%s\n' "immutable FUSE helper checks passed"

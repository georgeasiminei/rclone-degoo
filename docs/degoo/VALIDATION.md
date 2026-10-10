# Degoo offline validation

Validation uses HTTP fixtures for Degoo and local storage for the encrypted
vault. No Degoo credentials or live test vault were available. These results
do not establish live provider compatibility; see
[API_EVIDENCE.md](API_EVIDENCE.md) and the live checklist in
[USAGE.md](USAGE.md).

## Checks

- `go build`: passed.
- `make quicktest BRANCH=degoo-vault`: passed with the built binary and FUSE
  helper on PATH.
- `RCLONE_CONFIG=/notfound go test -race -count=1 ./backend/degoo/... ./cmd/degoo`:
  passed.
- golangci-lint for `backend/degoo/...`, `backend/chunker`, and `cmd/degoo`:
  passed. Markdown lint and `git diff --check`: passed.
- The native Linux FUSE smoke test passed with real mount operations, encrypted
  local storage, overwrites, seeks, truncation, rename, deletion, and remount.
- The opt-in 21 GiB logical-file test passed a full encrypted round trip and
  SHA-256 comparison, plus reads above 20 GiB and across chunk boundaries.
  It generated data as a stream and checked encrypted chunk names, headers,
  count, and object-size limits.

The HTTP tests cover pagination beyond 1,000 entries, token refresh,
credential-error redaction, refusal of token-bearing redirects, rejection of
mutation replay after partial GraphQL success, strict range responses,
multipart upload,
registration ambiguity, duplicate names, root confinement, and preservation
of the prior object when replacement fails. Vault tests cover encrypted
contents/names/manifests, wrong secrets, corruption, missing chunks, exclusive
cache use, identity changes, and dirty-cache recovery after graceful restart
and abrupt subprocess exit. Chunker regressions cover failed manifest
publication and rejection of manifests whose transaction chunks are missing.

## Reproduce the opt-in checks

```sh
RCLONE_CONFIG=/notfound go test ./cmd/degoo \
  -run '^TestVaultNativeMount$' -degoo-fuse-test -count=1 -timeout2m
RCLONE_CONFIG=/notfound go test ./cmd/degoo \
  -run '^TestVaultLargeLogicalFile$' -degoo-large-test -count=1 -timeout15m
```

The native test requires `/dev/fuse` and a `fusermount3` helper. This machine
has FUSE 2.9.9's `/bin/fusermount`; validation used a temporary PATH-only
`fusermount3` symlink to that helper. No system packages were changed. The
large-file test needs more than 21 GiB of free temporary storage and took
about five minutes on this machine.

For the full offline suite, put the freshly built `rclone` on PATH. The
Makefile embeds the branch name in a semantic version; branches containing
`/` need a valid label override:

```sh
PATH="$PWD:$PATH" make quicktest BRANCH=degoo-vault
```

## Live gates

Folder-placeholder promotion and cleanup, account token lifetime, CDN ranges,
provider object limits, and failure recovery under actual Degoo consistency
must be checked in a disposable live vault before production use. The
implementation is experimental until those gates pass.

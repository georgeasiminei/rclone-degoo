# Degoo validation status — 2026-10-10

Offline implementation checks are recorded separately from live acceptance.
Live smoke checks passed against a dedicated disposable Degoo folder. A full
2 MiB logical file has been published and verified after a fresh-cache
remount, but the remaining live gates mean this does not establish general
provider compatibility or complete release acceptance.
See [API_EVIDENCE.md](API_EVIDENCE.md) and the remaining checklist in
[USAGE.md](USAGE.md).

## Offline checks

The completed offline checks use HTTP fixtures for Degoo and local storage for
the encrypted vault:

- `go build`: passed.
- The initial `make quicktest BRANCH=degoo-vault` passed with the built binary
  and FUSE helper on PATH. The follow-up
  `make quicktest BRANCH=degoo-live-setup` passed after the transient failure
  described below was rerun and the full suite was repeated.
- `RCLONE_CONFIG=/notfound go test -race -count=1 ./backend/degoo/... ./cmd/degoo`:
  passed after the final nested-directory change.
- golangci-lint for `backend/degoo/...`, `backend/chunker`, and `cmd/degoo`
  passed with zero issues, including the final Degoo changes. Markdown lint
  and `git diff --check` passed.
- `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest -v contrib.degoo.test_uninstall`:
  passed all 14 offline cleanup tests.
- The native Linux FUSE smoke test passed with real mount operations, encrypted
  local storage, overwrites, seeks, truncation, rename, deletion, and remount.
- The opt-in 21 GiB logical-file test passed a full encrypted round trip and
  SHA-256 comparison, plus reads above 20 GiB and across chunk boundaries.
  It generated data as a stream and checked encrypted chunk names, headers,
  count, and object-size limits.

An earlier full quicktest follow-up failed only in
`cmd/serve/s3/TestPutObjectFailureCached/minimal/Memory`: its 10-second wait did
not see `test-60/cached.txt` appear on the in-memory backing filesystem, and
the log showed the related VFS cache file missing. The exact subtest passed
when rerun alone, and all four `TestPutObjectFailureCached` subtests passed in
a scoped rerun. This appears transient under full-suite load; the cause is not
confirmed. The subsequent full-suite rerun passed.

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

The current live probes established these provider behaviors using the
dedicated test folder:

- Account authentication was discovered and exercised without recording the
  account password in the vault configuration.
- Folder creation under the account's device root succeeded for the dedicated
  test folder. Creating under account root ID `0` was rejected, so the backend
  must keep using the explicit dedicated folder parent.
- Live responses use RFC3339 timestamp strings and boolean mutation
  acknowledgements.
- A newly created encrypted-name directory placeholder was observed as a
  zero-size `Cat0`, not `Cat6`. The original placeholder remains in its
  parent; only its internal child marker moves into the returned `newCat2`
  folder.
- A generated 2 MiB logical file was fully published as encrypted chunks and
  an encrypted manifest; the pending-upload count reached zero. After
  unmounting, the old cache was moved aside, and the vault was remounted with
  a new empty private cache. A seek to 1 MiB + 123 followed by a 65,536-byte
  read succeeded, and a full readback SHA-256 matched the generated source.
  This verifies live Degoo CDN range reads and logical-file readback after
  remount. The redundant old cache was removed after this proof.
- Empty-file, overwrite, truncate, and rename operations passed readback after
  a fresh-cache remount. Generated empty and lifecycle-test files were then
  deleted, and a fresh mount's root listing verified their removal. This was
  a smoke check, not a full repeated lifecycle matrix.
- A rename probe exposed a routing edge case: the parent check for the final
  rename upload was true before the operation and false afterward when reusing
  the original same-name `Cat0` ghost. The backend now retires verified-empty
  ghosts automatically, and its tests pass; it does not generally delete
  non-empty ghosts.
- Live cleanup required recycling active items before hard deletion, then
  waiting for recycle visibility. The retired setup ghost and 23 failed
  generated uploads were recycled and successfully hard-deleted.

Nested directory creation, fresh-cache child-file readback, directory rename,
file deletion, and empty-directory removal passed. A final fresh-cache remount
confirmed an empty vault and zero pending uploads. Category 0 placeholder
handling requires a newly acknowledged create and no pre-existing entry.
The uninstall inventory passed against the installation; destructive uninstall
was tested with offline fixtures and was not run on the active installation.

Account-token lifetime, the live
provider limit above 20 GiB, pagination beyond 1,000 entries, and failure
recovery under Degoo's consistency model remain unverified. Keep all live
writes inside the dedicated test folder, and do not claim release acceptance
until the remaining gates pass.

The contained local installation and local-only uninstall procedure are
documented in [INSTALLATION.md](INSTALLATION.md) and
[contrib/degoo/README.md](../../contrib/degoo/README.md). Uninstall preserves
remote encrypted objects deliberately.

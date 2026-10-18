# Degoo scalable storage integrity review

Review date: 2026-10-18. Corrective delivery:
[PR #52](https://github.com/georgeasiminei/rclone-degoo/pull/52), targeting
`staging`; the user merges it.

## Scope and assessment

The review covers the cumulative changes from `7cf96f79f` (before PR #42) to
`e5012d922` (PR #52 before these corrections): 61 files, including 15 non-test
Go/Python source files. Independent reviews covered API/directory caches,
authority/allocation and protocol experiments, and encrypted trees/namespace.
Integration review covered object mutation identities, chunk/manifest/file I/O,
the updater and documentation. This is a differential review, not a complete
audit of upstream rclone or Degoo itself.

No active production data-loss regression was reproduced. One concrete
test-model integrity defect, an installed-settings coverage gap and an optional
namespace format-hardening issue are corrected below. The new encrypted
trees, allocation ledger and authority components remain inactive. Their
presence does not establish remote backup recoverability for the installed
mount or make the full enhancement ready for go-live.

## Cleanup must protect unresolved root references

The single-host experiment added in PR #51 allowed this schedule:

1. Confirm a data object and issue a delayed root create referencing it.
2. Reopen the local registry while the root-create result remains unknown.
3. Schedule an exact-ID delete of the referenced data. Only current/previous
   roots were checked, so the unresolved root intent did not protect it.
4. Apply and reconcile the delayed root create, making that root current.
5. Apply the delayed delete. The current root now references missing data and
   cannot be read.

This was reproduced with a failing generated-data regression. It is a defect
in a test-only model, not an active mounted deletion path. Cleanup now
protects references held by unresolved create intents. Reconciliation also
refuses roots referencing retired or delete-in-flight children, preserving
the intent and complete roots when unsafe overlap is already persisted.
Exact immutable IDs alone do not prove that an object is safe to delete.

Permanent regressions cover both protections across registry reopen and
retain the independent superseded-object late-delete case. The model still
does not supply provider consistency, production inventories, cleanup progress,
reader pins or remote publication authority.

## Installed-settings lifecycle and interrupted overwrites

The historical encrypted Degoo-mock lifecycle uses `transactions=rename` and
`hash_type=md5`. The installed mount uses `transactions=norename`,
`hash_type=sha1all` and `fail_hard=true`. The historical trace shares its setup
and lifecycle assertions with permanent installed-settings coverage. HTTP fault
handlers use the common mock fixture,
installed before its server starts; no copied review-only fixture remains.

The installed-settings cases cover create, full/range read, overwrite, rename
and delete. Failed replacement sources at six offsets and rejection of final
manifest publication preserve exact old-file readback after clearing
metadata caches. No-rename lookup may download its manifest; historical
rename-mode request counts must not become assertions for both modes.

These are generated localhost tests, not live Degoo, mounted VFS, power-loss
or delayed-provider-publication acceptance.

## Namespace format hardening

Namespace decoding rejected unknown fields and trailing values but accepted
duplicate JSON keys. Validly encrypted inode/link records with duplicate
properties were accepted; nested and escaped-equivalent property names also
are now rejected. Validation before typed decoding bounds records to 8 KiB
and nesting to 16 levels, and compares decoded keys with Unicode case folding
to match Go's field-name matching, including long-s aliases.
The normal writer emits unambiguous JSON. No authentication bypass or actual
data-loss scenario was demonstrated for this issue.

The seeded namespace state-machine test is a normally named regression:
120 operations compare lookup/list results and recheck retained snapshots.
Temporary review-only test files were removed after their useful cases were
integrated into permanent tests.

## Recovery and activation gates remain open

- Password-only recovery after Degoo login must discover all required KDF,
  vault identity and bootstrap state remotely and select the latest complete
  committed root. Recreating instances from a retained trusted root/key proves
  graph traversal, not this requirement.
- A local flock/checkpoint cannot establish remote freshness or rule out an
  old provider request completing after process death. Publication and delayed
  effect reconciliation require verified provider evidence.
- The inactive ledger retains at most 32 records and has no retirement or
  inventory-transfer path. Exhaustion refuses work; it is not a sustainable
  production allocator.
- Physical placement, inventories, reader pins, cleanup authority, mounted
  fault/rebuild acceptance, live large-directory/large-file checks and soak
  remain unfinished. Synthetic splitting above 20 GiB does not encrypt,
  upload or restore that volume through Degoo.

The [requirements](REQUIREMENTS.md), [decision](SCALABILITY_DECISION.md) and
[tracker](SCALABLE_STORAGE_PLAN.md) remain authoritative. These review fixes do
not change the go-live boundary. Pending bytes never uploaded cannot be
recovered after loss of their only local copy.

## Validation and operations

Final commands, failing-before/passing-after evidence and integrated results
are recorded in [VALIDATION.md](VALIDATION.md). Review-phase passing suites
preceded the new counterexample; they do not supersede its regression.

All fault data is generated locally. No live provider/authentication request,
installed-runtime change, reset, pending-write discard or user-data mutation
is part of these corrections. No secret or private vault inventory belongs in
the report. No temporary review logs or generated outputs are committed.

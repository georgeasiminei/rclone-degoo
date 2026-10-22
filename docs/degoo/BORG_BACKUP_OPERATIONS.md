# Borg backups through cloud mounts

Borg expects filesystem operations to preserve its repository transactions and
locks. A cached cloud mount acknowledges local writes before remote upload
completion. A successful Borg command alone therefore does not verify cloud
publication, completeness or integrity.

## Backup phases

Keep one writer for each repository and retain its destination lock throughout
backup and upload waiting. Before starting Borg, confirm the intended filesystem
is mounted and wait for prior uploads and any provider cooldown to finish.
After each operation, wait for writeback before starting the next one:

1. Inspect the existing repository, or explicitly initialize a new one.
2. Create the archive and drain uploads.
3. Prune the selected archive set and drain uploads.
4. Compact the repository and drain uploads before reporting success.

For the contained Degoo mount, use `degoo-rc wait --timeout 7200 --json`. This
command reads aggregate local statistics and the private provider cooldown
record. It does not send requests to Degoo. A timeout, missing runtime service,
unsafe cooldown record or unknown upload state is a failure, never an empty
queue. Writes from other applications are not excluded by this command.

A generic rclone mount can expose queue statistics through its private RC
service. For the older Google Drive mount without that service, the deployed
Immich backup script uses a read-only waiter over the configured VFS metadata
after Borg exits and closes every handle. The current cache base is
`/data/gdrive-rclone-cache`; the waiter validates the expected data and
metadata roots and requires two unchanged, clean metadata scans. It accepts
owner-owned regular metadata files without group/world write or special mode
bits (including rclone 1.60's `0644` files) inside owner-private directories.
Invalid JSON, unsafe paths, or files that change during scanning fail closed.
This does not establish that a concurrently running application has closed
its own writes.

The deployed Immich script drains uploads before and after each Borg phase. It
allows at most one retry for eligible `info` or `compact` transient I/O
failures after writeback is confirmed; it never retries `create` or `prune`,
and it does not run `borg break-lock` automatically.

## Failures and recovery

Degoo records a provider cooldown locally when an API request is rate limited.
During that hold, synchronous filesystem operations can return an I/O error,
while already saved local writes remain available for asynchronous retry. Wait
for the cooldown and pending uploads before retrying an eligible maintenance
operation. Bound total waiting and retry attempts, preserve the original error
when recovery fails, and never retry arbitrary repository or authentication
errors as if they were throttling.

Rclone's `--tpslimit` and `--tpslimit-burst` can also bound HTTP request bursts.
Apply them to the mount process, including after a service restart. A chosen
rate is an operational setting, not a verified Degoo quota, and does not
replace cooldown handling or writeback checks.

Do not automatically repeat `borg create` after a failure: its archive commit
may be ambiguous. Do not run unconditional `borg break-lock`; confirm no
repository user remains before any deliberate lock recovery. Failed `borg info`
on an existing repository is not permission to initialize another repository.

An interrupted compact may have written replacement segments before failing
to remove an obsolete one. Borg can replay transactions on subsequent access,
but that does not prove a cloud mount's complete remote state. Preserve the
repository and pending cache, establish healthy writeback, and check references
before resuming maintenance. Missing-segment compaction warnings alone do not
prove either corruption or safety.

## Validation

Run `borg check --archives-only --last 1 REPOSITORY` without repair mode to
check the latest archive's metadata and referenced chunk IDs. This skips full
segment checks and file-content verification. Restore a representative file to
a separate temporary directory and compare its checksum with the source.
Keep generated validation data separate from the real repository.

A full `borg check --verify-data` reads the file payloads and can be expensive
through a cloud mount. Schedule it deliberately. Queue completion and a small
restore test do not substitute for that check or password-only vault recovery.

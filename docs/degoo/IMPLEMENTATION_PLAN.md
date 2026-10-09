# Degoo private vault implementation plan

**Status:** Advisory handoff for a future Linux implementation agent.

This document describes a recommended route, not an API contract or a binding
architecture. [REQUIREMENTS.md](REQUIREMENTS.md) records the required behavior;
the summary below is for orientation. Later agents may choose different
libraries, architecture, or sequencing if they record the evidence and
rationale here before relying on the change.

## Firm human requirements

- Provide a writable Linux mount, through FUSE or equivalent, for a private
  Degoo file vault. Reads return plaintext locally; every file's contents must
  be encrypted before upload and decrypted locally on read.
- The supported user mount must require encryption and fail closed. It must
  never silently mount the raw Degoo backend or fall back to plaintext writes.
- Support logical files larger than 20 GiB by storing them as chunks.
- Keep the vault password in a simple private file configuration. Do not
  require a system keyring or a separate secret manager.
- Keep Degoo account login separate from the vault password. Renew account
  authentication automatically where supported and provide an explicit login
  path when renewal fails. Do not require a keyring for login or token refresh.
- Local VFS/cache contents may be plaintext on the user's trusted Linux
  machine. Local cache encryption, including LUKS, is not a requirement.
- Use only a dedicated test vault with generated test data for live writes.
  Do not read, alter, or delete existing personal Degoo data. Never print or
  dump sensitive file contents during debugging or validation.

## Recommended starting design

Implement a Go Degoo filesystem backend in this rclone fork. Reuse rclone's
Linux mount and VFS, then compose its existing chunker and crypt overlays:

`mount -> VFS -> chunker -> crypt -> Degoo backend`

This order makes crypt encrypt both data chunks and chunker metadata/sidecars
before they reach Degoo. Enable encrypted file and directory names by default
for the vault. Validate the resulting encoded names against Degoo limits.

Keep the Degoo transport backend raw for backend tests and internal use, but
make the supported vault mount entry point validate the complete encrypted
stack and reject raw mounts. A native rclone remote chain may be enough if the
entry point can enforce that rule. Add a small vault wrapper only if needed to
make that enforcement reliable. Never expose a plaintext fallback.

Start with one writer and a durable local pending-write cache. Define flush,
close, and fsync behavior explicitly: do not report remote durability until
all required chunks and encrypted metadata are confirmed. Preserve pending
writes across process restarts and make failed uploads visible and recoverable.
Do not promise multi-writer consistency or remote durability on close/fsync
until evidence supports those guarantees.

Use 1 GiB as an initial advisory chunk size. Prove it against observed Degoo
object limits, request behavior, and mount performance before fixing a default.
Keep chunk size configurable. Future agents may revise the layer order after
testing metadata exposure and encoded-name length, while retaining encrypted
sidecars and fail-closed mounts.

Store vault and auth configuration in private files and directories on Linux.
Owner-only permissions (for example, mode 0600 for files and 0700 for their
directory) are a recommended default. The config may contain the vault
password in plaintext; no separate key file is required. Rclone's config
obscuring is not encryption. Prefer an interactive Degoo password prompt and
private refresh-token storage without persisting the account password. Logout
must remove any stored account credentials while preserving the vault secret.

## Existing rclone capability and new work

Rclone already provides Linux FUSE mounting, VFS caching, the crypt and
chunker overlays, HTTP helpers, pacing, and backend test scaffolding. These
pieces do not provide Degoo connectivity or make an arbitrary raw backend
mount encrypted by themselves.

New work is the Degoo Go backend and its login, token refresh and private-file
storage, GraphQL/API mapping, object transfer, pagination, error handling, and
the vault mount entry point that enforces the encrypted chain. Use the Python
API in [cligoo](https://github.com/georgeasiminei/cligoo) as a behavior and
API-flow reference. Port the needed behavior to Go; do not invoke the Python
CLI as a subprocess.

Follow this fork's current default branch, `master`, from fetched
`origin/master`. This rclone fork has no `development` branch. Use a feature
branch based on the actual fetched default and target this fork unless a
separate instruction asks for upstream contribution. Do not assume or invent
an upstream PR target.

## API evidence to establish first

Treat these as known gaps to verify, not settled provider guarantees:

- The current Python client has no demonstrated ranged-download contract.
  Test the actual CDN response for `Range`, status 206, and a correct
  `Content-Range`; also test expired download URLs and token refresh.
- Upload has separate data-transfer and metadata-registration steps. Track
  their outcomes independently. Determine idempotency and orphan cleanup
  before retrying a transfer whose metadata registration failed.
- Listing and path resolution must paginate beyond the observed 1,000-item
  boundary. Test repeated pages, duplicate names, and stable path resolution.
- Degoo's provider checksum is a seeded SHA1 over the first 1 MiB, not an
  end-to-end integrity proof. The current upload verifier checks for a
  nonzero size and a download URL; it does not compare complete content.
- Classify expired credentials separately from transient failures. Refresh an
  expired token as appropriate; fail fast on a wrong password. Retry rate
  limits and network/server failures with bounded backoff, honoring provider
  retry guidance when present. Never log passwords, tokens, or file contents.

Use authenticated crypt reads and generated-file size/hash comparisons for
integrity evidence. Missing or invalid chunks must fail the whole read; never
return a truncated file as a successful result. Authenticated encryption can
detect content tampering, but it does not prevent remote deletion or rollback.

## Implementation sequence and exit gates

1. **Map the API and build an offline backend harness.** Read the current
   backend conventions and relevant `fs`, `lib/rest`, `fs/fshttp`, `lib/pacer`,
   `lib/dircache`, and VFS interfaces. Record observed Degoo request and
   response behavior from the Python reference. Add mocked tests for auth,
   pagination, path lookup, duplicate names, error classification, and
   transfer/registration partial failure. Do not start with writes to a
   personal account.

2. **Prove login and read-only listing.** Prompt for Degoo login interactively;
   persist refreshable tokens in the private file and refresh them without a
   keyring. Verify wrong-password behavior, token expiry, refresh, logout, and
   permissions. Exercise list and path lookup over more than 1,000 generated
   test entries in mocks, then do read-only live checks only against the
   dedicated test vault.

3. **Prove encrypted object round trips.** Wire the recommended stack and
   upload generated files only to the dedicated test vault. Confirm remote
   file data, names, chunk data, and chunk metadata are encrypted. Compare
   sizes and hashes after local readback; inspect no sensitive contents.
   Startup must fail if crypt or the required vault configuration is absent.

4. **Prove range reads and Linux mount behavior.** Validate CDN range behavior,
   crypt seeks, and chunk-boundary reads, including offsets above 20 GiB using
   generated data. Exercise stat, open, seek, read, and interrupted reads
   through the supported Linux mount. If range support is absent, document and
   resolve that limitation before claiming correct large-file mount behavior.

5. **Add writes and crash recovery.** In the dedicated test vault, exercise
   create, overwrite, truncate, rename, delete, and large-file writes. Interrupt
   transfers between data upload and metadata registration, then restart and
   prove that pending local writes recover, incomplete remote objects are not
   exposed as complete files, and orphans can be identified or cleaned up.
   Inject 429s, network timeouts, and 5xx responses; verify bounded retries
   and that wrong credentials do not loop.

6. **Run the backend suite and realistic vault validation.** Run unit tests,
   the backend's rclone integration suite, and practical Linux mount checks.
   Use a generated, disposable test tree with many entries, long names,
   duplicate names, empty files, and files larger than 20 GiB. Do not run
   destructive checks against existing personal vault contents.

The vault is ready for handoff only when the supported mount rejects raw or
unencrypted configurations; remote file bytes and chunk sidecars are encrypted;
round trips preserve generated contents; pagination is complete; missing
chunks fail hard; range and large-file behavior have evidence; token refresh
works from the private file without a keyring; and partial writes have a
documented recovery path. Report limits, failed probes, and any weaker
consistency guarantees alongside the evidence.

## Primary references

- [Rclone mount](https://rclone.org/commands/rclone_mount/)
- [Rclone crypt](https://rclone.org/crypt/)
- [Rclone chunker](https://rclone.org/chunker/)
- [Backend guide](https://rclone.org/contributing/#writing-a-new-backend)
- [cligoo API reference](https://github.com/georgeasiminei/cligoo)

# Degoo backend requirements handoff

Status: requirements recorded on 2026-10-09; implemented operational behavior
and current validation are tracked in
[IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) and [VALIDATION.md](VALIDATION.md).

This document gives a future implementation agent the user’s requirements for
a Degoo filesystem in the `rclone-degoo` fork. It separates those requirements
from design ideas proposed earlier in the project discussion.

## Repository context

- Fork: [rclone-degoo](https://github.com/georgeasiminei/rclone-degoo),
  based on upstream rclone `master` at the time of this handoff.
- Transport reference: [cligoo](https://github.com/georgeasiminei/cligoo).
- The reference repository may help explain observed Degoo transport behavior;
  it is not a promise that an endpoint or login flow remains available.
- Confirm current provider behavior before depending on historical API details
  or the reported object-size limit.

## User requirements

These points are the binding product requirements recorded from the user.

### Linux mount

- Linux is the primary platform for the initial implementation and acceptance.
- Provide a normal, writable Degoo filesystem mount using FUSE or an
  equivalent Linux mechanism. The rclone-based approach is recommended below.
- Mounted paths should behave like ordinary files for common operations:
  create, read, update, make directories, rename, and delete.
- The mounted view must transparently decrypt files for local applications.
- Preserve enough remote metadata to reopen files after unmounting, remounting,
  or restarting the client.
- Preserve pending local changes across restarts, provider outages, and failed
  authentication. Distinguish saved locally from uploaded remotely.
- Backup-only behavior is not the goal. Existing Degoo files are not part of
  the encrypted vault unless explicitly copied into it.
- Windows-specific support is outside the initial acceptance scope.

### Mountpoint safety and upload status

- New contained installs use `/mnt/degoo` as the default mountpoint. An upgrade
  keeps its recorded mountpoint unless the user explicitly changes it.
- The installer attempts to create `/mnt/degoo` as an owner-private directory.
  If it cannot, it prints `sudo mkdir`, `chown`, `chmod 700`, and `chattr +i`
  commands for the user to run. The mount refuses to cover ordinary local
  files in an unmounted mount directory. Users may keep the immutable flag set
  on `/mnt/degoo` while mounted and unmounted; the installer must not ask them
  to clear it. Standard `fusermount3` cannot mount there: the immutable flag
  prevents its required write-access check even when the directory is
  owner-writable. Retaining the flag requires a manually installed, root-owned,
  setuid helper at `/usr/local/libexec/rclone-degoo/fusermount3`. The helper is
  limited to an empty `/mnt/degoo` owned by the invoking user and not writable
  by group or other users. The default directory remains owner-private. The
  helper preserves the flag, mounts with that user's ID, and does not support
  `allow_other` or `allow_root`; rclone itself runs as the ordinary user.
  Mount launch must validate the helper's path, ownership, permissions, and
  setuid bit before selecting it, and must not install or modify privileged
  files automatically.
  When `/mnt/degoo` is immutable and no valid helper is available, show one
  administrator command for the current install root. For the default root,
  it builds as the invoking account and installs the fixed helper:

  ```sh
  sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup
  ```

  When no systemd user manager is available, use this administrator command
  so it prints the foreground continuation:

  ```sh
  sudo "$HOME/.local/share/rclone-degoo/degoo-rc" mount-setup --foreground
  ```

  State that this preserves the flag and does not mount or start a service as
  root. After it succeeds, print a clearly marked normal-user continuation.
  With systemd, run `degoo-rc service enable` and then `degoo-rc info`. Without
  systemd, run `degoo-rc mount` in the foreground and check `degoo-rc info`
  from another terminal. When the flag is absent, the mount guard protects the
  underlying directory through its retained directory handle.
- The privileged helper only accepts `/mnt/degoo`; it cannot mount the legacy
  in-install `mount/` path. An upgrade that needs to retain an immutable flag
  must run `degoo-rc install --mountpoint /mnt/degoo` to migrate the saved path
  while preserving the cache, after making the old underlying mount directory
  empty. This is a documented limitation of immutable legacy mounts.
- Changing an existing install from the legacy in-install `mount` directory to
  `/mnt/degoo` is refused while the old directory contains files. The
  installer and maintenance helpers accept only these two mountpoint choices.
  Uninstall removes the contained client but preserves the external
  `/mnt/degoo` directory.
- The generic VFS upload-status attribute is opt-in with
  `--vfs-upload-status` (default `false`). The contained Degoo mount enables it
  and exposes the read-only `user.rclone.upload-status` attribute with values
  `pending`, `uploading`, `uploaded`, or `unknown`.
- `uploaded` means VFS knows the current file version has a remote
  representation after writeback. It is not a new remote read or integrity
  audit. Files created directly under an unmounted mountpoint are ordinary
  local files: they are local-only and are never queued for cloud upload.
- `degoo-rc status` reports authentication failures before mount, service, and
  pending-upload health. An unhealthy report returns a nonzero exit status;
  `--json` provides machine-readable output. Per-file status is available for
  a selected file or directory. The private mount socket reports aggregate
  queued, uploading, and retrying counts only.
- Runtime status and Degoo diagnostics use the private `status.log` and
  `diagnostics.log` files. Command failures are kept concise, without Cobra
  usage blocks, and displayed log lines redact recognized credentials.

### Mounted benchmarks

- Provide `degoo-rc benchmark read` and `degoo-rc benchmark write` in place of
  shell benchmark aliases, with parameters for the selected file and test size
  or I/O settings.
- Delete the selected benchmark file after a successful test by default.
  Support `--no-delete`, especially on writes so the file remains for a read
  test. Preserve files when a benchmark fails or is interrupted.
- Include upload completion in write timing and explain the distinction
  between local persistence, VFS upload state, and independent cloud checks.

### Encryption and names

- Encrypt every user file before sending its content to Degoo.
- Decrypt file contents transparently when read through the local mount.
- Encrypt remote names and manifests. Plaintext user names and paths must not
  appear in the remote vault object names or manifest contents.
- Keep a mapping from local paths and logical files to their encrypted remote
  representation so normal mount operations can work.
- Use a standard authenticated encryption design and reject failed integrity
  checks. Record the chosen format and version for future compatibility.
- Do not print the vault secret, refresh token, or plaintext file contents in
  logs or error messages.

### Large logical files

- Support logical files larger than the reported 20 GiB provider object limit
  by splitting them into multiple remote objects.
- Keep each encrypted object below the provider limit, including headers,
  authentication tags, and other encryption or transport overhead.
- Store enough manifest information to reconstruct chunks in the right order.
  Manifests must be encrypted as required above.
- Publish a remote logical-file version as complete only after its referenced
  chunks are uploaded and validated. A complete local cached write may remain
  pending upload, with its remote completion state reported separately.
- Keep prior complete data readable when an update fails, where the selected
  storage model permits it.
- Reassemble and decrypt a logical file transparently through the mount.
- Treat 20 GiB as a reported limit to validate, not a live guarantee. Leave a
  safety margin and verify the current limit against Degoo before release.

### Vault secret and account login

- The vault encryption secret is independent of Degoo account authentication.
- A user may keep the vault password or secret in a local file protected by
  filesystem permissions on a secure, trusted machine.
- An OS keyring or external secret manager is not required.
- The local secret file is trusted local state; document that host compromise
  or access by another local user can expose it.
- Renew Degoo authentication automatically when the provider supports it.
  Provide an explicit login path when renewed authentication is unavailable.
- Never use the vault secret as an account credential or vice versa.
- Logout clears Degoo authentication state only. It must never clear the vault
  secret, vault configuration, or remote vault data.

The updated user requirement (2026-10-12) permits saving the Degoo account
password for automatic login when token renewal is rejected. Encrypt the saved
account credentials with the same key used for vault data and store the
ciphertext in the private vault configuration JSON. Keep token renewal as the
first choice, use the password as a fallback, and protect against repeated
login attempts during outages or provider rate limiting. Account login and
vault encryption remain separate purposes even though saved credentials use
the vault key for encryption. Logout must clear saved account credentials as
well as tokens, while preserving vault data, its secret, and pending writes.
Configuration backups must exclude account passwords, including their encrypted
form, and access/refresh tokens. Restoring a backup requires a fresh login.

### Configuration and data safety

- Explain remote-change cache staleness and distinguish listing freshness from
  local cache cleanup and data retention. Do not promise a provider visibility
  deadline that has not been established.
- Provide `degoo-rc sync [local path]`, including `degoo-rc sync .` from inside
  the mount, to refresh the selected remote directory view. Preserve pending
  local edits; this operation must not mirror or delete remote user data.
- Provide Linux installer and uninstaller wizards with step by step prompts.
  Suggest defaults for settings that can be inferred; account email and
  password have no invented defaults.
- List dependencies even when installed, and offer to install missing ones.
- Before each action that depends on local prerequisites, check them for the
  selected install, build, mount, or service path. Report which requirements
  are missing, present, misconfigured, or optional. For a detected and
  repairable problem, print safe, copy-pastable setup commands for the current
  distribution and system context, explain when they need administrator
  privileges, then give commands to verify the repair and retry the blocked
  action. Do not treat printed instructions as a successful repair.
- For setup requiring several privileged commands, provide a narrow dedicated
  setup helper whenever practical. Mark its one `sudo` invocation separately
  from the ordinary-user continuation. If a package prerequisite also needs
  administrator access, label that command too. Never ask the user to start
  rclone or its service as root. Do not pause for input before displaying the
  commands needed to clear a known blocker.
- Do not enable or start a service when a known required prerequisite means it
  cannot mount, and do not report setup as ready until the requested mount is
  confirmed. If setup is blocked, preserve completed configuration and pending
  local changes and print an actionable retry command.
- Never clear an immutable mountpoint flag automatically. The immutable-mount
  helper remains an explicit administrator setup: `degoo-rc mount-setup`, run
  with `sudo`, builds the helper as the calling user and installs it at the
  fixed root-owned path. The wizard may print this command but must not invoke
  it automatically. The command must not start the service or mount as root.
  When GCC is missing, print the detected distribution's compiler package
  command. Explain other mount blockers such as missing FUSE tools, unavailable
  or inaccessible `/dev/fuse`, and an unusable systemd user manager when
  relevant to the selected action.
- Show a visible password prompt even when logging to a file. Finish setup
  with the install and mount paths, actual mount state, helper commands, and
  log access. Make rejected authentication renewal visible with an actionable
  login-required message while preserving pending local changes.
- When the user chooses to start the mount during installation, return a
  nonzero exit status and an explicit error if startup fails or a connected
  mount cannot be confirmed. Preserve completed configuration for recovery
  and explain known startup blockers and retry commands.
- Offer to keep selected local state during uninstall. Preserve pending
  changes and the configuration needed to recover them; remote removal is
  separate from removing the local installation.
- Uninstall returns an error when any required local cleanup fails or cannot
  be verified. It reports success only after the installation directory and
  its owned command and service links are absent. Explain incomplete cleanup
  and the action needed to retry, including immutable directory flags.
- Keep scripts organized in their own directories, including a Python
  subdirectory. Select a dedicated vault under a discovered Degoo device
  folder by default when creation at account root ID `0` is unsupported.

The user-requested client update lifecycle (2026-10-13) is:

- Provide `degoo-rc update` to install the latest successful package from
  `staging`. Package delivery may move to GitHub releases in the future without
  changing the user's update workflow.
- Publish a small update-metadata artifact with the package checksum and a
  manifest binding the archive name, size, checksum, binary hash, managed
  payload hash, source commit, and dirty-source state. The updater checks the
  actual installed managed files against this metadata before downloading the
  large archive. A matching payload is a no-op without archive download or
  service, configuration, or cache changes. For a changed payload, download
  and verify the full archive against the metadata and workflow provenance.
  Missing metadata from older runs permits a visible full-archive fallback;
  present but invalid metadata must fail closed.
- Report update phases and elapsed time during long operations, repeating the
  elapsed-time message every ten seconds without estimated percentages.
- Before changing an active mount, refuse an update launched from inside that
  mount by the updater or its calling shell. For a remote update, perform this
  check before downloading the full archive when lightweight metadata is
  available. A no-op remains allowed from inside the mount. Explain that the
  child updater cannot change its parent shell's working directory; give the
  user the mount's parent path to `cd` to and let them retry in the same shell.
  Do not prompt, force an unmount, or use a lazy unmount.
- Keep enough installed build identity to recognize an unchanged build. An
  unchanged update is a no-op: do not rewrite configuration or change the
  service, cache, or install state, even when `--delete-cache` is supplied.
- Preserve configuration and pending local changes across updates. Keep the
  cache by default. Optional `--delete-cache` may remove it only when the
  pending-write state is proven clear; unknown or unverifiable state must block
  deletion.
- Prepare an active mount for update without forcibly terminating application
  handles. Briefly refuse new opens and file or directory changes while existing
  work completes. Wait up to 30 seconds by default, configurable to at most one
  hour; if applications keep handles open or local changes cannot be flushed,
  fail before stopping the mount or changing the installation.
- Make completed local changes durable before shutdown. Do not require cloud
  upload completion for an ordinary update: pending uploads remain locally
  available for retry. Offer an optional upload wait, off by default and capped
  at one day; if that wait expires, leave the runtime and installation intact.
- Respect the prior runtime state. Normally unmount before stopping the
  contained service, leave an inactive installation inactive, and restore only
  a service or mount that was active before the update. A busy unmount must not
  force-stop the service. Refuse automatic shutdown of a running mount that
  cannot prepare safely; require a normal unmount first. An already-current
  update is a no-op without preparation or runtime changes. Report a nonzero
  failure when replacement or restart fails. If the new build installed but its
  restart fails, retain the new build and configuration for recovery.

Configuration must provide a way to set these values or their equivalents:

- The remote root directory reserved for the encrypted vault.
- The local cache directory and the chunk target or maximum size.
- The vault password or a reference to its local file, and the local
  authentication-state path. Keeping the password directly in a private
  configuration file satisfies the simple-file requirement.
- Mount settings needed for the Linux mount.

- Keep filesystem mutations scoped to the configured vault root. Account
  authentication and metadata lookup needed to locate that root are separate.
- Use a dedicated, disposable encrypted vault for development and live tests.
- Do not point initial tests at a user’s existing Degoo files or modify them.
- Do not assume native Degoo objects are encrypted or readable by this backend.

## Security and metadata boundaries

Encryption must protect file contents, remote names, and manifest contents.
This handoff does not promise or request hiding all observable metadata.
Unless a later design and test demonstrate otherwise, Degoo or an observer may
learn object sizes, chunk counts, object counts, timing, or access patterns.
Directory topology may also be visible through the remote layout. Document
what the selected layout exposes; do not claim metadata-oblivious storage.

The stated local trust model allows a permission-protected secret file on a
trusted machine. The implementation should protect file permissions and avoid
accidental disclosure, but it does not need to add a keyring or secret manager.
Decrypted local cache files are allowed under this trust model; encrypting that
cache or requiring full-disk encryption is not a project requirement.

## Non-binding implementation suggestions

The following ideas came from earlier agent discussion. They are suggestions,
not user constraints or acceptance gates:

- Implement a Go rclone backend compiled into this fork.
- Layer chunking over rclone’s crypt remote.
- Start with 1 GiB chunks.
- Store Degoo refresh state in a separate file named `auth.json`.
- Begin with a single writer, with an optional read-only development phase.

A future agent may choose another design if it meets the user requirements.
Do not describe these suggestions as decisions already made by the user.

## Acceptance evidence

Use Linux and a dedicated disposable encrypted vault for live checks. Record
the Degoo behavior and observed object limit at the time of testing.

- Mount the vault and create, read, update, rename, and delete files and
  directories. Verify the files remain readable after unmount and remount.
- Inspect remote objects and metadata to confirm that file content, remote
  names, and manifest contents do not expose plaintext user data.
- Upload and read a logical file larger than 20 GiB. Confirm it uses multiple
  encrypted objects, each below the current provider cap after overhead.
- Interrupt a pending multi-chunk write, restart the client, and verify that
  recovery leaves a complete readable file or a clear failure with no partial
  file presented as complete.
- Repeat an interrupted update to an existing file and confirm that the mount
  never reports a mixed old and new file as a successful complete write.
- Try a wrong vault secret and a corrupted or missing chunk. Reads must fail
  clearly and must not silently return corrupted plaintext.
- Verify account-token renewal and the interactive re-login path when a token
  is rejected. Verify logout clears auth state but preserves the vault secret.
- Verify filesystem mutations stay under the configured vault root and that
  native Degoo files outside it remain untouched.
- Verify configured cache, chunk, secret, auth-state, and mount settings are
  honored.
- Verify a new install defaults to `/mnt/degoo`, existing upgrades retain the
  recorded mountpoint, migration refuses a nonempty old mount directory, and
  uninstall preserves the external directory. Confirm unmounted ordinary files
  are reported as local-only and cannot be mistaken for queued uploads.
- Verify the VFS status attribute is read-only, disabled by default for generic
  mounts, enabled by the Degoo mount, and reports the current version's state
  without claiming a fresh remote audit. Verify unhealthy `degoo-rc status`
  exits nonzero and reports authentication failures first.

Live endpoint availability, refresh behavior, and object limits can change.
Do not claim them as guaranteed from this requirements document alone.

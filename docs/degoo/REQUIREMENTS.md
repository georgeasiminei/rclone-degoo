# Degoo backend requirements handoff

Status: requirements recorded on 2026-10-09; implementation and offline
validation are tracked in [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md).

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

The recommended initial login flow prompts for the Degoo account password,
saves a refresh token in a private local file, and avoids saving the account
password. Future agents may adapt the account-login mechanism while preserving
the separate vault secret, automatic renewal, and simple local-file setup.

### Configuration and data safety

- Provide Linux installer and uninstaller wizards with step by step prompts.
  Suggest defaults for settings that can be inferred; account email and
  password have no invented defaults.
- List dependencies even when installed, and offer to install missing ones.
- Offer to keep selected local state during uninstall. Preserve pending
  changes and the configuration needed to recover them; remote removal is
  separate from removing the local installation.
- Keep scripts organized in their own directories, including a Python
  subdirectory. Select a dedicated vault under a discovered Degoo device
  folder by default when creation at account root ID `0` is unsupported.

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

Live endpoint availability, refresh behavior, and object limits can change.
Do not claim them as guaranteed from this requirements document alone.

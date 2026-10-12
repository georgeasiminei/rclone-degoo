# Session recovery proposal

The encrypted-password fallback is the first recovery layer. It runs during a
cloud request: try token renewal, then saved-password login if renewal requires
login. It keeps the FUSE mount and local cache intact. Explicit login updates
the same token file, allowing the next mount request to recover without a
service restart.

## Proposed next layer: recovery while the mount is idle

Add a small authentication supervisor inside the mount process. This is a
proposal, not behavior implemented by the password-fallback change.

- Check access with a read-only vault-folder metadata request every minute.
  Do not list user files, download content, or write probe objects.
- Share the mount's token client and login cooldown. Avoid independent
  processes racing to log in or restarting the mount to retry authentication.
- After a network failure or temporary provider failure, retry with jittered
  backoff from 30 seconds to 15 minutes. Respect provider `Retry-After`.
- When refresh requires login, use the encrypted account password. If that
  password is explicitly rejected, pause password attempts until a successful
  interactive login or changed credential. Temporary login failures remain
  eligible for delayed retries.
- On successful recovery, resume queued uploads and refresh the failed
  directory view. Preserve dirty local changes through the existing VFS
  mechanisms; do not delete or rebuild the cache.
- Log state transitions once: connected, retrying, login required, recovered.
  Expose the last successful provider check, failure category, and next retry
  time in `degoo-rc status --json` and `degoo-rc info`. Keep default status
  inspection local-only.

An optional desktop notification or external alert could report sustained
`login required` status. That would need a separately configured destination;
logging and status should work without it.

## Acceptance checks

Use generated files in a disposable vault. Expire the access token, reject
renewal, and verify automatic password login. Interrupt the network while an
upload is pending; verify that idle retries recover and the retained write
eventually uploads. Test bad credentials, HTTP 429 with `Retry-After`, and
restart during an outage. Confirm there are no duplicate password attempts,
cache loss, plaintext credential logs, or health reports that mistake a running
service for a successful provider connection.

# Degoo API evidence

Transport discovery used
[cligoo commit 444ae9c](https://github.com/georgeasiminei/cligoo/tree/444ae9c54690a0e77d0ba148cc9921477b75cdff)
on 2026-10-10. Its current `api.py`, `auth.py`, and `queries.py` are the request
reference. Some earlier sections in its API notes are superseded by later
corrections; normal uploads use a seeded checksum and millisecond timestamps.
These request shapes are reference evidence, not a live provider guarantee.

| Operation | Observed request contract | Failure boundary |
| --- | --- | --- |
| Login | REST `/login`, `GenerateToken`, `Username`, `Password` | Wrong credentials stop without repeating login |
| Renewal | REST `/access-token/v2`, `RefreshToken` | Rejected renewal requires explicit login |
| GraphQL | `operationName`, `query`, `variables.Token`, public `x-api-key` | HTTP success may still contain GraphQL errors |
| Listing | `GetFileChildren5`, string `ParentID`, `Limit=1000`, `Order=3`, opaque `NextToken` | Every page is needed for path resolution |
| Download lookup | `GetOverlay4`, `ID: {FileID: id}` | Listing URLs may be empty or expire |
| Upload authorization | `GetBucketWriteAuth4`, `StorageUploadInfos` | Authorization is separate from byte transfer |
| Byte transfer | Signed multipart POST to returned storage URL | Success does not publish filesystem metadata |
| Registration | `SetUploadFile3`, string size/checksum/time | Outcome can be ambiguous; mutation idempotency is unproven |
| Rename | `SetRenameFile`, `FileRenames: [{ID, NewName}]` | No distributed transaction across move and rename |
| Move | `SetMoveFile`, `FileIDs`, `NewParentID`, `Copy=false` | Account-root moves are not supported by the reference |
| Trash listing | `GetDeletedFiles`, `Limit=1000`, `Order=3`, opaque `NextToken`, `IsInRecycleBin` | The reference request has no parent/device filter and may be account-wide |
| Delete | `SetDeleteFile5`, `IDs: [{FileID}]`, `IsInRecycleBin` | Dedicated-vault confinement is essential |

The provider checksum is SHA1 over seed bytes and at most the first 1 MiB,
wrapped in the reference protobuf envelope. It does not prove whole-object
integrity. Empty files use a nonempty checksum; zero size plus empty checksum
is the reference folder creation sentinel.

Folder creation has a substantial evidence gap: the reference observes a
category 6 placeholder and delayed promotion to category 2 after a child is
added. Category 6 also describes ordinary documents. Empty directories and
promotion must be verified against a live disposable vault before release.

## Live evidence (2026-10-10)

Authenticated discovery and a dedicated generated-data test folder confirmed
these differences from the original offline fixtures:

- Metadata timestamps can be RFC3339 strings, including timezone offsets,
  rather than only decimal Unix milliseconds.
- `SetUploadFile3` can acknowledge success with JSON `true` instead of a
  string. Mutation decoding accepts successful booleans and legacy strings,
  and rejects false, missing, null, or unexpected acknowledgements.
- Newly acknowledged creates using encrypted names can initially produce
  category 0 zero-size placeholders. Only the app's own confirmed creation
  allows adding the marker; existing zero-size category 0 files are not adopted.
- Creating a child promoted a category 6 folder to category 2 with a different
  ID. The original entry became an empty category 0 ghost. The internal marker
  moved to the new folder and retained the name derived from the original ID.
- Creating a test folder under an existing device container worked; creation
  directly under account root was rejected.
- A surviving same-name category 0 placeholder caused file rename to change
  the uploaded file's parent back to that placeholder. Renaming the verified
  placeholder to a unique reserved name resolved the issue. Automatic
  retirement requires the moved marker and an empty placeholder.
- `IsInRecycleBin=true` hard deletion rejected an active replacement backup.
  Recycling with `false` removed it from the live parent listing. A subsequent
  hard deletion of the recycled setup-owned retired folder succeeded.
- A 2 MiB encrypted logical file published successfully. A fresh-cache mount
  passed an offset read through the CDN and a full plaintext SHA-256 match.

No credentials, signed URLs, account filenames, or folder identifiers are
recorded here. Current object caps and long-lived token behavior remain live
acceptance gates; see [VALIDATION.md](VALIDATION.md) for the tested scope.

## Registration diagnostics and recovery (2026-10-11)

Live generated-data probes encountered negative boolean acknowledgements for
registration and deletion. Successful encrypted 33-byte logical payload probes
received registration acknowledgements in about one second and appeared in
listings within another second. These samples do not establish why other
registration attempts fail or whether a negative response can have side
effects.

The API reports false, null, and omitted acknowledgements separately, without
logging response bodies. Each remains an error; a negative acknowledgement has
a typed marker for selecting the shorter confirmation window. The backend
confirms the exact staged object's metadata before publication. Offline tests
simulate a lost acknowledgement followed by delayed visibility, and verify
that reconciliation uses one byte transfer and one registration request.
They also cover an unconfirmed negative acknowledgement and cancellation of
blocked or throttled listing requests. These fixtures are failure simulations,
not claims about live provider rejection semantics.

## Rate limits and public web-client evidence (2026-10-13)

No public Degoo documentation reviewed for this change states a numeric
GraphQL/API request quota. The official [features](https://degoo.com/features),
[terms](https://degoo.com/terms), and [help site](https://help.degoo.com/)
pages reviewed do not publish one. This is a search result, not evidence that
no private or account-specific limit exists.

A live `GetFileChildren5` request returned HTTP 429 with
`Retry-After: 65043` at `2026-10-13 23:02:13 EEST`, which yields a requested
retry time of `2026-10-14 17:06:16 EEST`. The operation and response header are
recorded without account identifiers, item names, or paths. This confirms a
server-directed cooldown for that request; it does not identify whether the
scope is an account, endpoint, IP address, or broader service limit. No
additional authenticated request or quota/evasion probe was made as part of
this research. The already-running mount continued its automatic retries until
it was stopped at about `2026-10-13 23:21 EEST`.

The public web application redirects `cloud.degoo.com` to `app.degoo.com`.
Its unauthenticated public JavaScript bundles fetched on 2026-10-13 include
[the GraphQL client and operation service](https://app.degoo.com/4373-es2015.b7400e92aaedad458a8d.js),
[folder pagination](https://app.degoo.com/8314-es2015.c3cd367dfb34fc02dfc3.js),
[upload scheduling](https://app.degoo.com/1108-es2015.d04fa11b4f48d926cbe1.js),
and [file download handling](https://app.degoo.com/8954-es2015.f1adb5d544601252d903.js)
([additional viewer handling](https://app.degoo.com/9196-es2015.c92c99ac88d53ff15c32.js)).
These are public build artifacts; hashed filenames can change when Degoo
deploys a new web client.

In those bundles, the file manager requests folder pages of 50 items and loads
additional pages on demand. Each cache miss for `GetFileChildren5`,
`GetOverlay4`, `GetBucketWriteAuth4`, or `SetUploadFile3` is constructed as a
separate GraphQL POST. The client has an optional local result-cache argument,
used by two unrelated feed queries, but not by those four operations. The
GraphQL client code contains no visible `Retry-After` handling; it delegates
the HTTP call to an imported fetch helper (whose retry behavior was not
independently established here). The UI's folder/page state therefore offers
evidence for demand-driven listing, not a published request quota or proof of
provider-side batching.

The web client batches uploads differently: it collects upload authorization
inputs and registration file records into array-valued GraphQL operations.
That can reduce upload round trips, but does not establish that listing calls
can be combined or that replaying a registration batch is safe. The download
viewer separately maps HTTP 429 from a content URL to a “daily limit reached”
message. That is direct content-download handling, distinct from the observed
GraphQL listing 429; it does not establish that both responses share a quota
or explain the listing cooldown.

Two pinned community implementations provide additional, non-provider
reference evidence: [cligoo at
444ae9c54690a0e77d0ba148cc9921477b75cdff](https://github.com/georgeasiminei/cligoo/tree/444ae9c54690a0e77d0ba148cc9921477b75cdff)
uses `GetFileChildren5` pages up to 1,000 and has no request-result cache in
its API client; [Degoo at
3b646f54681dbd7290ea41a0f4499ea348a4cc43](https://github.com/bernd-wechner/Degoo/tree/3b646f54681dbd7290ea41a0f4499ea348a4cc43)
records the same observed page ceiling and reuses folder listings to satisfy
item lookups. These are implementation observations, not current Degoo
guarantees.

### Request reduction and local cache behavior (2026-10-14)

The backend stores a typed rate-limit deadline beside the private auth file
and fast-fails API calls while that deadline is active; an expired deadline no
longer blocks requests. On Linux, clients coordinate through a private
sidecar lock and re-read the deadline before token retrieval and HTTP calls.
Concurrent deadlines merge by keeping the latest expiry, so a shorter response
cannot reduce an existing cooldown. Lock waits are bounded and context-aware.
Backend metadata lookups use a shared ten-second total bound across pagination
and retries, honor caller cancellation, and stop retrying when Degoo supplies
`Retry-After`. The API's shared directory-listing request has a separate
30-second ceiling; an enclosing backend deadline can end it sooner. Background
VFS writeback schedules a pending upload at the later of its normal retry time
and the provider deadline, rather than sleeping in the request path. Download
URLs are reused for 30 seconds. Upload publication first checks the targeted
item metadata and skips a redundant canonical directory listing only when the
returned item matches the expected ID, name, parent, size, and file category.
If publication cannot be confirmed, the pending local write remains available
for VFS retry.

`GetFileChildren5` uses a five-second in-memory snapshot cache with at most 32
parent entries. A concurrent miss for one parent and cache generation shares
one paginated request; each waiter can cancel independently, while the shared
request retains the leader's context values and is bounded by the 30-second API
ceiling. `ListDirFresh` bypasses cached responses and cache storage, invalidates
only its parent, and can share a concurrent fresh request for that same parent
and generation. Known mutations invalidate the known parent or parents before
and after the attempt. A bounded item-to-parent hint table is learned from
listings and lookups; if the affected parent is unknown, invalidation clears
the in-memory cache conservatively. A mutation epoch prevents an older
in-flight lookup from replacing a newer parent hint after a move. Explicit
directory-cache flush clears all listing snapshots and parent hints. These
mechanisms reduce duplicate work and avoid unrelated parent invalidations when
the affected parents are known; they do not establish or change Degoo's
unpublished request quota.
The signed object-storage byte upload alone does not change folder metadata
and does not invalidate directory snapshots; metadata registration does.

The implementation also batches upload authorization requests for up to 30
files in one parent and registration mutations for up to 100 files, including
files in different parents. Authorization responses must contain exactly one
result per input; per-file rejections remain attached to that file. A
150-millisecond collection window gives concurrent file uploads a chance to
share requests; each shared operation is bounded to 30 seconds and one caller's
cancellation does not cancel the others. The registration mutation is not
automatically replayed because its outcome can be ambiguous. These batch sizes
follow array-valued public web-client request shapes and local fixtures; they
are not a provider-published limit or live guarantee. A local `degoo stats`
command reads aggregate counters through
the mount's private Unix socket. The counters include HTTP attempts by fixed
operation name and listing and cache activity, without request data or
identifiers, and the command makes no Degoo request.

The native encrypted vault also has a persistent cache for small raw provider
objects. It stores only ciphertext objects up to 256 KiB under the configured
cache's metadata area, with a 30-minute TTL, 64 MiB encoded and 8,192-entry
bounds, and
version keys derived from the object ID, size, modification time, and creation
time. A SHA-256 check and exact-size check reject damaged or stale entries.
Range opens can use a cached full object; upload readback verification bypasses
the cache. The cache index is refreshed on a one-minute sweep rather than
scanning all files on each small-object access. Unsafe cache paths fail closed,
while cache write failures do not turn a successful remote read into a
failure. Clearing the directory cache also clears this persistent cache. The
cache avoids repeat small-object downloads; it does not cache listing responses
or reduce GraphQL listing calls.

The request-reduction and small-object-cache changes passed the credential-free
full Go suite, scoped race, vet, and lint checks, plus a localhost FUSE fixture,
as recorded in [VALIDATION.md](VALIDATION.md). This is offline evidence and
must not be read as additional live-provider evidence. The earlier
localhost-cooldown smoke-test results remain limited to the scope recorded
there. A production mount later returned `EIO` for `ls` in 2.9 ms during the
recorded cooldown. A point-in-time process socket inventory showed no INET
sockets, but that is not packet-level proof across the cooldown. None of these
checks establishes live Degoo upload recovery.

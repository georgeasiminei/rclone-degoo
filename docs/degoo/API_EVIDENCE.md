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

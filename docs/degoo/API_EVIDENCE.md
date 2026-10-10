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
| Delete | `SetDeleteFile5`, `IDs: [{FileID}]`, `IsInRecycleBin` | Dedicated-vault confinement is essential |

The provider checksum is SHA1 over seed bytes and at most the first 1 MiB,
wrapped in the reference protobuf envelope. It does not prove whole-object
integrity. Empty files use a nonempty checksum; zero size plus empty checksum
is the reference folder creation sentinel.

Folder creation has a substantial evidence gap: the reference observes a
category 6 placeholder and delayed promotion to category 2 after a child is
added. Category 6 also describes ordinary documents. Empty directories and
promotion must be verified against a live disposable vault before release.

No live credentials were used during this implementation. CDN range support,
current object caps, token behavior, and metadata publication consistency
remain live acceptance gates.

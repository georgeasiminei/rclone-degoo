# Scalable storage decision — 2026-10-15

Status: **conditional architecture selected for further experiments; production
format and provider activation blocked**. Owner: coordinating Codex agent.
The user owns go-live. This record completes the offline comparison, but does
not close P1-LOOKUP or P1-DECISION's live evidence gate. Requirements are in
[REQUIREMENTS.md](REQUIREMENTS.md); measurements and executable experiments are
in [SCALABILITY_EVIDENCE.md](SCALABILITY_EVIDENCE.md).

Delivery: [#48](https://github.com/georgeasiminei/rclone-degoo/pull/48),
merged into `staging` on 2026-10-15 at `773b9f8c3`.

## Writer scope and robustness priority — 2026-10-15

The user selected the recommended first-release scope: one writable Linux host,
normal restart on that host, no automatic cross-host takeover. Robustness is
more important than performance. This resolves writer-scope preference; it does
not establish a verified publication protocol or close P1-DECISION.

| Scope | Upside | Cost / robustness condition |
| --- | --- | --- |
| One writable Linux host, selected | One owner and local recovery domain; no extra service | Writes wait when the host is unavailable; durable identity and a shared lock must cover different caches/config copies; no timed takeover |
| Explicit handoff between hosts | Move the writer deliberately | Must establish that the old writer cannot resume, preserve pending state and transfer authority safely; excluded initially |
| Concurrent writable hosts | Simultaneous access | Needs a verified authority for conditional publication, pins and destructive work; adds a service/recovery dependency if Degoo cannot supply it; excluded initially |

A non-expiring kernel lock can reject a second cooperating local process and
remains held while its owner is paused. [Linux flock semantics](https://man7.org/linux/man-pages/man2/flock.2.html)
apply to an open file description, not a PID lease or remote account. All
supported writers must share the same stable lock inode outside disposable
metadata caches; machine/account binding must reject another host or isolated
namespace that cannot establish that shared authority. The lock must not be
unlinked/recreated for takeover. A copied local config must not create a second
writer through a different cache path.

Crucially, an in-flight provider request can finish after the old process dies
and its local lock is released. A lock alone therefore does not make delayed
rename/delete safe. The publication design still needs durable intent before
side effects, immutable transaction/version identities, bounded reconciliation,
protection of complete roots/readers, and refusal of unsafe destructive work.
It must prove cache-loss/restart/bootstrap behavior without pretending a local
bookmark is a verified fresh remote read. Missing/corrupt safety state fails
closed; no automatic empty-vault initialization or pending-data discard.

The previous CAS/epoch comparison below describes the distributed alternative
and its missing provider primitive. Remote fencing is required before any
future multi-host or automatic-takeover support. For the selected initial scope,
P1-DECISION must instead verify a concrete single-host publication/recovery
protocol, including delayed provider effects after process death. Existing
mocked CAS is not its implementation. Keep format implementation/activation
blocked until that protocol and its provider assumptions have evidence.

## Decision and scenarios

Continue with an immutable encrypted sharded remote index, stable logical file
and directory IDs, bounded physical allocation, and an explicit exclusive-writer
publication boundary. A bounded local ID/page cache is an accelerator only.
Keep the current mount format until the selected publication/recovery protocol
is verified.
No new runtime, remote format identifier, cipher suite, or reset is introduced
by this decision. P3-FORMAT must specify those before implementation.

The decisive scenarios, in priority order, are:

1. A restart with no local index can open one known path by reading a trusted
   root and bounded pages; it must not enumerate all siblings or rebuild the
   entire vault first. The generated 100,000-record index needs four node
   reads, versus 100 listing pages in the existing layout.
2. A crash, timeout, authentication outage, or 429 during replacement leaves
   pending local bytes recoverable and exposes only a complete remote version
   or a clear failure. A lost acknowledgement must not trigger blind replay.
3. Renaming a populated directory changes its parent/name links and a bounded
   number of index paths, preserving descendant data IDs. Full enumeration
   remains O(N); rename must not move every encrypted descendant.
4. Every physical directory, metadata page, root/control record, transaction
   inventory page, and cleanup step has an explicit bound. Repeated failures
   must eventually stop allocation rather than accumulate unlimited orphans.

These are product/architecture constraints, not claims that the test models
satisfy the complete mounted contract. One local process owns the metadata
coordinator; no new service boundary is justified. Provider operations remain
behind the API client. Authentication, cooldown persistence, and VFS pending
write ownership retain their existing boundaries.

## Alternatives and tradeoffs

| Candidate | Measured evidence | Decision / cost |
| --- | --- | --- |
| Smaller listing pages | At 100,000 siblings, 100-item responses require 1,000 calls versus 100 with 1,000-item responses | Reject as scalability solution: cold lookup remains O(N); no live throttle benefit established |
| Bounded persistent IDs over current layout | Restart hit: one direct metadata request, no pages; retained fixture cache 16,348 bytes | Retain only as accelerator: misses/rebuild still enumerate siblings; old IDs do not prove the current name winner |
| Immutable encrypted radix pages | Cold reads 2/3/4 at 1,000/10,000/100,000 records; overwrite writes 2/3/4; full rebuild 126/273/4,369 reads | Conditional selection: bounds point metadata work and permits copy-on-write; adds object count, write fan-out, allocator and GC complexity |
| One encrypted whole-vault index | No performance experiment needed to establish unbounded rewrite/recovery unit | Reject: violates bounded metadata requirement |
| Append-only roots, choose maximum generation | Publication model demonstrates stale writers and stale visibility | Reject as sole authority: generation alone neither fences writers nor establishes latest root; root listings/history grow without cleanup |
| Two named root slots with ordinary rename/overwrite | No verified provider atomic replacement or conditional update | Reject for activation: duplicate names, delete/create gaps, stale reads and ambiguous acknowledgements remain unresolved |

All user names, keyed lookup records, file metadata, chunk descriptors,
transaction inventories and root contents must be encrypted. Degoo can still
observe physical object sizes/counts, request timing, traversal/access patterns,
and possibly the bucket/folder topology. This design does not hide those
observables or protect against deletion/rollback by the provider.

The radix model's simplified name/key/data-ID records omit full file metadata
and chunk descriptors. Its bytes are not an equivalent-metadata comparison
with API listings. Radix 16, 64-record leaves, and 128 KiB encrypted pages are
experiment parameters, not a committed wire format or provider quota. Bound
both records and encoded bytes, validate hash collisions, and fail explicitly
at maximum depth/record size. A fixed hash prefix alone does not cap bucket
occupancy under skew or repeated versions.

## Authority, root discovery, and writer fencing

Remote encrypted state is authoritative. A portable vault descriptor binds the
vault identity, format, KDF parameters, and bootstrap locator; it is retained
with the vault configuration, never solely in the disposable metadata cache.
It must locate a bounded remote control namespace without a full-vault scan.
Creation must use a discovered device folder; earlier live setup rejected
account root ID `0`. If a bootstrap provider ID can change, its bounded
rediscovery mechanism and
identity checks are a P3-FORMAT gate. A missing or corrupt descriptor/control
record fails; it must not silently initialize an empty vault.

The distributed alternative compares the expected root revision and writer
epoch, then atomically installs a bounded authenticated control value:
current root, previous root, transaction identity, allocation high-water mark,
and cleanup-inventory root/cursor. Large inventories live in bounded encrypted
pages referenced by this value. This is a **required abstract primitive, not
an implemented or verified Degoo mutation**. Encryption authenticates the vault,
format, object role, logical/version identity, and references; it does not
establish freshness against remote rollback.

A local cache lock excludes only processes sharing that lock. A timestamp,
random writer ID, increasing counter in an ordinary object, pre-write read,
or successful post-write read cannot exclude another machine or a paused old
writer. Any lease expiry/takeover must advance an epoch enforced by the same
authority that accepts root publication. Expected-root comparison rejects
concurrent updates; epoch checking also rejects an old writer after ownership
changes. Garbage collection and allocation use that authority too. Recovery
cannot rely on an operator's clock to decide an old writer has stopped.

No such provider-enforced conditional primitive appears in the implemented
GetOverlay4/SetUploadFile3/SetRenameFile/SetMoveFile contracts. Their Boolean
acknowledgements do not imply atomic pointer replacement, compare-and-swap,
unique create, idempotency, or linearizable reads. Absence from these client
contracts is not proof Degoo offers no suitable operation. Verify the actual
provider or revise the protocol before production implementation.

An external authoritative coordinator could supply fencing and root revision
storage, but would add a deployment and recovery dependency outside the current
simple client setup. It is an alternative requiring a separate architecture
review, not an implicitly approved new service. Restricting tests to one host
is useful experimental containment, not remote fencing evidence. Do not ship a
second writable client or advertise multi-machine safety on that basis.

## Publication and ambiguous outcomes

The coordinator serializes publication while allowing data preparation within
bounded quotas. A transaction has a durable local identity before network
side effects and a remotely discoverable, paged allocation inventory. It stages
immutable data/chunk-descriptor pages, verifies authenticated content and
expected identity/size, then writes encrypted index descendants before parents.
Only then may it attempt conditional root publication. File manifests use a
bounded descriptor tree, not one ever-growing chunk array.

| Boundary / failure | Required result |
| --- | --- |
| Before any staging or during chunk/index upload | Old root remains authoritative; local write pending; inventory retains every possible allocated slot |
| Root publication explicitly rejected on expected revision/epoch | Conflict; preserve local bytes; reconcile/rebase deliberately; no silent last-writer-wins |
| Root publication acknowledgement lost | Outcome unknown; retain old and proposed roots and allocations; stop further publication and destructive GC until reconciled |
| Readback shows old root but visibility may lag | Still unknown; neither proof of abort nor permission to recycle proposed objects |
| Authoritative readback proves the same transaction committed | Confirm the complete version; resume idempotently without a duplicate root publication |
| Restart while outcome unresolved or provider throttled | Recover local transaction; make no requests before persisted deadline; expose explicit pending/error status |

A positive metadata response is not complete content validation. A direct ID
response must match the expected ID, role, parent, size and authenticated
version references before use. Null, 401/403, 429, timeout, 5xx, malformed or
mismatched responses must not become authoritative absence. GetItem currently
returns API errors, with no fallback sibling listing; parent/name/size checks
belong to its caller. No provider visibility deadline has been established.

Conditional publication alone does not preserve old immutable objects if an
unfenced stale worker can delete or reuse slots. Unique non-reused transaction
allocation and serialized, authority-checked cleanup are required. A lost create
acknowledgement reserves the possible slot until bounded reconciliation proves
its outcome; duplicate create/name semantics must be measured. A single
unresolved transaction is sufficient to stop new metadata publication safely.

## Physical placement and stable directory identity

Separate index, data, transaction, and cleanup namespaces beneath a fixed
bounded vault root. Recursively allocate physical directories with a bounded
number of children; terminal directories contain a bounded number of objects.
Count child directories, provider placeholders/ghosts, historical versions,
markers and in-flight creates toward occupancy. Reserve explicit capacity for
split/recovery before allocation. If capacity or visibility is uncertain, stop
allocation. Choosing a random bucket or making each shard individually small
cannot establish a physical-directory bound.

A possible allocator uses persisted slot ordinals and fixed-radix directory
paths with a maximum depth. Its locator pages and cursors are encrypted and
bounded. Capacity exhaustion is an explicit error, not unbounded extra root
folders. The publication experiment tests a finite tree with files plus child
directories counted. Its fixed-depth folder count is deliberately measured;
adaptive placement and its tradeoffs are still a P3 decision. It does not test
real folder creation, ghost retirement,
provider-assigned IDs, or synchronization of allocator state. Final fanout,
byte limits, headroom, maximum depth, and capacity belong to P3-FORMAT and
P3-SHARDS, using live evidence rather than an invented provider threshold.

Logical directory identity is independent of its encrypted pathname and physical
placement. Index keys use directory identity plus a keyed child-name digest;
authenticated records include name, child identity, kind, parent and version.
Rename changes source/destination mappings and the moved directory's parent
record in one proposed root. Descendant directory/file identities and data
references remain unchanged. Cycle checks walk parent identities with bounded
depth and fail on malformed cycles. Directory replacement and open-handle
semantics must be specified and tested; the small model proves identity
preservation only, not full POSIX rename or mounted handle behavior.

## Retention and orphan cleanup

Protect current and previous complete roots, active transaction references,
and roots pinned by readers. Reader pins need a bounded retention budget;
publication backpressure must preserve live readers instead of deleting their
data to meet a quota. Authoritative pin registration, expiry/takeover and
allocation-intent publication before remote side effects remain explicit
P3 protocol gates, including behavior after client crash or network partition.
An append-only root history is not the recovery index.
Superseded objects become candidates only after reference checks against all
protected roots and authoritative publication reconciliation.

Each allocation must enter a bounded encrypted inventory before an uncertain
remote side effect. Inventory pages and traversal checkpoints allow bounded
work per cleanup step and restart without a complete account scan. A lost
registration response may hide a provider ID: reserve its known physical slot,
reconcile that bounded directory, and retain the intent until absence or the
matching object is established. If inventory is incomplete or stale, stop GC.
Do not infer unreachable status from a stale root or timeout.

Repeated failed attempts consume an explicit staging/orphan budget. At that
budget, stop new staging/publication until reconciliation and cleanup restore
headroom. This trades write availability during provider failure for bounded
storage and preservation. A per-attempt cap alone cannot bound accumulation.
Live object storage scales with user data; the bound concerns each container,
record and step plus the admitted temporary backlog, not a constant total vault
size. Retention parameters must leave room for a large logical file's paged
transaction inventory and the previous complete version.

Delete/recycle acknowledgement is not proof of reclaimed account bytes. Earlier
live evidence required recycling before hard deletion and waiting for visibility.
Do not empty account trash as a vault cleanup implementation. Confirm the
selected vault's exact generated IDs and two-stage delete semantics; retain
cleanup inventory through ambiguous deletion. Remote orphan GC remains
unimplemented. The PR #48 publication model exercises only known, fully visible
metadata-object deletion under a single actor; its allocation/cleanup has no
writer fencing. Its referenced file data and reader pins are not modeled.

## Provider evidence and remaining gates

On 2026-10-15, local read-only inspection found an installed client and persisted
cooldown expiring at **21:30:29 UTC**. No API, token renewal, auth login, runtime
installation, or reset was attempted. Expiry permits a later controlled
experiment; it is not permission to ignore a newer persisted deadline.
The Python reference [cligoo](https://github.com/georgeasiminei/cligoo) describes
an unofficial GraphQL client. It supplies endpoint evidence, not a provider
consistency or fencing guarantee.

| Assumption / gate | Current evidence | Required before activation |
| --- | --- | --- |
| Opaque ID direct reads and stability | Generated GetOverlay4 identity/error/cooldown contracts; earlier small live smoke only | Generated dedicated-vault create/direct read/restart/rename/move/overwrite/delete matrix; classify ID continuity and errors |
| Root/bootstrap persistence and freshness | Model locator/control only | Cold-cache rediscovery and authoritative readback across restart; never reinterpret missing as empty |
| Conditional publication and writer fencing | Test model offers explicit revision/epoch primitive | Verify a real provider primitive or revise/review protocol; reject stale writer after takeover, including stale delete/allocation work |
| Physical capacity / directory behavior | Finite placement model; earlier live placeholder caveat | Count real folders, ghosts and ambiguous creates against reserved headroom; fault every split/allocation boundary |
| Orphan discoverability and delete completion | Known-object model inventory; earlier recycle/hard-delete smoke | Restart with lost create/register/delete acknowledgements and incomplete visibility; no referenced-object removal |
| Encrypted object cap / large files | Reported 20 GiB; provisional 19 GiB safety setting; local large-file evidence | Live encrypted overhead and generated logical file above 20 GiB; bounded manifests and verified chunk hashes |

After the hold, first re-read persisted state and coordinate with any existing
mount so probe traffic cannot race its cooldown state. Use the shared client
cooldown guard; never issue a raw token-bearing request around it. Use a fresh
generated object in an isolated test namespace for mutations; do not mutate an
existing mounted object or enumerate private account contents for evidence.
Stop on the first new throttle/auth failure and record response class/counters
only. Repeat direct reads in a later client/session and test generated rename,
move, overwrite and delete separately. The current contracts do not provide a
CAS experiment to run blindly; discover a supported primitive first.

## Offline recovery refinement — PR #48 follow-up

Delivery: [PR #49](https://github.com/georgeasiminei/rclone-degoo/pull/49),
targeting `staging`;
merging and source-branch deletion remain with the user.

The follow-up [evidence](SCALABILITY_EVIDENCE.md#durable-allocation-and-adaptive-placement-follow-up)
extends the conditional architecture with disk-persisted intent-before-effect,
checkpoint-error refusal until reopen, fixed temporary-journal capacity,
non-reused allocation ordinals and bounded cleanup checkpoints. A modeled
complete graph protects current/previous/reader roots and their file-data IDs.
Its epoch authority rejects allocation/deletion as well as the earlier root
publication experiment. These remain local experiments with assumed authority
and complete/fresh graph inputs, without remote-only recovery or a real fence.

The adaptive comparison places the same 576 generated objects in 74 directory
records versus 8,650 for the fixed-depth model, while counting 148 reserved
slots and keeping maximum occupancy sixteen. This supports further lazy
allocation experiments, not adoption of these parameters. Persisting the whole
placement map is a fixture convenience; P3 must page allocator state and bound
encoded bytes. Retained orphans, unresolved deletes and exhausted lifetime
ordinals still stop work. Remote intent discovery, bootstrap/control reservation,
fresh reachability, safe ordinal/namespace evolution and cleanup progress
remain protocol gates. No new cipher, wire-format version or mount path is
selected by these experiments.

## Reviewed provider contracts — PR #48 follow-up

Read-only source review reconfirmed the pinned public cligoo reference at
[`444ae9c`](https://github.com/georgeasiminei/cligoo/tree/444ae9c54690a0e77d0ba148cc9921477b75cdff).
Its [query/mutation definitions](https://github.com/georgeasiminei/cligoo/blob/444ae9c54690a0e77d0ba148cc9921477b75cdff/src/cligoo/queries.py)
and [API methods](https://github.com/georgeasiminei/cligoo/blob/444ae9c54690a0e77d0ba148cc9921477b75cdff/src/cligoo/api.py)
provide no expected-root/revision, enforced epoch, unique-create or idempotency
argument in the reviewed operations. The Go client's corresponding requests in
[api.go](../../backend/degoo/api/api.go) likewise have no such field.
GetOverlay4 selects identity/location/size/URL/timestamps, without an
authoritative revision or ETag. Acknowledgements from registration, rename,
move and delete cannot establish the required conditional boundary from these
contracts alone.

This review does not discover the entire provider schema or prove that a
suitable operation cannot exist. No authenticated introspection or speculative
mutation was attempted. Publication/fencing is **unverified**, and direct-ID
continuity/deletion remains **unmeasured live**. The persisted shared-client
cooldown remains effective even though earlier browser controls sometimes
succeeded.

## Next live evidence protocol

This protocol is pending while any persisted provider/auth hold is active.
At 09:56 UTC on 2026-10-15, local state still held API access until 21:30:29 UTC.
Do not infer availability from another browser's success or from this timestamp
alone. Re-read deadlines immediately before a later experiment, use the shared
client guard, and coordinate with the existing mount without discarding pending
writes. Stop at the first new throttle/auth failure; keep private recovery
locators outside git. Do not bypass the guard with raw GraphQL or token renewal.

Use a new generated encrypted object beneath a dedicated test-vault parent.
Bound the experiment in advance by operations, elapsed time, and fixture bytes;
unresolved outcomes stop it rather than increase polling or replay mutations.
Record aggregate calls, response class, identity equality and content hashes,
never signed URLs, actual IDs, names, or tokens. A restart case uses a fresh
process/client with no listing snapshot or ID hints; it must share cooldown/auth
state rather than start with a clean deadline file. A private probe artifact
retains known generated IDs and outcomes. This tests direct addressability,
not proposed-format recovery from remote inventory or loss of all local state.
The production transaction/cleanup journal remains unimplemented.

| Stage | Evidence to retain | Failure/identity rule |
| --- | --- | --- |
| Create and direct read | Registration outcome, exact-ID comparison when available, parent/name/size and authenticated hash | SetUploadFile3 may acknowledge with a Boolean; resolve only in the isolated bounded parent, retaining uncertain outcomes without create replay |
| Fresh-client direct read | Same identity checks and zero sibling-list requests | Positive read proves that object only; it does not prove latest pathname winner |
| Rename | Separate direct reads of old/resulting IDs and bounded namespace reads of old/new names; payload checks | Classify preserved/replaced/rejected ID without assuming rename preserves identity |
| Move to second generated parent | Direct-ID parent checks, separate bounded namespace reads, both IDs if different; payload hash | Limit reconciliation to the two known bounded test directories |
| Overwrite | Old and new IDs plus version hashes; fresh-client path winner separately | Old-ID success may be historical data; never accept it as the current version merely because it exists |
| Recycle exact generated ID | Boolean acknowledgement, bounded parent visibility, direct-ID response class after restart | GetOverlay4 lacks recycle-state evidence; null/error alone proves neither recycling nor reclaimed capacity |
| Hard-delete that exact recycled ID | Two-stage outcome and bounded visibility checks retained in private probe artifact | Require scoped recycling evidence; otherwise defer. Never list/empty account trash; lost ack keeps outcome unresolved |

GetDeletedFiles is account-wide and has no reviewed vault filter. It must not
be used to fill the recycle-state evidence gap for this test. A successful
recycle acknowledgement and disappearance from a parent are observations;
if they cannot establish the required scoped prerequisite for hard deletion,
record the gate as unresolved rather than broaden the inventory.

The matrix measures observed continuity and deletion classes, not a consistency
or visibility guarantee. Compare successful namespace readback separately from
direct-ID readback. A provider error, stale view or unconfirmed absence cannot
justify reclaiming a slot or deleting a referenced version.

Publication verification is a separate gate. First identify a supported
operation with an atomic expected-root/revision condition and an authority-issued
epoch, or review a concrete alternative protocol. Then test two writers on
generated state: stale revision, stale epoch with current revision, fabricated
future epoch, pause/takeover/resume, lost acknowledgement before/after apply,
and stale allocation/deletion as well as root publication. A successful serial
rename/readback sequence cannot verify those properties. Do not add guessed
conditional GraphQL arguments or probe schema mutations blindly. Until a real
primitive/protocol meets this gate, P1-DECISION stays open and P3-FORMAT cannot
activate the conditional model as a provider-backed filesystem.

## Evolution, reversal, and reset consequences

The coordinating agent owns implementation evidence and plan updates. The
API worker owns direct-ID/error fixtures; the storage-model worker owns
publication/placement experiments. Follow-on P3 owners must specify the wire
format and test publication, sharding and rebuild before wiring a mount.
P2 cache/invalidation improvements independent of the new format may proceed.

Reverse the conditional selection if direct reads require unbounded scans,
bootstrap cannot be recovered, publication cannot be safely fenced, or physical
allocation/GC cannot preserve the bounds. Do not weaken writable-mount,
encryption or pending-write requirements to make an experiment pass.

The target format will be incompatible with the current chunker/crypt layout.
No migration or queue drain is required for disposable pre-live data. Prefer
an isolated empty test namespace first. Any later reset must announce the
specific vault/cache/pending data affected, downtime, and source paths to copy
again with rsync, while protecting credentials and all source data outside the
mount. No reset is necessary or performed for this decision. Go-live remains
explicit and requires the P4/P6 recovery and acceptance evidence.

## Single-host implementation boundary and delayed-effect refinement

Delivery: [#51](https://github.com/georgeasiminei/rclone-degoo/pull/51), targeting `staging`;
merging remains with the user.

The first writer scope selected in the independent PR #50 is one Linux host,
with normal same-host restart and robustness before performance. This follow-up
implements a local lifetime-lock component and an inactive bounded encrypted
metadata codec; neither is a complete publication mechanism or an activated
format. Generic directory cache bounds also ship independently of P3.

The new delayed-effect experiment avoids an assumed remote CAS. Required
control state outside the disposable cache contains a monotonic frontier,
current/previous locators and bounded exhaustive intents. Remote objects are
unique and immutable. Unknown effects retain reservations and block dependent
publication; exact-ID retirement never permits reuse. A delayed request can
therefore add or delete only its own immutable object under the model contract.
The encrypted root carries the child reference digest, so a derived-cache wipe
can rebuild the fixture from that root without a local child metadata record.

This is a candidate protocol, not a provider-backed decision closure. It relies
on immutable non-overwriting identity, authenticated exact reads, complete intent
persistence and exact deletion. Loss of required control state fails closed;
restoration of a stale valid registry is neither detected nor supported. The
registry cannot be treated as a disposable cache or a remotely fresh bookmark.
A production control-state recovery/backup policy remains required. The model's
two pending intents, sixteen lifetime slots and one-root/one-data graph do not
implement paged inventories, reusable allocation or active-reader lifetime pins.

P1-LOOKUP and P1-DECISION remain open. The page codec's role/vault/logical-ID/
generation fields and encrypted-byte digest authenticate identity, not freshness.
The sequencing plan now allows this independent inactive component before
provider activation. P3 still needs its payload schemas, allocator, publication,
shards/manifests, rebuild and mount wiring. No provider request, runtime update,
reset or reseed was performed. The persisted hold must be rechecked before any
later live direct-ID or deletion experiment.

# Degoo rate-limit investigation — 2026-10-15

This record preserves the live investigation that motivated the rate-limit
summary in [SCALABLE_STORAGE_PLAN.md](SCALABLE_STORAGE_PLAN.md). It supplements
the earlier [API evidence](API_EVIDENCE.md#rate-limits-and-public-web-client-evidence-2026-10-13)
without claiming a provider-published quota. The reviewed Degoo documentation
does not specify a numeric GraphQL quota or its scope; that remains a gap,
not proof that no limit exists.

The observations below are historical, read-only diagnostics. Recording them
does not authorize repeated probes during a provider cooldown or establish a
recovery procedure. No new Degoo requests were made to prepare this document.
Account identifiers, folder IDs, filenames, tokens, passwords, signed URLs,
raw listings, and the credential-bearing browser request are omitted.

## Incident and local state

The user reported that a roughly 350 GB test upload had stopped with about
30 GB remaining, and that `ls /mnt/degoo` failed. Local inspection found:

- The mount was connected and its user service was active.
- Native directory listing returned `Input/output error` (`EIO`).
- All 78 queued files were retrying; no upload was active. Dirty VFS metadata
  recorded 33,286,847,218 pending bytes, about 33.3 GB / 31.0 GiB.
- Cache-volume free space was about 431 GB and `out_of_space` was false.
  The inspection did not indicate local disk exhaustion.
- Mount-lifetime counters recorded 8,202 HTTP attempts, including 2,994
  `GetFileChildren5` listing attempts. These cumulative counters do not identify
  a quota threshold, requests per second, or historical cloud completeness.

The mount log recorded a `GetFileChildren5` rate-limit event at approximately
`2026-10-15 00:30:28 EEST` (`2026-10-14 21:30:28 UTC`). The persisted deadline
was `2026-10-15 21:30:29 UTC`, or `2026-10-16 00:30:29 EEST`: an approximately
24-hour hold. This was a new incident, distinct from the earlier deadline
ending on 2026-10-14 recorded in API_EVIDENCE.md.

The pending files remained in the existing local cache. The investigation did
not establish independent cloud integrity or successful upload recovery.
Current pre-live reset authorization is a separate policy; this historical
inspection neither reset test state nor requires future legacy migration.

## Browser success during the client's cooldown

**The user's Windows browser continued to list, download, and upload test files
while the Linux client's persisted cooldown was active.** Windows was on the
same LAN as Linux. Those download/upload successes were user-reported; the
agent independently verified read-only listing success, not browser writes
or downloads.

The supplied Windows request used
`https://production-appsync.degoo.com/graphql`, operation `GetFileChildren5`,
`Limit=100`, and `Order=3`. It used the same public routing API key as the
backend and carried no Cookie header. Its query declared and passed the
optional `AllParentIDs` argument and selected more item fields than the backend
query; the supplied variables did not set `AllParentIDs`.

Replaying that request's headers and semantic query **from Linux**, using a
fresh token for the same account, returned HTTP 200 and nine entries with no
GraphQL errors at `2026-10-15 03:30:54 UTC`. This was roughly 18 hours before
the client's saved deadline. Further controls succeeded with HTTP/1.1 and with
minimal native-client headers over HTTP/2. A successful replay therefore did
not require Windows, browser cookies, the full browser header set, or HTTP/2.
These controls do not prove that every request or directory was available.

The replay used Linux curl 7.81.0. The supplied browser headers identified
Chrome 155 on Windows and included Origin, Referer, language, priority, client
hints, and fetch-metadata headers. The minimal-header control retained JSON
content type, the public API key, and a rclone-like user agent. Authentication
stayed in the request body and was not printed or placed in curl arguments.

The important distinction is between a **local client-wide hold** and the
**provider's response to a particular request**. The reported Windows activity
and measured Linux listing success coexisted with the hold; neither established
that the affected upload-directory requests would succeed or that the provider
deadline could safely be discarded.

## Request comparisons and pagination

The following results used fresh same-account authentication. Folder labels
describe their role only; they are not remote names or identifiers. The
browser query was the wider Windows query above. The backend query selected
the smaller field set without `AllParentIDs`. All directory requests used
`Order=3`. Controls were observations, not an exhaustive factorial experiment.

| UTC on 2026-10-15 | Request or control | Result |
| --- | --- | --- |
| 03:30:54 | Browser query, small folder, limit 100, Linux curl / HTTP/2 | 200; nine entries; no GraphQL errors |
| 03:31:44 | Same folder/query/limit, forced HTTP/1.1 | 200; nine entries |
| 03:31:45 | Same folder/query/limit, minimal headers and rclone-like user agent | 200; nine entries |
| 03:32:22 | Backend query, small folder, limit 1,000 | 429 |
| 03:32:22 | Backend query, configured vault root, limit 1,000 | 200; four entries |
| 03:32:56 | Backend query, small folder, limit 100 | 200; nine entries |
| 03:32:56 | Browser query, same small folder, limit 1,000 | 429 |
| 03:33:19 | Backend query, upload directory, limit 100 | 200; 100 entries and a next-page cursor |
| 03:33:24 | Backend query, upload directory, limit 1,000 | 200; 1,000 entries and a next-page cursor |
| 03:33:50 | Complete-list attempt in upload directory, limit 1,000 | First page 200; second page 429 |
| 03:34:16 | Later backend-query attempt in upload directory, limit 100 | First page 429 |
| 03:34:25 | Browser-query small-folder control, limit 100 | 200; nine entries |
| 03:34:45 | Browser query, upload directory, limit 100 | 200; 100 entries and a next-page cursor |
| 03:35:21 | Later browser-query attempt in upload directory, limit 100 | First page 429 |

The same fresh-token comparison sequence contained both successes and failures.
Thus re-login alone was not a demonstrated remedy. The results also rule out
simple claims that all 1,000-item requests fail, that 100-item requests are
always safe, or that one query shape reliably fixes the affected directory.
The larger-page comparison is evidence of different outcomes in those samples,
not proof that the limit parameter alone caused the rejection.

A first page containing 1,000 items is not a complete listing beyond 1,000
entries. No successful complete enumeration of the affected directory was
established. Raw page size is also distinct from a physical-directory cap:
the backend follows cursors until it has all entries. Smaller pages can
increase total requests; the separately generated
[scalability measurements](SCALABILITY_EVIDENCE.md) quantify that tradeoff
without establishing a live quota.

## Initial 429 responses and apparent browser 504 errors

A fresh isolated Linux Chromium login to `https://app.degoo.com` succeeded,
but its live GraphQL requests produced empty HTTP 504 responses, including
`GetFileChildren5`, `GetUserInfo3`, and `GetCategoryContent`. Authentication
success alone did not establish listing availability.

The request and response comparisons captured these additional facts:

| UTC on 2026-10-15 | Diagnostic | HTTP status | Retry-After seconds | Response body |
| --- | --- | ---: | ---: | --- |
| 03:24:23 | Captured web listing replayed through Playwright's request context | 429 | 65,165 | `{}` |
| 03:25:05 | One POST with neither an account token nor an API key | 429 | 65,123 | `{}` |

Both delays yield approximately `2026-10-15 21:30:28 UTC`, within one second
of the persisted deadline. The unauthenticated request used only a minimal
GraphQL type query; it did not access an account listing. Its rejection does
not establish an IP-wide quota: later authenticated Linux requests succeeded
on the same endpoint during that interval.

The 429 responses had `Content-Type: application/json` and `Server: cloudflare`.
The latter identifies a component in the response path; it does not prove
Cloudflare generated the rejection rather than forwarding it from an origin.
No response identified the actual quota key or enforcement rule.

The web app had an Angular service worker registered from
`https://app.degoo.com/ngsw-worker-polyfill.js`. Its public `safeFetch` handler
catches failed network fetches and creates an empty HTTP 504 response. With
that worker bypassed for a diagnostic fetch, Chromium instead reported
`TypeError: Failed to fetch` and `MissingAllowOriginHeader` through its network
diagnostics. A browser CORS failure can concern a preflight; it does not expose
the original response status to page JavaScript.

Together these observations explain why the browser could display a synthetic
504 while direct request replay exposed a 429. An empty service-worker 504
must not automatically be classified as an origin gateway timeout. The direct
replay is the 429 evidence; the page's 504 alone is not.

Linux resolved IPv4 addresses for the app and API host, and an IPv6 connection
was unavailable in that environment. Windows routing and DNS were not measured.
Same-LAN placement alone does not prove identical egress. However, successful
Linux controls mean a blanket operating-system or Linux-network explanation
is insufficient.

## Confirmed failure mechanisms and unresolved provider cause

Three different layers must be kept separate:

1. **Provider-facing failure observed:** selected listing requests returned
   HTTP 429 with a long `Retry-After`, while other same-account requests
   succeeded. The precise reason for those rejections remains unknown.
2. **Mount-wide failure explained:** the backend converts a 429 into a shared,
   persisted global API cooldown. It checks that deadline before token work
   and HTTP calls. Consequently, subsequent mount calls can fail locally
   without sending a request, even when another client's request would
   succeed. Listing errors propagate as FUSE `EIO`, and VFS reschedules pending
   writes to the deadline. Later "rate limit is active" messages are not
   evidence of new remote 429 responses.
3. **Browser symptom explained:** a failed fetch/CORS path can be converted by
   the app's service worker into an empty 504, obscuring the underlying
   response from page code.

The existing metadata design contributes work: path and chunk discovery can
enumerate siblings; chunks and manifests share a parent; upload mutations
invalidate listing snapshots. At the incident, snapshots lasted five seconds
and were bounded by parent count. These are code-supported amplification
factors, not proof that a particular number of files or API calls triggered
Degoo's 429. The later offline improvements and candidate-index measurements
remain distinct from this live evidence.

An account-wide ban, exhausted storage, an expired password, a universal
100-entry threshold, a daily transfer-byte allowance, a required Windows user
agent, and a specific Cloudflare policy were not established. Neither a
page-size/header change nor successful upload recovery was demonstrated.
Preserve the provider deadline unless a separately justified change to its
scope is designed and validated; these observations are not an instruction
to clear the sidecar or bypass the hold.

## Investigation outcome and future records

At the end of the investigation, the mount remained running with all 78 files
pending. No mount restart, cache reset, installed authentication-file change,
remote mutation, or source-code change was made. Temporary diagnostic login
material and raw response bodies were removed; no private listing was printed.
The documentation task itself makes no claim about the later runtime state.

Future incident records should distinguish user reports, live observations,
code behavior, mocks, and hypotheses. Capture UTC timestamps, operation,
non-identifying folder role, query variant, requested page size, page ordinal,
HTTP versus GraphQL outcome, `Retry-After` and derived deadline, local versus
remote rejection, cache state, and whether enumeration actually completed.
Record comparison controls and unsuccessful follow-up attempts as well as
successes; do not retain credentials, raw request payloads, HAR exports, or
private account inventory.

Reproduce failure combinations in generated local fixtures. Further live
validation should follow the generated-vault and cooldown rules in
[SCALABLE_STORAGE_PLAN.md](SCALABLE_STORAGE_PLAN.md), rather than treating
the historical comparisons above as a procedure to repeat during a hold.

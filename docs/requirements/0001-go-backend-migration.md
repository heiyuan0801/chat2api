# 0001: Migrate the backend to Go while retaining Vue

Status: implementing

- Owner: backend migration
- Created: 2026-10-20
- Updated: 2026-10-21
- Baseline: `d58db042411f512449dd90e3b7a73de79ab38c46` / v3.2.3
- Related ADRs: [0005](../adr/0005-isolate-go-prototype-and-preserve-contracts.md), [0006](../adr/0006-go-control-plane-rehearsal.md), [0007](../adr/0007-go-public-execution-and-file-task-ownership.md), [0008](../adr/0008-go-remaining-domain-repositories-and-operations.md), [0009](../adr/0009-optional-shared-text-cache.md), [0010](../adr/0010-bind-image-recovery-to-origin-account.md)

## Problem and user outcome

The user approved a complete Go backend migration while retaining the existing
Vue frontend, public API contracts, and data compatibility. The first delivery
is an interface compatibility inventory and a runnable Go upstream-communication
prototype. A successful prototype is not a completed backend replacement.

## Current behavior and evidence

The baseline has 111 explicit HTTP route declarations (112 method/path pairs,
including the SPA GET/HEAD fallback). The current owners and persistence are
defined in [CONTEXT](../../CONTEXT.md), [the system map](../maps/system-map.md),
and accepted ADRs 0001–0004. Python remains the production implementation.
`curl_cffi` owns browser-like transport; `OpenAIBackendAPI` owns the upstream
conversation handshake. The clean clone does not contain the ignored Python
or frontend runtime tests. Source inspection alone does not establish live
upstream availability.

## Scope and delivery sequence

| Stage | Deliverable | Exit gate | Current state |
| --- | --- | --- | --- |
| 0 | Migration plan, complete HTTP inventory, compatibility rules | Every explicit method/path and source handler is accounted for | complete: 112 method/path/handler entries checked |
| 1 | Independent Go upstream prototype | Build, offline handshake/SSE/image fixtures, cancellation and transport checks; live evidence reported separately | implemented; offline checks and sampled live Turnstile/image flow verified; user acceptance pending |
| 2 | Account Repository, User Keys, settings, proxy routing and scheduling | SQLite/PostgreSQL copy-based differential tests; credential and concurrency semantics | implemented locally; isolated DB/HTTP/concurrency checks and sampled live PostgreSQL import/sync/key checks passed; broader live checks and user acceptance pending |
| 3 | Public protocol adapters and durable Image Tasks | JSON/error/SSE parity, owner isolation, restart and partial-success behavior | implemented locally; offline differential/persistence/race/build and Vue Studio checks passed; sampled live text/chat-image/generation/edit and Redis replay verified; follow-up flare/reference-SSE repairs live-verified in the live guide; user acceptance pending |
| 4 | Management, imports, assets, editable files, metrics and backup | All remaining inventory rows and existing Vue workflows verified | implemented locally; complete 112-route rehearsal, offline domain/browser verification and measured optimizations; live/user acceptance pending |
| 5 | Packaging, managed updates and cutover | Clean-source build, PostgreSQL/Redis container rehearsal, backup/restore and rollback; explicit production authorization | local container, native bundles and isolated PG18/Redis recovery/rollback verified; remote CI and target-host cutover tracked separately |

Stages 0–2 established the inventory, upstream client and control plane. The
current task completes stage 4 and local Go packaging/update rehearsal. Production
cutover remains a separate stage-5 gate.

### Current work: release readiness before canary

The user authorized completing the three release-readiness gaps: first update
this document, then wire Go Docker/Compose/CI, verify clean builds and rehearse
backup, restore and rollback with PostgreSQL and Redis. Preserve Vue source,
the 112 baseline API contracts and existing SQL/file ownership. This authorizes
local image builds and isolated containers; no remote publication, tag, image
push or production traffic change is included.

1. Add an explicit Go container and standalone Compose deployment, keeping the
   existing Python deployment available as the rollback entry point. Use one Go
   worker owner per task/asset directory; Redis remains disposable text cache.
2. Wire clean-checkout CI verification and tag-only release jobs for Go native
   bundles and a separate Go image. Never release on PR or workflow dispatch.
   Do not depend on ignored local test sources or host-built frontend output.
3. Add repeatable offline schema initialization and operational backup/restore
   commands. Stop the sole writer before capturing PostgreSQL plus file state;
   restore into an empty isolated target and verify SQL identities and file
   hashes before making it eligible to serve. Never copy running PG files.
4. Build from a complete source-only candidate without local fixtures, compile
   Go targets, build unchanged Vue, start the actual container with PostgreSQL
   and Redis, verify auth/models/management/static/error/SSE contracts, then
   rehearse backup, independent restore, version rollback and rejected restore.
5. Run focused and full local regression. Record local evidence separately from
   actual GitHub CI, publication and target-host acceptance. Prepare the canary
   runbook only after the local gates pass; do not claim production readiness
   from synthetic upstream traffic or previous image samples alone.

Release verification is pending at the start of this work. Production host,
domain, proxy path and traffic percentage are not yet selected. The resulting
runbook must state the remaining target-host gate without inventing a deployment.

The user subsequently explicitly authorized pushing the completed source to
`heiyuan0801/chat2api` and building it on GitHub. The target repository was empty
when inspected. This authorization includes a verification-only `main`/`codex/**`
push workflow as a task-specific exception to the repository's older tag-only
push-trigger rule. Release, tag and registry publication remain restricted to
explicitly authorized version-tag pushes. CI must expose downloadable native
bundles and the verified container without publishing a production image.

The source-only snapshot built unchanged Vue and all four native targets. An
actual Linux arm64 container used PostgreSQL 18.6 and Redis 7: authenticated
settings/models/static responses, synthetic Redis SSE replay, account/key
mutations, quiesced backup, independent restore, stable IDs/Call Record counts
and file hashes passed. The original Python Repository read restored PG18 data,
including unknown large integers. A deliberately failing image rolled back to
the captured image. Corrupt/link archives, nonempty targets and incomplete
restore startup were rejected. No real upstream generation was submitted for
these packaging checks. The final source-control/remote CI result is reported
separately; [the runbook](../runbooks/go-release.md) owns repeatable operations.

### Current verification: PostgreSQL, Redis and authorized live accounts

The current authorized repair binds read-only image recovery to the originating
Upstream Account. Before implementation: add a credential-free internal stable
account reference to existing task JSON, carry it through timeout/reload/resume,
and acquire only that account through the existing Account Pool and egress lease.
Quota exhaustion and image-tool cooldown do not require another generation and
must not prevent authenticated read-only recovery; disabled/deleted/unusable
accounts fail without substitution. Old tasks may recover an unambiguous binding
from the same User Key's Call Records for that conversation, otherwise fail
closed. No account-wide probing, token persistence or new durable repository.

Verify new/legacy tasks, restart, credential rotation, missing/conflicting origin,
owner isolation, concurrent resumes and account/egress limits. Rehearse with the
authorized accounts on isolated PostgreSQL/Redis: shorten only the private test
poll budget, observe an actual timeout, resume via the task HTTP route after a
process restart, audit the actual originating-account fingerprint and generation
POST count, and decode returned images. Start with two new benign generations;
allow at most four if upstream failure prevents observing successful recovery.
The user's approximate free allowance of 25 is a budget hint, not a replacement
for remote quota evidence or a new hard-coded allowance. Preserve Vue, public
task/API projections and compatible SQL/file ownership under ADR 0010.

Origin binding is now implemented in the existing task/execution/pool owners.
Two live tasks timed out under the isolated five-second budget, survived a real
process restart and returned decoded images via the originating accounts in
8.571/8.639 seconds. Transport audit recorded exactly two initial generation
POSTs and zero after restart. Legacy evidence recovery and fail-closed missing
proof were also exercised without new generation. The live guide distinguishes
new images from replayed real-conversation and synthetic receipt fixtures.
Final Go 1.25 SQLite/PostgreSQL/Redis race regression passed 207 top-level and
871 leaf cases, with no failures/skips; targeted recovery race cases passed
three repeats. Vet, route inventory and CGO-disabled cross-builds passed.
User acceptance and production cutover are still separate pending gates.


The current performance follow-up measures the successful 132.910-second chat
reference edit before changing code. Its stored timings show 109.266 seconds
in result discovery and 10.086 seconds in download; upstream tool timestamps
account for roughly 97–104 seconds of generation. Add credential-free poll
subtimings, prioritize authoritative conversation results over diagnostic task
probes, and retain settling, failure precedence, cancellation and read-only
resume. Compare identical ready/pending/error fixtures before and after. Then
run isolated PostgreSQL/Redis live rounds at two and four simultaneous calls,
with at most twelve new image executions, account/egress limits preserved,
actual decoded outputs, latency distributions and upstream failures recorded.
Do not attribute vendor timing changes or small-sample percentiles to Go CPU
improvements. Vue, API envelopes and persistence ownership remain unchanged.

The measured follow-up is locally implemented and verified: the same two
accounts returned chat edits in 70.350/81.275 s before and 47.753/57.673 s after;
the four-account mixed round delivered three images and one 133.752-second
poll timeout. Read-only inspection later confirmed the latter's delayed upstream
tool error, without another generation. These eight calls produced seven decoded
PNGs and do not establish sustained capacity or all-account availability.
Polling submetrics, deterministic wait removal, full Go 1.25 race regression
(200 top-level/858 leaf cases), vet, inventory and cross-builds passed. See the
[live guide](../go-live-verification.md) for attribution and origin-bound
recovery verification. No Vue/API/schema change or production cutover.


The follow-up repair targets the two failed live samples. Read-only conversation
inspection found completed tool messages with `metadata.is_error=true`; these
must remain upstream tool failures instead of being reclassified as user text
review or waiting for another image. A poll's own timeout must retain
`image_poll_timeout` and the known conversation rather than become request
cancellation. Verify these cases with captured-shape fixtures, then compare
the same account on the unchanged `auto` aliases and reference-image SSE route.
Use only bounded new generations and read-only polling for existing work;
preserve all HTTP/SSE envelopes, credential isolation and persistence owners.
Any account scheduling change requires execution evidence and remains within
the Account Pool owner, without persistent capability guesses.

Same-account baseline execution returned a decoded-result candidate for
`gpt-image-2.5-flare` before any protocol change, while previous failed `auto`
accounts had structured tool errors. Add bounded, process-local ten-minute
success preference / terminal tool-failure cooldown per stable account and actual
image route. Aliases sharing `auto` share evidence; `gpt-5-3` and Codex remain
separate. Refresh cannot erase evidence, and an older concurrent success cannot
clear a later failure. Preserve concurrency limits, unknown-account discovery,
existing attempt limits and cancellation. A known-conversation poll timeout
returns resumable failure rather than submitting generation on another account.

The user approved an optional shared text cache in Redis (ADR 0009). Before
implementation, the current-task sequence is: add the cache adapter without
changing domain persistence; verify cross-process replay, expiry, bounded
capacity and outage behavior; start isolated PostgreSQL/Redis; import the
authorized token file through HTTP; exercise models, text/chat images, image
generation and multipart image edits; decode and inspect returned images. Keep
credentials and raw account projections outside version control. Record actual
passes and failures, remaining vendor/production gates and service cleanup in
the [live-verification guide](../go-live-verification.md).

### Stage 4: remaining native management, files and measured performance

The user authorized the remaining migration and then performance improvement.
This plan is updated before implementation. Preserve Vue source and all existing
HTTP/data contracts; extend the explicitly invoked Go server and isolated data
directory, without production cutover.

Current-task sequence:

1. Finish remote CPA/Sub2API configuration/import jobs, OAuth, account execution
   tests and clearance testing. Preserve provider revisions, one active import,
   bounded Account Processing Concurrency, credential generations and progress.
2. Extend the sole Image Asset owner with gallery queries, tags, thumbnails,
   generation/op-id guarded deletion, compression, synchronization, GenBox and
   disk/retention cleanup. Preserve file layout, indexes and recovery tombstones.
3. Add independent Call Record queries/deletion, active monitoring, incremental
   dashboard hourly/model aggregates and Prompt Library sources/snapshots.
   Retention must preserve the metrics cursor; active requests stay in memory.
4. Port Editable File Tasks/PPT/PSD, native upstream execution, staged atomic
   publication of primary+ZIP, owner isolation, restart interruption and safe
   public downloads, retaining their existing SQL schema and external files.
5. Port backup list/test/run/delete with the existing archive contract and
   R2 behavior. SQLite snapshots use online backup plus integrity verification;
   PostgreSQL uses a consistent native dump. Retain independent backup/cleanup
   coordination leases and cancellation; never copy live WAL files.
6. Finish framework/CORS/HEAD/static Vue delivery and Go-aware update contracts.
   Python bundles/uv installation must never replace Go. Local packaging and
   rollback rehearsal can be prepared; push, releases and deployment require
   separate authorization. Do not invent successful unsupported operations.
7. Capture Python model/HTTP/storage fixtures, test real ZIP/PPT/PSD/image bytes,
   failure/restart/concurrency/lease/cursor behavior and SQLite/PostgreSQL round
   trips. Run complete race/vet/build and unchanged Vue desktop/mobile checks.
8. Only after functional gates pass, measure representative account, log,
   gallery/task and dashboard workloads on isolated fixed data. Identify the
   dominant query/allocation/I/O cost; implement bounded optimizations through
   the existing owners, then repeat the same benchmark and correctness gates.
   Report measured workload, before/after latency/throughput/allocations and
   unverified production behavior; no inferred speedup from language choice.

The implementation guide and route inventory will record actual delivered
coverage and limitations. API registration alone, offline fixtures and a build
are not live-service or user acceptance. Full automatic background maintenance
is opt-in during rehearsal; no production scheduler is started implicitly.

### Stage 3: public protocols and durable Image Tasks

The user explicitly authorized stage 3, retaining the Vue frontend and existing
API/data contracts. This section is updated before implementation. Extend the
independent Go rehearsal server; production Python traffic is unchanged.

Implementation order and current-task gates:

1. Preserve `/v1/models`, `/v1/chat/completions`, `/v1/responses`,
   `/v1/messages`, `/v1/images/generations`, `/v1/images/edits` and `/v1/search`.
   Keep model routing, OpenAI/Anthropic errors, JSON/usage, SSE framing/order,
   content filtering, auth and cancellation. Reuse stage-2 User Keys, settings,
   Account Pool and egress leases; never implement another pool or retry owner.
2. Extend the native upstream adapter for text attachments/thinking, Web image
   uploads/edits, Codex image tool results and search. Separate transport results
   from public protocol projection, output counts, usage and asset persistence.
3. Introduce one native Image Asset save/read owner for the execution paths:
   preserve image file layout, `image_index.json` entries/generations and lock
   conventions, local/WebDAV modes, URL/base64 behavior, bounded pinned-address
   downloads and safe public reads. Preserve the configured resize-engine path,
   EXIF orientation and encoded image format when scaling; native fallback may
   differ in pixels but must not silently force JPEG/WebP to PNG. Gallery mutation, compression and bulk
   reconciliation remain stage 4; preserve their catalog/tombstone state.
4. Port the four `/api/image-tasks` routes with owner-scoped idempotency,
   bounded queue/workers, edit spooling, progress/results, partial success,
   retention, durable JSON plus backup, shutdown and restart interruption.
   Queued/running jobs must never regenerate automatically after a restart.
   Resume polling must read an existing conversation without generation.
5. Keep Image Task and Asset files outside the Application Database under ADRs
   0003/0004. Require an explicit isolated data directory for enabling stage-3
   execution in the rehearsal server. Do not silently open production files or
   run Python and Go schedulers against the same task state.
   Append one compatible final Call Record for each actual public execution or
   task execution/resume, including failures and streamed completion. Preserve
   its existing SQL tables and independent owner; task admission/idempotent
   replay must not invent execution records. Log queries and metric aggregation
   remain stage 4.
6. Capture Python fixtures using synthetic credentials and injected execution;
   compare public success/errors/SSE, image input limits, owner isolation,
   idempotency, queue saturation, partial results and Python↔Go durable data.
   Exercise cancellation, concurrent submissions/leases, interrupted writes,
   backup recovery, restart and read-only resume. Run Go race/vet/build and
   unchanged Vue build/Studio checks, reporting unsupported paths explicitly.

The baseline persists `conversation_id` during task execution but drops it on
reload. Cross-restart resume must be tested explicitly; any correction must be
reported rather than described as previously verified Python behavior.

Editable-file/PPT/PSD APIs, full gallery/management/monitoring/backup and managed
updates retain stages 4–5. Call Record/metrics management is not moved into the
Image Task repository. Do not label successful fixtures as live-account,
full-console, production or user acceptance. No real credential file is used in
this stage's default verification; push and deployment remain separate gates.

### Stage 2: control-plane state and scheduling

The user explicitly authorized continuing account management, User Keys,
settings and proxy scheduling in Go, retaining Vue and API/data compatibility.
This document was updated before implementation. Python remains the running service;
the new Go HTTP entry point is an explicitly invoked rehearsal server against a
separately supplied Application Database. There is no automatic traffic cutover.

Implementation order and gates:

1. Preserve `accounts`, `auth_keys`, `storage_revisions`, `system_settings`,
   `account_group_configuration` and `proxy_configuration` schemas and JSON
   records. Keep collection CAS revisions, SQLite writer serialization,
   PostgreSQL row/advisory locks and each repository's transaction boundary.
2. Port bearer/bootstrap authentication, User Key CRUD, role/capability views,
   hashed storage, key rotation and disabled/revoked-key behavior.
3. Port settings defaults, validation, metadata, environment precedence,
   revision checks, partial nested updates and masked-secret preservation.
4. Port Proxy References, defaults, groups/nodes, import validation, assignment
   checks, route precedence, strategy metadata and image-egress leases. Preserve
   the actual Python runtime's capacity-aware random node selection: its stored
   `round_robin`/`time_window` fields currently do not select those algorithms.
   Proxy Session settings must never supply default egress.
5. Port account normalization and stable management identity, credential/quota
   projections, filtering/pagination/selection, local import/export, group
   assignment and atomic batch mutations. Port pool admission/release and the
   separate bounded account-maintenance lifecycle, including token rotation
   and synchronization through a replaceable upstream adapter.
6. Expose migrated management routes with the existing method/path, JSON,
   Chinese projections, errors and secret-response headers. Keep unmigrated
   routes explicit in the inventory; never return fake successful placeholders.
7. Use synthetic fixtures and isolated SQLite/PostgreSQL databases for Python ↔
   Go read/write differential checks, HTTP contract comparisons, stale revision,
   restart, cancellation and concurrent lease/mutation checks. Run the complete
   Go regression, vet and builds. Verify the unchanged Vue API adapter shapes.

CPA/Sub2API remote import, interactive OAuth login, account generation tests
with Call Records/Image Assets, cleanup/backup execution and the remaining
management surfaces retain their later-stage owners. Public generation routes
and durable Image Tasks belong to stage 3. Stage 2 may preserve their settings
and account state but must not claim those execution paths are migrated.

No production database, existing account file or live proxy configuration is
changed for verification. Real-account maintenance, deployment, push and
publication are separate authorization gates. Report implementation, automated
checks, frontend/browser checks, user acceptance and deployment separately.

### Stage 1 follow-up: Turnstile and live image delivery

The user supplied a credential-file location and explicitly authorized porting
the existing Python Turnstile logic, then repeating image generation and
verifying actual downloaded image bytes. Prior live sampling reached the
requirements prepare response and stopped at the unported Turnstile challenge;
it did not submit image generation. This follow-up supersedes the earlier
offline-only verification boundary for this narrow flow.

1. Port the baseline VM semantics into the Go upstream owner; evaluate a public
   implementation only where it demonstrably improves compatibility. Keep a
   bounded instruction budget, nesting/size limits and context cancellation.
2. Pass the solved value through requirements finalize and conversation headers.
   Do not log credentials, challenge payloads, solved values or signed URLs.
3. Compare synthetic cases with the Python implementation and run handshake,
   malformed-input, step-limit, cancellation and race regression checks.
4. Sample the explicitly supplied credential file, generate a benign image,
   download through a bounded HTTPS verifier, decode and inspect its pixels.
   Distinguish handshake success, asset URL discovery and actual image delivery.
5. Update current documentation with verified capabilities and remaining gaps.

No Vue, database, production Python or account-pool migration is included in
this follow-up. Arkose remains outside scope. Live failures must be diagnosed
without bulk-generating images across the credential file.

## Non-goals for the current stage

- Replacing or routing traffic away from the Python application.
- Changing Vue/Nanocat, public routes, model defaults or business projections.
- Opening, migrating or writing existing account databases and data files.
- Production cutover, unsolicited live-account operations, source publication,
  release creation and deployment. Remaining management/files/backup execution
  and measured performance work are included in the current authorized stage.
- Claiming live compatibility from fixture tests, or claiming a performance gain
  without measurement. No credentials are copied from another checkout.

## Unique owners and Interfaces

| Concern | Authoritative owner | Interface | Boundary |
| --- | --- | --- | --- |
| Existing business semantics and persistence | Existing Python domain services | Existing HTTP contracts | No dual writes |
| Go probe invocation, deadline and exit status | `go-backend/cmd/upstream-probe` | CLI arguments, environment, stdin/stdout | No HTTP server or background worker |
| Browser transport and session cookies | Go upstream Client | Context-bound HTTP requests | Per-client session; no automatic credential retries |
| Bootstrap, requirements and conversation lifecycle | Go upstream Client | Text stream / single-image result | One bounded operation per invocation |
| Turnstile evaluation | Go upstream Client's bounded VM | Challenge plus requirements token to solution or controlled error | No arbitrary script execution, network or filesystem access |
| Go control-plane lifecycle | `go-backend/cmd/control-server` | Explicit rehearsal HTTP server and shutdown | No automatic production routing or periodic maintenance |
| Go control-plane state | `internal/controlplane` and compatible repositories | Existing management JSON, revisions and leases | Same domain schemas and transaction boundaries |
| Go public execution | Public adapters and shared native executor | Existing JSON, errors, SSE and final Call Records | Reuse pool/egress; no Python forwarding |
| Go Image Tasks and Assets | Independent file-backed owners | Existing task projections, catalog and image reads | Explicit data directory; no SQL migration or replay |
| Browser state, rendering and polling | Existing Vue page runtimes | Existing API adapters | Unchanged |

Existing domain terms remain unchanged; no new persistent domain is introduced.

## Compatibility and persistence requirements

The [compatibility inventory](../go-api-compatibility.md) owns route-level
tracking and contract rules. Stages 2–5 must preserve absent/null/empty values,
JSON number meaning, Chinese labels, status codes, headers, SSE order and
terminal behavior, task ownership, pagination, URLs and download semantics.
Application Database tables, CAS revisions and transaction rules remain
compatible. Image Tasks, Image Assets and live monitoring retain ADR 0004's
separate stores. Rehearsals use isolated data copies; never run two schedulers
or cleanup workers against the same live data as a comparison technique.

## Prototype security and failure behavior

The probe reads a token from a dedicated environment variable, never an argument.
Requests target the fixed ChatGPT HTTPS origin; signed asset URLs are returned
without fetching them. HTTP/SOCKS proxy configuration remains explicit. Cookies
and browser identity are isolated per Client. Network, event and body sizes and
proof-of-work attempts are bounded. Context cancellation closes the active
request. Authentication, throttling, unsupported challenges, malformed SSE and
timeouts return distinguishable errors without dumping upstream bodies or
credentials. Arkose and unsolved challenges fail before conversation submission.
Known-conversation image polling is read-only and never resubmits generation.

## Acceptance criteria and verification matrix

| Criterion | Verification | Evidence/state |
| --- | --- | --- |
| Inventory covers every baseline method/path and handler | AST inventory checker | passed: 112 entries |
| Go CLI builds independently of Python and Vue | Go build, vet and CLI smoke | passed on Go 1.27.2; Darwin arm64 and Linux amd64 builds; 8 offline CLI cases including SIGINT and stdin deadline |
| Browser transport negotiates TLS/HTTP2, retains cookies and supports configured proxies | Loopback TLS/proxy integration | passed: HTTP/2, Chrome-profile cipher order, cookie isolation, HTTP CONNECT and certificate rejection; SOCKS not exercised |
| Bootstrap and requirements prepare/finalize use correct paths/headers | Mock upstream transcript | passed, including script-build precedence, SHA3-512 proof validation and Turnstile finalize/conversation forwarding |
| Turnstile VM preserves the baseline challenge behavior | Python differential fixtures, limits and live Sentinel | passed: 19 deterministic Python comparisons, bounded execution/cancellation and sampled live acceptance |
| Text snapshots and patches are decoded; SSE respects framing/limits | Deterministic fixtures | passed, including exclusion of analysis-channel text |
| Image prepare, conversation stream, polling and output resolution work | Mock upstream transcript; no input attachment counted as output | passed, including quoted SSE encoding marker, explicit nonterminal tool invocation and GET-only recovery |
| Cancellation, malformed events, auth/429, unsupported challenge and body limits are bounded | Failure-path integration tests and race detector | passed: 65 cases / 26 top-level tests, race detector clean; upstream statement coverage 75.1% |
| Live image generation delivers actual pixels | User-authorized sampled account, Go operation and independent bounded download/decode/visual check | passed for the sampled flow; PNG bytes fully decoded and visually matched the benign prompt; production Image Asset persistence remains outside this prototype |
| Live text and account-pool/model coverage work | Separately scoped live checks | not verified; remain cutover gates |
| Vue, Python production files and persistent data stay unchanged | Git diff and file inspection | unchanged; inventory checker is an added development script |

Automated checks, live upstream execution, browser checks, user acceptance,
source-control publication and deployment are separate gates.

## Stage 2 observed evidence

The [control-plane guide](../go-control-plane.md) owns current implementation
and detailed verification results. The stage-2 server registers 39 existing method/path
pairs; 73 remain scheduled for later stages. Python model/HTTP differential,
SQLite/PostgreSQL bidirectional persistence, concurrency, race/vet/build and
process smoke checks passed. The unchanged Vue built and its migrated sections
were checked against Go; optional cleanup checks and the unmigrated overview
still report 404. The existing Vue runtime test sources are absent from the
clone. PostgreSQL validation used 17.11; PostgreSQL 18, live maintenance, complete
failure/error variants and full-console cutover remain verification gates. Stage 4 supersedes the earlier
missing-route observations.

## Stage 3 observed evidence

The [public-execution guide](../go-public-execution.md) owns the current
implementation and verification limits. With an explicit data directory the
server registers 51 of 112 routes. Public contracts, native uploads/search/image
fixtures, task/asset/SQL round trips, final-call lifecycle, full regression and
unchanged Vue Studio/browser checks passed locally. Conversation IDs now survive
task reload. Real account and complete production cutover remain unverified;
stage 4 is described in the remaining-operations guide; production cutover remains planned.

## Stage 4 observed evidence

The [remaining-operations guide](../go-remaining-operations.md) owns delivered
behavior and limits. Full rehearsal registers all 112 baseline method/path
pairs. Remaining management/import/gallery/log/monitor/prompt/backup/cleanup and
editable-file operations are native. Local Go bundles and managed-install/
rollback notification are implemented; production and actual OS process
replacement are separate gates.

The final local regression passed 182 top-level tests / 837 leaf cases under the
race detector (905 pass events, zero failed or skipped cases), with SQLite and
PostgreSQL 17 bidirectional checks. Vet, Go 1.25 CGO-disabled Darwin/Linux builds,
route inventory, native CLI startup/HTTP/SIGTERM, actual local bundle creation
and unchanged Vue build passed. Eight pages were checked at desktop/narrow
viewports, including a submitted synthetic image task, persisted logs, gallery
and monitor. Same-workload Go benchmarks improved gallery pagination about 2.1x
and warm Prompt Library HTTP about 7.9x; these are not production or Python/Go
comparison results. Full external-service/live/cutover/user acceptance remains
unverified. Test sources and generated evidence remain ignored or external.

## Documentation and CHANGELOG impact

Maintain this PRD, the route inventory, ADR 0005, the Go prototype usage document,
documentation index and backend/system maps. Add an Unreleased development-tool
entry only after the probe exists. Keep production runtime documentation factual.

## Rollback

Stop the standalone probe or rehearsal control server and discard its isolated
fixture database. Python routing, deployment, schemas and files remain unchanged,
so this delivery needs no production data rollback. Later cutover must have its
own rehearsed rollback and migration plan.

## Unresolved verification

The original offline-only boundary was superseded by explicit authorization for
the sampled Turnstile/image flow. Live text, account-pool coverage, unsupported
challenge variants and production asset delivery remain unverified. Browser
fingerprint support does not guarantee future upstream acceptance. Stage 2 is
implemented locally with the explicit limits above; stage 3 is implemented locally with the public-execution guide
limits and pending live/user acceptance; stage 4 and local Go packaging are implemented
with the remaining-operations guide limits, while production cutover remains planned;
the overall migration is not deployed or user-accepted
by virtue of automated checks or the successful image sample.

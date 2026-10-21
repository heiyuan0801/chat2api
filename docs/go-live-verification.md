# Go PostgreSQL, Redis and live public API verification

Status: current

## Storage and cache configuration

The native `cmd/control-server` accepts `-redis-url` / `REDIS_URL` alongside the
existing explicit `DATABASE_URL` and `-data-dir`. A configured Redis requires a
successful startup ping. With no Redis URL, the existing memory cache remains.
`-redis-namespace` / `CHATGPT2API_REDIS_NAMESPACE` isolates deployments; the
default is a SHA256 fingerprint of the database URL and bootstrap secret, without
exposing either value. Keep an explicit namespace stable across instances of the
same deployment and use a distinct namespace for unrelated deployments.

Redis stores only sanitized successful public text events, using the existing
`chat_completion_cache` settings and request signature. Hash/index updates are
atomic, expired entries are removed, capacity evicts the earliest expiry, and
both keys expire after the final entry. Cache I/O has a 750 ms deadline and no
command retry. Runtime failures bypass the cache and execute upstream; errors
are not cached. In-flight deduplication stays local. This adapter uses a bounded
standalone [go-redis](https://redis.io/docs/latest/develop/clients/go/) pool;
it does not make file tasks or account/egress concurrency distributed.

PostgreSQL owns the existing structured domain repositories. Image tasks,
catalogs and image files retain their existing file format. Redis cache contents
are private response data and require suitable access protection. Neither API
projections nor Vue sources change. See [ADR 0009](adr/0009-optional-shared-text-cache.md).

## Authorized live rehearsal

The 2026-10-20 rehearsal used dedicated PostgreSQL 17 and Redis 8.2.2 instances,
isolated SQL/data directories and disabled automatic maintenance. The supplied
file contained 96 unique access tokens, all with a free-plan JWT claim; one JWT
was already expired. All 96 were imported through `POST /api/accounts` with
automatic upstream synchronization disabled and persisted across server restarts.
Automatic invalid-account removal was disabled for this diagnosis. Three
non-expired candidates were subsequently synchronized: two succeeded and one
returned a real upstream 401. Import counts do not establish all-account health.

The inherited proxy endpoint was unavailable and system DNS returned incorrect
ChatGPT addresses. An isolated proxy process using the existing node configuration
provided a working US exit. Its ports and runtime were separate from the user's
services; no system proxy, TUN or DNS setting was changed. Upstream availability
therefore depends on a working configured exit, not just valid account data.

| Public operation | Live result |
| --- | --- |
| `GET /v1/models`, `GET /api/model-catalog` | HTTP 200; six catalog models (two text, four web-image names) |
| `POST /v1/chat/completions`, `auto`, text JSON | HTTP 200, exact requested text; 17.125 s initial call, 0.004 s Redis replay; a second 6.240 s response replayed byte-for-byte in 0.004 s after replacing the Go process |
| `POST /v1/chat/completions`, `gpt-5.6`, text SSE | HTTP 200, text deltas, stop and `[DONE]`; 13.795 s |
| `POST /v1/chat/completions`, `gpt-image-2.5` | HTTP 200, actual blue-cup PNG decoded and inspected; 30.006 s |
| `POST /v1/images/generations`, `gpt-image-2`, URL | HTTP 200, returned URL downloaded, actual PNG decoded; 35.236 s |
| `POST /v1/images/generations`, `gpt-image-2.5`, SSE | HTTP 200, `image_generation.completed` with actual decoded PNG; 30.139 s |
| `POST /v1/images/edits`, multipart, `gpt-image-2.5-sunburst` | HTTP 200, actual uploaded green-tree reference became an orange tree with a purple sun; 37.597 s |
| `POST /v1/chat/completions`, reference image + `gpt-image-2.5`, SSE | Initial HTTP 503 `task_interrupted` after 156.221 s; repaired-path evidence follows below |
| `POST /v1/images/generations`, `gpt-image-2.5-flare`, SSE requested | Two initial HTTP 400 upstream text failures; repaired-path evidence follows below |
| User-key create/use/admin denial/revoke | HTTP 200 / 200 / 403 / 200; revoked key returns 401 |
| Gallery, logs and realtime monitoring | HTTP 200 and actual request/image records |

All successful image samples above are 1254×1254 PNGs; requested size is an
upstream hint, not a guarantee of exact dimensions. The `gpt-image-2.5*` names use
the existing upstream `auto` route. Catalog presence and public model aliases do
not prove a distinct underlying vendor model or paid/Codex entitlement.

There were real upstream tool failures: the first `gpt-image-2` request and
both `gpt-image-2.5-flare` requests returned a text failure after about two
minutes (121.575 s and 104.504 s). The successful retry above does not erase
those failures. One earlier
request was deliberately interrupted during the download-path repair and is not
counted as successful live generation.

## Follow-up diagnosis and repair

Read-only inspection of the three failed conversations found terminal tool
messages with `metadata.is_error=true` and no output image. The original image
parser missed this structured error and could return `upstream_text_reply`, or
continue polling until its timeout. A deadline during an active poll HTTP read
also crossed the execution boundary as request cancellation. Captured-shape
offline cases reproduced both issues before repair. The tool flag is now
classified as `image_tool_error`; the poll owner retains `image_poll_timeout`
and resumable conversation metadata. Caller cancellation remains cancellation.

Before changing the protocol, the same known successful account returned a
`gpt-image-2.5-flare` SSE PNG in 31.475 s. This rules out an unconditional alias
failure; the private pool had different account/route execution outcomes despite
healthy authentication and quota metadata. It does not establish the vendor's
reason for a given tool failure. Model aliases and upstream payloads remain
unchanged.

The Account Pool now records successful image execution and terminal tool errors
per stable account ID and actual upstream route. Entries expire after ten minutes
and are capped at 4096. The three `auto` aliases share evidence, while `gpt-5-3`
and Codex do not. Eligible proven accounts receive preference; unknown accounts
remain candidates when a proven account is busy, subject to existing limits.
An older concurrent success cannot clear a later failure; metadata refresh and
credential rotation cannot erase its cooldown. No account deletion, durable
capability state, SQL/file schema or Vue/API projection is added. Cold starts
still discover usable accounts, and vendor availability is not guaranteed.

The follow-up used a private PostgreSQL clone of the 96-account rehearsal and
the same configured US exit/Redis instance. A baseline pool request still failed
after 130.450 s; its actual account differed from the subsequent pinned control.
After the repair, the pinned proven account completed the flare request; all 96
test accounts were then re-enabled before the chat-reference request. The native
pool selected that same proven account for the latter, without switching.

| Repaired public operation | Actual delivery |
| --- | --- |
| `gpt-image-2.5-flare`, `/v1/images/generations`, SSE | HTTP 200, `image_generation.completed`, 30.867 s, 965070-byte PNG, 1254×1254, green triangle as requested |
| `gpt-image-2.5`, `/v1/chat/completions`, uploaded reference + SSE | HTTP 200, content delta, `finish_reason=stop` and `[DONE]`, 132.910 s, 1057463-byte PNG, 1254×1254; the supplied blue cup became red with its shape and white background preserved |

Both files were fully decoded and visually inspected; these are actual upstream
images, not fixtures. The chat sample demonstrates successful delivery near the
configured polling budget, not universally short upstream latency or all-account
availability. No timeout extension, model rename or frontend change was used.

## Download repair and validation boundaries

Native image execution now downloads ChatGPT's fixed
`/backend-api/estuary/content` route before closing its originating authenticated
browser client. Other signed asset hosts use the bounded credential-free public
transport. Redirects on the authenticated route are rejected. Download failure
after assets were generated no longer resubmits billable generation on another
account. Final bytes still pass image decoding and the existing asset owner.

Real-Redis checks cover independent-client/HTTP replay including SSE, TTL,
capacity, namespace isolation, malformed entries, cancellation and runtime loss.
Full-suite investigation also reproduced a SQLite in-memory cancellation issue:
concurrent reads/writes with canceled contexts could discard the final pooled
connection and destroy the database. An independent idle connection now anchors
the shared in-memory store until `Database.Close`. Three runs of the new stress
case failed before the repair; all three cancellation checks passed three repeats
afterward. The latest SQLite/PostgreSQL/Redis race suite passed 196 top-level and
852 leaf cases, with no failed or skipped tests. New captured-shape error,
route-sharing, expiry, rotation, concurrent-outcome and no-regeneration checks
also passed three repeats. Inventory covers 112 baseline
method/path pairs; vet and CGO-disabled Go 1.25 Darwin arm64/Linux amd64 builds
passed. The unchanged built Vue entry and its JS/CSS assets returned HTTP 200.
PostgreSQL and SQLite file-mode pooling are unchanged.
SQLite/PostgreSQL compatibility, race checks, API inventory, vet and cross-builds
remain required regression gates. Test reports, account payloads and images stay
outside tracked source. The earlier live checks above are samples, not a load
test or all-account validation. PostgreSQL 18, real external management/storage vendors, production
backup/restore, long-running concurrency/load tests, user acceptance and an
authorized production cutover remain separate gates. Nothing was pushed or
deployed by this rehearsal. The dedicated Go/PostgreSQL/Redis/proxy processes
were stopped after final binary checks; their test ports are closed and the
unrelated existing services remain running. Protected rehearsal data and decoded
image samples are retained outside the repository.


## Go image performance follow-up

The 132.910-second successful reference edit was inspected before optimization.
Its stored phases were account admission 1.389 s, upload 5.224 s, bootstrap
3.543 s, prepare 0.433 s, conversation stream 2.948 s, result discovery
109.266 s and download/publication 10.086 s. Read-only upstream inspection found
roughly 96.662 s from tool invocation creation to image-message creation and
103.905 s to its final update. Message updates are upstream timestamps, not
local observation times; they locate the dominant vendor generation interval
but cannot reconstruct every historical network request. SQL/Redis or Go CPU
were not established as the dominant cause.

Configured polling now reads the authoritative current-turn conversation first.
If it has committed image pointers, diagnostic `/backend-api/tasks` calls are
skipped. Pending/unavailable conversations still consult tasks, preserving
task-only results, structured failure precedence, partial results, Retry-After,
settling and cancellation. Existing generation payloads, timeouts, five-second
poll/settle settings and API/SSE framing are unchanged. No known-conversation
recovery submits a new generation.

Credential-free callbacks accumulate `poll_conversation_ms`, `poll_tasks_ms`,
`poll_sleep_ms`, `poll_settle_ms`, `asset_resolve_ms`, `asset_download_ms` and
`asset_publish_ms` in the existing per-image/Call Record monitor metrics. They
carry fixed names and durations only and do not reset lifecycle stage clocks.
Poll timings are sums of completed HTTP reads/waits, not vendor compute time.
For multiple images the call summary retains the maximum per-slot accumulation;
the per-image metrics retain their own totals. No SQL/file schema is added.

The deterministic ready-result benchmark used a simulated ten-millisecond
serial diagnostic delay. Ten iterations, repeated three times, changed from
11.314–11.860 ms/op to 10.083–15.458 microseconds/op by eliminating that request.
This measures removal of a controlled diagnostic wait, not real generation
throughput or a universal application speedup. Stalled probes, task-only output,
failed conversations, task auth/errors, additional outputs during settling and
eight HTTP requests sharing four one-slot accounts are covered by regression.


The isolated live follow-up used a PostgreSQL clone, Redis text-cache namespace,
four authorized accounts and the same private US egress. Redis does not cache
image outputs. Account concurrency stayed at one, maintenance concurrency was
four, and retries were disabled only in this private test configuration so each
HTTP request submitted at most one generation. Stream/poll budgets stayed at
140/120 seconds; initial/interval/settle waits stayed at five seconds. Baseline
and optimized two-call rounds used identical accounts, source PNG and prompt;
the optimized process began without runtime account evidence. The mixed round
added two previously unproven accounts. Healthy account/quota metadata alone is
not proof of image execution capability.

| Live round | Delivered/requested | P50 seconds | P95 seconds | Wall seconds | Delivered/minute |
| --- | --- | --- | --- | --- | --- |
| Baseline, two simultaneous chat reference edits | 2/2 | 70.350 | 81.275 | 81.287 | 1.476 |
| Optimized, same two chat reference edits | 2/2 | 47.753 | 57.673 | 57.681 | 2.080 |
| Optimized, four accounts: two edits + two flare generations | 3/4 | 43.074 | 133.752 | 133.756 | 1.346 |

Percentiles use nearest rank and include failures; P95 is the maximum at these
sample sizes. This is a bounded concurrency exercise, not a sustained capacity
claim. The four-call round overlapped all four distinct accounts with at most
one execution per account. Its successful edits/generations took 46.813 s and
38.416/43.074 s. All seven delivered PNGs fully decoded at 1254×1254; sampled
visuals show the reference cup changed to red and flare produced green triangles.
The original supplied file has 96 unique tokens; this exercise did not validate
all of them or claim that its filename's count represented usable accounts.

The two-account latency improvement cannot be attributed entirely to the poll
change: for the same account, download/publication fell from 31.124 to 9.844 s,
while discovery fell from 32.137 to 29.098 s. Optimized local publication across
successful calls took 12–23 ms. Two-second process samples during the mixed
round observed CPU up to 6.0% and RSS up to 97.8 MiB; interval sampling can miss
short peaks and is not a CPU profile or deployment sizing result.

The failed edit returned HTTP 502 `image_poll_timeout` after 133.752 s with a
known conversation and `can_resume_poll`; its 120.047-second discovery interval
included 14.525 s of conversation reads, 8.062 s of task probes and 97.389 s of
configured sleeping. No pointer or terminal error was visible before that
budget elapsed. Read-only inspection later found `metadata.is_error=true` and
no image: the tool failure arrived about 144.715 s after tool invocation and
158.932 s after the HTTP request began. A same-account native read-only poll
recognized `image_tool_error` in 2.046 s, issuing two GETs and zero generation
POSTs. Increasing the timeout would not turn this sample into an image.

The final Go 1.25 SQLite/PostgreSQL/Redis race suite passed 200 top-level and
858 leaf cases (927 test pass events), with no failed or skipped cases. Targeted
performance/concurrent-admission cases passed three repeats. Vet, unchanged
112-route inventory and CGO-disabled Go 1.25 Darwin arm64/Linux amd64 builds
passed. Vue/Python source is unchanged; no new browser UI acceptance is implied.
Sanitized measurements, decoded PNGs, protected fixture configuration and test
logs are retained under the external performance artifact directory.

The performance run exposed unbound multi-account recovery: the old native
executor selected a text-pool account and a different authorized account received
404 for the conversation. This is repaired by the origin-bound recovery below;
the performance diagnostic itself used the originating account explicitly and
was not evidence for the old task HTTP recovery implementation.

The dedicated performance Go/PostgreSQL/Redis/Mihomo processes were stopped;
ports 18090, 55442, 56442, 57897 and 59091 were verified closed. Existing
services on 55439, 56389, 8080 and 3001 remain listening. Protected rehearsal
files and actual image outputs are retained; nothing was committed, published
or deployed.


## Origin-bound timeout recovery

The repair follows [ADR 0010](adr/0010-bind-image-recovery-to-origin-account.md).
Native attempts carry the conversation and stable originating account together,
including parallel output slots and failures. The existing task owner persists
an optional internal `upstream_account_id`, loads it after restart and supplies
it through a private execution context. Tokens, input bytes and resubmittable
prompts are not stored. The public task projection and accepted request fields
are unchanged; injected origin fields in requests do not control execution.
Python readers accept the additive task metadata.

`Accounts.AcquireImagePoll` reserves only that account's existing image slot and
image-egress capacity, then uses its current credentials and proxy selection.
Generation quota exhaustion/tool cooldown do not prevent reading submitted work;
disabled/deleted/unusable accounts fail instead of selecting another account.
A resume's own deadline retains the known conversation and resumable timeout,
while actual caller cancellation retains its meaning. No generation, switching
retry or image-quota outcome mutation runs during resume.

For legacy files without a reference, the executor reads a bounded set of
conversation-indexed Call Records using the existing repository. Evidence must
belong to the same User Key and resolve unambiguously to one current account.
A single legacy image attempt or explicit per-attempt conversation identity can
provide the missing email association. Missing, conflicting or deleted-account
proof fails with controlled `no_available_account` and no upstream probing.
The task owner retains a recovered reference. Native final/streamed records now
retain existing account/conversation audit fields before public projection.
The existing injectable upstream-client factory also covers native image calls,
so the same protocol code can be observed by a credential-free transport adapter.

Live validation used the authorized accounts with an isolated PostgreSQL clone,
Redis namespace, task directory and the same private egress. Only the rehearsal
poll budget was lowered to five seconds to exercise recovery; stream budget was
140 seconds and automatic account retries were disabled. Two different accounts
submitted one benign image each, both persisted a known-conversation timeout,
and the actual Go process was stopped/restarted before HTTP task resume.

| Task/model | Initial timeout duration | Resume execution | Actual image |
| --- | --- | --- | --- |
| `gpt-image-2.5-flare` | 13.636 s | 8.571 s | orange circle, 922065-byte PNG, 1254×1254 |
| `gpt-image-2.5` | 13.080 s | 8.639 s | purple diamond, 1050852-byte PNG, 1254×1254 |

These are separate task execution durations, not total generation latency:
upstream work continued during the restart gap. Both files fully decoded and
were visually inspected. Wire audit associated each conversation hash with its
originating account fingerprint: each had two conversation GETs after restart,
all using its own account. Generation POSTs remained exactly two across both
processes; resume added zero. No approximate free allowance was hard-coded:
the user's estimate of 25 remained a test-budget hint, with remote quota
synchronization still authoritative.

A reconstructed legacy task used the preceding performance run's real failed
conversation and its existing owner-scoped SQL Call Record. After another real
process restart, the HTTP resume path recovered the missing account reference
and returned the actual `image_tool_error` in 1.812 s, using that originating
account only. It did not create an image or submit generation. An unproven
synthetic legacy task failed with `no_available_account` in 51 ms and caused
zero upstream requests. These two receipt fixtures are distinct from the two
newly generated/decoded images above. Total new generation submissions for this
repair stayed at two.

Verification covers restart, current credentials after rotation, exhausted quota,
owner isolation, forged request fields, missing/conflicting/foreign/deleted or
disabled origin, shared account slots, repeat polling timeouts, parallel-slot
identity pairing, streamed audit counts and Python task projection compatibility.
Vue/Python source and SQL schemas are unchanged. Production cutover, long-term
vendor behavior and user acceptance remain separate gates.

The final Go 1.25 SQLite/PostgreSQL/Redis race suite passed 207 top-level and
871 leaf cases (941 test pass events), with no failed or skipped cases. The
seven origin-recovery tests, including their subcases, passed three race-enabled
repeats. Vet, the unchanged 112-route inventory, formatting and CGO-disabled
Darwin arm64/Linux amd64 builds passed. No new browser UI acceptance was run
because Vue source and task projections were retained. Test scripts, protected
account configuration, wire evidence and actual PNGs remain outside tracked
source; nothing was committed, pushed or deployed.

The dedicated recovery Go/PostgreSQL/Redis/Mihomo processes were stopped and
ports 18092, 55442, 56442, 57897 and 59091 were verified closed. Protected
fixtures, task files, sanitized audit evidence and decoded images are retained.

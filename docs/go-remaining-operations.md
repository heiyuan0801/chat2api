# Go remaining operations and performance

Status: current

Stage 4 of [PRD 0001](requirements/0001-go-backend-migration.md) completes the
native rehearsal implementation under [ADR 0008](adr/0008-go-remaining-domain-repositories-and-operations.md).
With an explicit data directory the Go server implements all **112 baseline
method/path pairs**, including the static GET/HEAD fallback. The existing Vue
and Python production sources are unchanged. Registration, offline verification,
live vendor behavior, publication and production cutover are separate gates.

The subsequent [PostgreSQL/Redis live-verification guide](go-live-verification.md)
owns current account, text, image-generation/editing and shared-cache results,
including failed samples and the optional `REDIS_URL` configuration.

## Run the isolated server

Build the unchanged frontend with `npm ci && npm run build` in `web-vue`.
Build `go-backend/cmd/control-server` with Go 1.25 or newer. Supply an explicit
`DATABASE_URL`, `CHATGPT2API_AUTH_KEY`, absolute `-data-dir` and absolute
`-web-dir` pointing at the built `web-vue/dist`. `-init-schema` explicitly creates
compatible tables on a rehearsal database. Without `-data-dir`, the original
39-route stage-2 scope remains available. The default listener is loopback.

`-background` explicitly enables account credential renewal, pending-auth
recovery, limited-account synchronization, ten-second metric ingestion, retention
and backup schedules. These workers are disabled by default. They share the
normal processing gates, coordinator leases, cancellation and shutdown barriers.
Never compare two schedulers against the same live data directory.

## Native ownership and compatible storage

| Owner | Native behavior | Preserved data |
| --- | --- | --- |
| RemoteImports | CPA/Sub2API configuration, job reservation/progress, bounded processing, cancellation, restart interruption | Independent `remote_import_configurations` and job tables, provider revisions and sanitized progress |
| OAuthLogin / ProxyClearance | PKCE state, one-time callback consumption, scoped account execution, manual/solver clearance | Existing account credentials/settings; clearance stays an expiring process cache |
| ImageAssets | Gallery queries, tags, thumbnails, ZIP downloads, compression, deletion, WebDAV synchronization and GenBox | Dated files, catalog, tags, generation/op-id tombstones and unknown catalog metadata |
| CallRecords / Dashboard | Filtered summaries/details/facets, deletion, sequence/generation checkpoint, retained hourly/model projection | Existing Call Record and three dashboard SQL tables |
| RealtimeMonitor | Active/completed windows, stages, real timings, P95, attempts, diagnostics and detail projection | Process memory; completed call snapshots also enter normal final Call Records |
| PromptLibrary | Bundled library, source switches, validated registry refresh, error/cache health and compatible legacy loading | Independent `prompt_library_state`, settings/snapshot revisions and digest |
| EditableTasks | PPT/PSD task commands, bounded workers, native upstream execution, progress and public downloads | Existing SQL metadata plus staged/published files |
| BackupService / Retention | R2 connection/list/run/delete, encryption, rotation, cleanup previews/runs and schedules | Independent coordination/lease tables and baseline archive members |

No generic repository or Python HTTP forwarding was introduced. SQLite uses
explicit transactions and PostgreSQL uses domain advisory/row locks. Schemas,
JSON numeric values, owner IDs, account credential generations and CAS rules
remain compatible. The existing `.cumulative_total` integer file increments on
new account additions and does not decrease on deletion.

Remote import reservation covers both providers. Inputs containing credentials
are not persisted as job bodies. Account import and synchronization reuse the
Account Repository, processing gate and native maintenance client. OAuth keeps
PKCE and state expiry; failed callbacks may be retried until successfully
consumed. Clearance cookies are applied only to the fixed ChatGPT origin and
never to OAuth, signed uploads or arbitrary downloads.

Gallery mutation batches commit at most 16 entries. Catalog generation and
operation guards prevent a late synchronization/save from resurrecting a deleted
asset. GenBox auto-push is triggered after a durable Studio task or resume reaches
success; idempotent replay and synchronous public calls do not duplicate it.
Thumbnails and archives use the existing asset boundary and bounded resources.

Editable tasks use four workers and a 64-entry waiting queue. User-key ownership
and client-task idempotency remain intact. Inputs use private temporary spools;
SQL task data contains no resubmittable input bytes. Startup interrupts unfinished
tasks rather than generating again. The native Web flow uses the baseline
thinking model/effort, sandbox file resolution and bounded artifact downloads.
A valid primary PPTX/PSD and its valid ZIP must both exist before atomic
publication. PPTX and ZIP central directories/CRC and PSD structure are checked;
public downloads retain Range and attachment behavior. The tested PPTX opens
with editable text and the PSD decodes to actual pixels. Native input admission
is bounded to 16 references and artifact downloads to 100 MiB.

## Metrics, monitoring and frameworks

Dashboard ingestion reads only new Call Records and maintains 30 calendar days
of aggregates. User log deletion and retention synchronize metrics first and
preserve the append cursor. A source generation change rebuilds the projection;
existing hourly data with a missing state gets a checkpoint without double
counting. On PostgreSQL, checkpoint order is domain advisory lock, Call Record
`SHARE` table lock, then source-generation row lock. Waiting for in-flight writers
prevents serial sequence numbers committed out of order from being skipped.
Large initial rebuilds can block writers; warm a copied projection before cutover.

Logs and monitor views retain backend-owned Chinese presentation, attempts,
image counts, timelines and diagnostics. No unmeasured phase is reported as a
measured duration. Go has no AnyIO entry thread pool, so the legacy
`threadpool.tokens` and `previous_tokens` fields are **0**. The legacy runtime
`python_version` field contains the actual Go version to preserve the existing
schema; the unchanged Vue currently displays its existing “Python” prefix.
OS/kernel, process memory/CPU and disk counters are measured; unavailable counters
remain null. Runtime snapshots are sampled at most once per second.

Framework routes provide OpenAPI, Swagger, its OAuth redirect, ReDoc, CORS and
static GET/HEAD. Static lookup follows the baseline file, directory/index and
`.html` candidates; missing paths do not become a general SPA index fallback.
The full Python app's catchall takes precedence over partial method matches.
Consequently trailing-slash/method behavior follows the complete application,
correcting stage-3 fixtures that loaded only the public router. Documentation
schemas describe the actual registered handlers. Swagger/ReDoc retain their CDN
assets; offline availability of those third-party browser scripts is not claimed.

## Backups and managed Go updates

SQLite archives use online backup and integrity verification; PostgreSQL uses a
consistent custom-format `pg_dump` snapshot. File members are safely read through
their owners, including catalog recovery metadata and cumulative account count;
this is not a transaction-wide point-in-time snapshot of every file. Editable
`.staging` inputs are excluded. R2 signing, encrypted OpenSSL-compatible envelope,
rotation and cancellation use bounded streams and shared execution leases.
PostgreSQL backup requires compatible `pg_dump` on PATH.

`go-backend/cmd/release-bundle` builds a local Go release from an absolute module
and built Vue directory. Its flags are `-module-dir`, `-web-dir`, `-output-dir`,
`-version`, with optional `-goos`/`-goarch`. Output is
`chatgpt2api-go-OS-ARCH.tar.gz` and `checksums.txt`; the tar root contains a Go
manifest, `VERSION`, `bin/control-server`, `bin/upstream-probe` and `web_dist`.
The manifest binds runtime, version, platform and allowed paths.

Installation is disabled by default. A managed root needs a regular
`.chatgpt2api-go-runtime` marker, versioned `releases/`, and relative
`current`/`previous` symlinks. Only explicit `-runtime-root` plus
`-managed-updates` enables verified installation and CLI restart ownership.
The updater rejects Python bundles, wrong platforms, checksum/manifest mismatch,
path traversal and archive symlinks. It stages before atomically switching
`current`, retains `previous`, records task state and requests graceful process
replacement through the CLI. The new process reads `current/VERSION` and keeps
the existing arguments/environment. Local install/rollback and restart-notification rehearsal uses synthetic release
metadata and actual local Go binaries; a fresh Server reads the new task/version.
Native CLI startup/HTTP/SIGTERM is verified separately. Actual CLI `syscall.Exec`
replacement remains unverified; no GitHub release or remote deployment was made.

## Measured performance

These are same-machine **Go before/after** measurements on synthetic isolated
fixtures, not Python-versus-Go or production latency claims.

| Workload | Before | After | Change |
| --- | --- | --- | --- |
| 10,000 catalog images, paged gallery, median of repeated samples | 120.67 ms; 132.5 MB allocated | 57.14 ms; 49.0 MB allocated | About 2.1× faster; allocation bytes down 63% |
| 301 bundled prompts, authenticated warm `/api/prompts`, 3 × 100 requests | 5.22 ms; 8.92 MB allocated/request | 0.657 ms; 0.447 MB allocated/request | About 7.9× faster; allocation bytes down 95% |

Gallery optimization removes repeated JSON cloning and projects only the selected
page; catalog reads still use the current file lock and no stale catalog cache.
Prompt responses reuse one immutable serialized projection after checking the
shared SQL settings/snapshot revisions. Updates invalidate it; other processes'
commits are observed before reuse. Full library responses, errors and legacy
loading remain covered by Python fixtures.

The PostgreSQL checkpoint safety barrier was separately measured on small
fixtures: 20 single-record increments averaged 0.520 ms with P95 0.807 ms;
200 pending records with 100 concurrent writes synchronized in 2.669 ms, writer
P95 0.476 ms. A controlled out-of-order-commit regression proves no skipped or
double-counted record. These figures do not bound a large first rebuild.

## Verification and release gates

Verification uses ignored local tests/fixtures and external artifacts. Current
coverage includes Python request/response differential, SQLite/PostgreSQL 17
bidirectional persistence, real image/PPTX/PSD/ZIP bytes, bounded queues, cancellation,
restart interruption, archive/signed requests, cross-process leases, cursor
rebuild and failure rollback. The unchanged Vue builds; Dashboard, Accounts,
Settings, Proxy, Logs, Monitor, Gallery and Studio were exercised at 1440px and
390px, with no request errors or horizontal overflow in the checked views.
A submitted offline Studio image reached success and appeared in logs, gallery
and monitor. Browser console errors/warnings were empty; dark mode and the log
detail Escape/close path were checked. The unchanged Vue returned focus to the
page body after Escape in this browser; this frontend behavior was not changed.
External services were simulated for this stage. The ignored Vue runtime-test
source directory is absent from the clone, so that script could not be run.

Final `go test -race -json ./... -count=1` passed **182 top-level tests /
837 leaf cases** (905 pass events, no failed or skipped test cases). `go vet
./...`, Go 1.25 CGO-disabled Darwin arm64/Linux amd64 builds, the 112-pair
source inventory check and native 112-route/10-HTTP/SIGTERM CLI smoke passed.
The local release-bundle tool produced a verified Darwin arm64 archive. Real CPA/Sub2API/OAuth/FlareSolverr/WebDAV/GenBox/R2 services, real editable
upstream execution, PostgreSQL 18, large migration/rebuild loads, production
rollback/restore and user acceptance remain separate verification gates. This
stage did not read real account-token files, publish, push or deploy.

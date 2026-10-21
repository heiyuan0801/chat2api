# Go control-plane rehearsal

Status: current

Stage 2 of [PRD 0001](requirements/0001-go-backend-migration.md) is implemented
in the independent [Go module](../go-backend/go.mod). Python remains the
production service. Without a data directory, the Go server registers 39 existing management method/path
pairs; [the inventory](go-api-compatibility.md) marks every native delivery-stage
pair. It runs natively without invoking Python, forwarding requests to Python,
or fabricating successful responses for unmigrated routes.

Stage 3 adds optional [public execution and durable Image Tasks](go-public-execution.md)
with an explicit isolated data directory. [Stage 4](go-remaining-operations.md)
completes the remaining native routes, clearance, metrics and backup. The
observations below describe the stage-2 delivery.

## Implemented behavior

| Domain | Native Go behavior |
| --- | --- |
| Identity and User Keys | Bootstrap/Bearer auth, roles/capabilities, hashed keys, CRUD, disable/revoke and rotation; raw keys only at creation |
| Settings | Existing defaults/metadata, environment base URL precedence, revision checks, nested patches, blank-secret preservation and explicit GenBox key clearing |
| Proxy management | Default/fallback references, groups/nodes, imports, URL normalization, reference-use protection and fixed-origin probes |
| Proxy scheduling | Account > account group > explicit > resource > default, terminal direct, unavailable-group failure and idempotent capacity leases |
| Accounts | Stable management IDs, historical token fingerprints, credential/quota diagnostics, list/detail/filter/selection, local import/restore/export, groups and atomic batch updates/deletion |
| Maintenance | Separate RT exchange and metadata/quota sync, singleflight credential generations, late-result guards, transient/terminal errors and a shared processing budget |
| Image admission | Local estimate zero remains eligible, remote quota confirmation, account and node slots, cancellation/release and image-scope pending auth verification |
| Operations | Canonical and legacy progress projections, bounded events, process-local retention and shutdown cancellation |

The stored proxy `strategy` and rotation interval remain compatible metadata.
The pinned Python runtime actually picks randomly from nodes with free capacity;
Go preserves that behavior. Proxy Session configuration never supplies default
egress. Its settings are stored and masked, but FlareSolverr execution, session
reset integration and clearance cache behavior are not migrated by this stage.

Maintenance, image-account slots and image-egress slots have separate owners.
The processing budget is dynamically read from settings (1–100). Network work
runs outside SQL locks. RT exchange never queries quota; admin sync does not
proactively rotate RT, but may recover from an authoritative 401. Image-scope
401 blocks image admission and queues a bounded background verification while
preserving text eligibility. Background jobs use the server lifecycle and a
three-minute deadline. Leases release once on success, failure or cancellation.
Late network results cannot resurrect deleted accounts, overwrite a replaced
credential generation or re-enable an account disabled during the request.
These are local/synthetic checks; live maintenance has not been verified.

## Compatible persistence

[ADR 0006](adr/0006-go-control-plane-rehearsal.md) preserves the existing domain
boundaries on the Application Database:

- `accounts`, `auth_keys` and `storage_revisions` retain JSON-in-TEXT records,
  stable collection identities, opaque `accounts:N`/`auth_keys:N` revisions and
  CAS mutations. No-op mutations do not advance revisions.
- `system_settings`, `proxy_configuration` and `account_group_configuration`
  retain their independent singleton JSON repositories and transactions.
- File SQLite uses WAL, foreign keys, busy timeout and `BEGIN IMMEDIATE` writes.
  In-memory SQLite serializes access through one persistent pooled connection.
  PostgreSQL uses collection row locks and the existing per-document advisory
  lock names. Multi-process repository correctness does not depend on a Go mutex.
- Unknown JSON fields and precise stored integers survive read/write cycles.
  Targeted mutations preserve unrelated malformed collection rows; explicit
  replacement/deletion may repair or remove them. No legacy-file import occurs.
- Image Tasks and Image Assets remain outside SQL. The current explicit schema
  initializer also creates the two compatible Call Record tables for stage 3;
  their writer retains an independent repository transaction. Metrics and
  remote-import state retain their later-stage owners. No persistence domains
  are unified.

The rehearsal opens only an explicitly supplied database. Opening SQLite does
not create a missing file. Schema creation requires `-init-schema`; a newly
created file is private (0600). Do not compare Python and Go by running two
active schedulers/cleanup workers against production data.

## Build and isolated use

Go 1.25.0 or newer is declared. Validation used Go 1.27.2. Standard `net/http`
and `database/sql` own HTTP/SQL boundaries; `modernc.org/sqlite` and `pgx/v5`
are the necessary database adapters, pinned by `go.mod`/`go.sum`. Request schema
metadata/defaults live in `internal/controlplane/spec.json`, derived from the
pinned Python baseline and owned by Go at runtime.

```sh
cd go-backend
go build -o /tmp/chatgpt2api-control ./cmd/control-server
# Use a dedicated test key and a NEW isolated database, never the live URL.
export CHATGPT2API_AUTH_KEY='synthetic-local-rehearsal-admin'
/tmp/chatgpt2api-control \
  -database-url 'sqlite:////tmp/chatgpt2api-go-rehearsal.db' \
  -init-schema -listen 127.0.0.1:18080 -version 3.2.3
```

For an existing Python-initialized fixture database omit `-init-schema`.
`DATABASE_URL` may supply the explicit URL instead of the flag. Supported URL
forms are `sqlite:///...`, `postgresql://...`, `postgres://...` and
`postgresql+psycopg2://...`. Supply database credentials through a local environment
or protected configuration; do not include real credentials in shared commands.
`CHATGPT2API_BASE_URL` retains the existing read-only environment override.
`-version` supplies the displayed application version (unconfigured: `0.0.0`).

The default listener is loopback. The startup line reports address, backend and
route count, without the database URL or key. SIGINT/SIGTERM cancels operations
and verification workers before shutdown. No account network maintenance starts
merely because the server starts. Operations remain ephemeral and progress IDs
are not durable across restarts.

For the unchanged Vue development server, use its existing proxy option:

```sh
cd web-vue
VITE_DEV_API_TARGET=http://127.0.0.1:18080 npm run dev -- --host 127.0.0.1
```

Use Accounts, Proxy and the migrated Settings/User Keys sections for this narrow
rehearsal. Supplying an isolated data directory also enables the stage-3 Studio
execution paths described in the public-execution guide. The Go server has no
SPA fallback. Dashboard, Monitor, Logs, Gallery, update, backup/cleanup execution,
remote imports and interactive OAuth remain incomplete until their scheduled stages. In particular, Vue's existing
post-settings-save cleanup checks return 404 here even when the settings save
succeeds. A full-console cutover would therefore be premature.

## Verification evidence

Verification used only synthetic credentials and newly created local databases.
No production database, supplied credential file or live proxy settings were read
or changed for stage 2. This evidence is independent of the earlier live image
sample recorded in [the upstream guide](go-upstream-prototype.md).

| Gate | Observed result |
| --- | --- |
| Python model differential | 51 cases: account normalization/projections, settings defaults/revisions, proxy views and URL normalization |
| Python HTTP differential | 78 sequential steps replayed on SQLite and PostgreSQL: auth/key rotation, settings, proxies, account operations/progress and JSON/TXT/ZIP exports |
| Python ↔ Go persistence | Both directions on SQLite and PostgreSQL, schema/PK/nullability, large integers, unknown fields, revisions, rollback, corrupt-row repair and concurrent writers |
| Concurrency/lifecycle | Refresh singleflight, manual replacement/deletion/disable during requests, separate budgets/leases, cancellation, restart pending state and background auth verification |
| Native upstream fixture | Fixed OAuth origin/form with no AT forwarding, RT retention/rotation, transient/terminal classification, three parallel metadata calls with 401 precedence and bounded proxy probes |
| Go regression/static/build | 308 leaf cases / 51 top-level tests passed with the race detector; `go vet ./...` and Darwin arm64 / Linux amd64 builds of both CLIs passed |
| Process smoke | 9 checks passed: explicit configuration, no implicit SQLite creation, help/invalid flags, HTTP auth, unmigrated 404, private file, restart and SIGTERM |
| Existing Vue | `npm ci` and `npm run build` passed; browser checked login, account row, Proxy, Settings and User Keys; settings value 30→29 saved to isolated Go database |
| Existing Vue runtime suite | Could not run: the clean clone omits the ignored `web-vue/tests/` sources referenced by `test:runtime` |

The PostgreSQL rehearsal used **17.11**, not the deployment's PostgreSQL 18.
Version-18 rehearsal, real-account maintenance, live proxy routing/failover,
all failure-taxonomy variants, comprehensive malformed-input/Pydantic error
parity, full responsive/browser workflows and production cutover remain gates.
A registered route or passing fixture does not establish those results.

Per repository rules, test harnesses, synthetic fixtures, logs and screenshots
remain in ignored local `tests/` and `.codex_tmp/`/external artifact directories.
A clean clone does not include them. The route inventory checker is retained as
a development script; it verifies source coverage, not full behavioral parity.
Automated tests, this limited browser check, user acceptance, publication and
deployment are separate statuses. Stage 2 has not been user-accepted or deployed.

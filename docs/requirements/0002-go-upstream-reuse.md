# 0002: Reduce repeated Go upstream setup

Status: pending-verification

- Owner: Go control plane and upstream maintainers
- Created: 2026-10-24
- Updated: 2026-10-24
- Related ADR: [0012](../adr/0012-bounded-go-upstream-reuse.md)

## Problem and user outcome

Every image admission remotely synchronizes its account, and native adapters
create and close browser transports per call. The user requested preflight
caching and connection reuse to reduce repeated network setup under concurrency.
Actual production latency improvement remains unmeasured for this change.

## Scope and non-goals

Add bounded successful preflight evidence and merge concurrent checks. Reuse
authenticated browser transports across requests without serializing protocol
Clients. Preserve account/egress capacity, deadlines, current quota/status and
original-account recovery. No new UI setting, account probe repair, poll timing
change or quota reservation scheme is included. Source delivery and deployment
are separately authorized follow-up actions.

## Domain language and unique owners

Use existing Upstream Account, Account Pool, Upstream Credentials, Proxy Session
and Account Processing Concurrency from [CONTEXT](../../CONTEXT.md).

| Concern | Authoritative owner | Consumer interface |
| --- | --- | --- |
| Durable account, credentials, quota and status | Account Repository / Accounts | Get, mutations |
| Preflight evidence, invalidation and merging | Maintenance | Preflight; Sync bypasses cache |
| Image and egress admission/recovery | Accounts / Proxy | AccountLease / EgressLease |
| Transport retention and shutdown | Server-owned upstream TransportPool | New / Close |
| Protocol state and callbacks | Per-request upstream Client | Existing execution methods |

## Interfaces, persistence and frontend

Keep existing Preflight, AccountUpstream and client factory call signatures.
TransportPool supplies the existing client factory. Cache memory is disposable
on restart; no database/file format, migration, backup or Vue changes are needed.
There is no new public route or response envelope.

## Concurrency, security and failure behavior

Only positive evidence lives ten seconds; current repository data is read on
every hit. Credential/status/quota/routing changes bypass old evidence, manual
sync stays fresh, failures are not cached, and a canceled waiter does not cancel
the initiating caller. An initiating cancellation permits surviving callers to
retry within their own deadlines. Shutdown cancels preflight work. Transport
reuse isolates account credentials and routing/session policy, bounds retained
entries and closes idle connections. Five same-account calls retain independent
operation gates and request callbacks. Recovery never submits another generation.

## Acceptance and verification matrix

| Criterion | Verification | Status |
| --- | --- | --- |
| Concurrent account checks merge and sequential checks reuse fresh evidence | Maintenance fixtures through Preflight and AcquireImage | passed locally |
| Expiry, mutations, quota/status, routing, sync, errors and cancellation remain correct | Deterministic fixture, race detector | passed locally |
| Sequential calls reuse actual TCP connections with isolated credentials/cookies | Local HTTP transport fixture | passed locally |
| Same account retains five concurrent operations, independent callbacks/limits | Local upstream fixture, race detector | passed locally |
| Retention, overflow and shutdown release connections | Transport lifecycle fixture | passed locally |
| Existing API, stores and recovery still pass | Full Go regression, vet and build | passed locally |

## Documentation and rollback

Update backend map, public execution guide, requirement index and CHANGELOG.
Reverting the Go change restores per-call preflight/transports without data
rollback. Deployment and live performance verification are separate release gates.
No unresolved product decision remains.

## Local verification evidence

The complete Go regression passed 233 top-level tests (1,010 including nested
cases), including the configured SQLite/PostgreSQL and Redis paths, with no
skipped tests. Targeted race checks cover preflight cancellation/invalidation,
processing capacity, five concurrent same-account operations, actual TCP reuse,
credential/egress/cookie isolation and transport shutdown. `go vet` and `go build`
passed with `-mod=readonly`.

The tests use synthetic accounts and an isolated local TLS/CONNECT fixture;
production services and existing account data were not changed. Frontend and
browser checks are not applicable because no UI or public contract changed.
Live speed/success-rate measurement and user acceptance remain unverified.
Source delivery and deployment have separate verification gates and do not
establish a measured performance gain or product acceptance.

---
status: accepted
---

# Rehearse the Go control plane with compatible domain repositories

The user authorized stage 2 of PRD 0001 after the upstream prototype. Extend
ADR 0005 with an independently invoked Go HTTP control plane. Require an
explicit Application Database URL and bootstrap key; schema initialization is
an explicit operation. Keep the default listener on loopback and leave Python
traffic routing unchanged.

The Account Repository still owns only Upstream Accounts and User Keys.
Settings, account groups and proxy configuration retain their own repositories
and transaction boundaries on the same physical database. Preserve existing
tables, JSON payloads, collection identities and revision formats. Unknown
stored fields survive mutations. No automatic import, dual writes, schema
conversion or Image Task/Image Asset database migration is introduced.

Go services own authentication, account projections and mutations, effective
settings, Proxy Reference normalization and scheduling through their existing
domain boundaries. HTTP handlers validate and translate those results; Vue
keeps its current API adapters and interaction/rendering ownership. Account
maintenance concurrency, image account slots and proxy-node slots are separate
limits. Leases must have one owner and idempotent release on cancellation.

## Consequences

- Stage 2 is tested against isolated SQLite and PostgreSQL fixtures/copies.
  Never open a production database merely to inspect migration compatibility.
- Preserve compare-and-swap and SQL locking semantics across Python and Go;
  in-process mutexes alone are insufficient for repository correctness.
- Use Go's standard HTTP and SQL interfaces. Database drivers are necessary
  adapters; protocol and policy logic must run natively in Go.
- Do not forward migrated routes to Python or add successful placeholders for
  later-stage routes. Keep the compatibility inventory explicit.
- Live account maintenance and production cutover remain separately authorized
  operations. No new background maintenance runs merely because the rehearsal
  HTTP server starts.

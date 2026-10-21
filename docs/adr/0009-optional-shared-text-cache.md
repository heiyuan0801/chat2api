---
status: accepted
---

# Optional Redis cache for public text responses

The user approved PostgreSQL live verification and an optional Redis shared
text-response cache. PostgreSQL remains the Application Database under ADR 0004;
Image Tasks and Image Assets remain files under ADRs 0007–0008. Redis does not
own accounts, task scheduling, leases, gallery state or durable business records.

An explicit Redis URL enables a bounded cache adapter for the existing public
text event cache. Without it the current process-local cache remains available.
The existing request signature, cache settings, stream replay and per-process
in-flight deduplication remain unchanged. Only successful, sanitized events are
cached, with the existing response-size ceiling. Atomic Redis operations enforce
expiry and capacity; namespaces isolate deployments, and colocated keys support
Redis Cluster hash slots. Response text is private data: protect Redis access
and use TLS when connecting over an untrusted network.

Configured Redis must answer a startup ping. Runtime cache errors are bounded
and degrade to native upstream execution; a cache failure must not fabricate a
successful response or prevent upstream errors from reaching the caller. Cache
entries are disposable, and no cache migration or dual-write is required.

## Verification

Rehearse against dedicated PostgreSQL and Redis instances with protected account
fixtures. Verify cross-client replay, expiry, capacity, cancellation, corrupt
entries and runtime outage, then import authorized accounts and test model
listing, text, chat images, image generation and image editing through HTTP.
Actual image bytes must decode before claiming live image success. Report each
model tested and distinguish catalog presence from upstream availability.

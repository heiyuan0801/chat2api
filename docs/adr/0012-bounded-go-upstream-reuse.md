---
status: accepted
---

# Bounded Go preflight evidence and transport reuse

The user authorized account preflight caching and connection reuse for the Go
runtime. Maintenance owns ten-second successful preflight evidence and merges
concurrent checks of the same credential generation and routing configuration.
Evidence never contains a second account snapshot: every admission reads the
Account Repository. Local account mutations, image outcomes, refresh, explicit
synchronization and authentication verification invalidate evidence. A signature
also includes canonical account and routing/session configuration so external
repository changes cannot silently reuse older evidence. Failures and exhausted
accounts are not cached. Caller cancellation and the existing processing gate
continue to bound work; surviving waiters can retry a canceled check.

Server owns one bounded upstream TransportPool and closes it during shutdown.
The pool retains at most 128 authenticated transports, isolates credentials,
egress, browser identity, clearance, TLS policy and timeout, and retires unused
entries after two minutes on access. Connections also have a 90-second idle
timeout. An active entry is never evicted for capacity; excess distinct active
configurations use independently closed clients. Unauthenticated login flows
retain independent cookie jars.

Each acquisition returns an independent protocol Client with its own operation
gate, limits and callbacks. Sharing a protocol Client would serialize concurrent
image requests and retain request callbacks, so only browser transport/cookies
are reused. New credential or proxy configurations cannot borrow old entries;
old idle configurations expire or are evicted. No credential fingerprint is
logged, and certificate verification, redirect policy and safe asset download
boundaries remain owned by upstream.

No public setting, schema, durable store, Redis role, quota reservation rule or
image recovery ownership changes. Account and egress leases still own admission;
known conversations still recover only through their originating account. This
reduces repeated setup work but cannot shorten upstream generation itself.

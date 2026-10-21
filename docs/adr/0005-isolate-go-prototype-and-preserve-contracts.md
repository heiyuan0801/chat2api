---
status: accepted
---

# Isolate the Go prototype and preserve existing contracts

The user approved a complete backend migration to Go, retaining Vue and API/data
compatibility, starting with an inventory and upstream prototype. Implement the
prototype in an independent `go-backend` module and invoke it explicitly through
a CLI. The Python application remains the sole owner of production HTTP routes,
domain semantics and persistent state during this stage.

Port protocol behavior against the current source, not method-name similarity.
Use a browser-profile-aware transport because the existing upstream adapter
depends on TLS/HTTP2 impersonation. A transport library is a candidate until
local tests and live upstream evidence demonstrate the required behavior.

The Go Client owns one conversation operation and its deadline. It exposes
transport-independent results for text and image discovery; it does not claim
to implement the public OpenAI API, Account Pool or durable Image Tasks yet.

## Consequences

- Keep accepted ADRs 0001–0004 and all existing data ownership intact.
- Preserve Vue API adapters and public JSON/error/SSE contracts at cutover.
- Track unimplemented routes explicitly; do not create fake success handlers or
  route the prototype through Python and call that a Go migration.
- Use isolated data copies and deterministic protocol transcripts for parity.
  Do not mirror writes or duplicate billable upstream generation as a test.
- Keep runtime credentials, signed URLs and raw upstream responses out of logs
  and committed artifacts. Live verification and deployment are distinct gates.
- Managed updates currently install Python source and run `uv sync`; their Go
  replacement belongs to the packaging stage, not this prototype.

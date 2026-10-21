---
status: accepted
---

# Complete native Go domain operations without merging ownership

The user authorized the remaining backend migration and subsequent measured
performance work under PRD 0001. Extend ADRs 0004–0007 with native management,
imports, gallery operations, observability, editable files and backups while
retaining the existing Vue and API/data contracts. Python traffic is unchanged.

Existing domain repositories remain independent on the Application Database:
remote-import configuration/jobs, Call Records, hourly dashboard projections,
Prompt Library, coordination/backup state and Editable File Tasks retain their
baseline tables, keys, JSON payloads and transaction boundaries. Explicit schema
initialization may create these compatible tables; server opening never infers
or converts legacy state. No generic domain repository or dual-write is added. PostgreSQL metric ingestion
locks the Call Record table in SHARE mode before its source-generation row and
checkpoint, preventing out-of-order serial commits from being skipped; large
initial rebuilds therefore require offline prewarming before cutover.

ImageAssets remains the sole catalog/file mutation owner, using generation and
operation guards and bounded mutation batches. Image Tasks remain file-backed.
Editable file metadata is SQL-backed while staged/published artifacts remain
files; primary and ZIP must both exist before successful publication. Active
monitor state is process memory. Cancellation, durable restart interruption and
exclusive worker ownership must not regenerate billable upstream work.

Backup and retention coordinators reuse baseline database coordination leases.
SQLite backup uses its online API with integrity verification; PostgreSQL uses
native consistent dumps. The backup archive excludes credentials from logs,
though archive content itself is protected user data. Destructive verification
uses only synthetic isolated files/databases and mock external endpoints.

Managed update contracts must recognize Go artifacts and reject incompatible
Python bundles. Packaging/rollback can be rehearsed locally; no release,
push, deployment or production maintenance is implied by migration work.

## Consequences

- Reuse native execution, Account Pool, proxy leases and final CallRecord hooks.
- Preserve errors, JSON projections, download/archive semantics and cursors.
- Background scheduling is explicit during rehearsal, never an accidental
  consequence of opening a copy of a live database.
- Performance changes follow functional parity and before/after measurements
  of the same workload; caches remain bounded and revision/generation-aware.
- Live upstream/vendor behavior, user acceptance and production cutover are
  separate gates from offline fixture/browser verification.

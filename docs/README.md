# Documentation

Status: current

## Source of truth

When documentation and implementation disagree, use this order:

1. Current code, tests, and public request/response contracts.
2. [`../CONTEXT.md`](../CONTEXT.md) for domain terms and ownership.
3. Accepted records in [`adr/`](adr/).
4. Current documents in this directory.

Plans describe intended work, not completed behaviour. Historical smoke records,
old route maps, and documents for deleted features are not retained as current
documentation.

Executable documentation rules live in
[`../.codex/rules/documentation.md`](../.codex/rules/documentation.md).

## Navigation

| Area | Owner and purpose |
| --- | --- |
| [`../CONTEXT.md`](../CONTEXT.md) | Domain terms, relationships, and ownership |
| [`../.codex/rules/`](../.codex/rules/) | Executable AI and automation development rules |
| [`adr/`](adr/) | Accepted architecture decisions |
| [`requirements/`](requirements/) | PRDs, acceptance criteria, and their lifecycle |
| [`maps/`](maps/) | Curated maps of the current implementation |
| [`runbooks/`](runbooks/) | Repeatable deployment, recovery, and operations procedures |
| [`references/upstream-projects.md`](references/upstream-projects.md) | Upstream projects and tool-usage boundaries |

## Current maps

| Map | Purpose |
| --- | --- |
| [`maps/system-map.md`](maps/system-map.md) | Runtime, authority, and persistence boundaries |
| [`maps/backend-map.md`](maps/backend-map.md) | Routers, services, projections, and repositories |
| [`maps/frontend-map.md`](maps/frontend-map.md) | Routes, page runtimes, API adapters, and UI ownership |
| [`maps/critical-flows.md`](maps/critical-flows.md) | Import, Image Task, observability, dashboard, and update lifecycles |

## Current documents

The [Go PostgreSQL/Redis live-verification guide](go-live-verification.md) records
the optional cache setup, authorized account import, actual public API results
and origin-account timeout recovery after process restart.
The [Go release runbook](runbooks/go-release.md) owns the separate container,
PostgreSQL/Redis deployment, clean-source CI and quiesced backup/restore/rollback
commands; production routing remains separately authorized.

| Document | Status | Purpose |
| --- | --- | --- |
| [`control-panel-architecture.md`](control-panel-architecture.md) | current | Routes, ownership, persistence boundaries, and UI boundary |
| [`control-panel-data-contract.md`](control-panel-data-contract.md) | current | Backend projections and frontend consumption rules |
| [`control-panel-action-safety.md`](control-panel-action-safety.md) | current | Mutation ownership and destructive-action boundaries |
| [`control-panel-performance.md`](control-panel-performance.md) | current | Query, pagination, refresh, and rendering rules |
| [`image-failure-handling.md`](image-failure-handling.md) | current | Image failure classification, switching, and diagnostics |
| [`storage-architecture.md`](storage-architecture.md) | current | Independent persistence responsibilities |
| [`prompt-library-architecture.md`](prompt-library-architecture.md) | current | Prompt source and library projection lifecycle |
| [`deployment.md`](deployment.md) | current | Docker, source development, backup, and upgrade guidance |
| [`upstream-sse-conversation.md`](upstream-sse-conversation.md) | current | Internal Conversation SSE and result-resolution reference |
| [`go-api-compatibility.md`](go-api-compatibility.md) | current | Complete method/path/source inventory and Go migration compatibility gates |
| [`go-remaining-operations.md`](go-remaining-operations.md) | current | Stage-4 remaining routes, files, observability, backup, local Go updates and measured performance |
| [`go-public-execution.md`](go-public-execution.md) | current | Stage-3 public protocols, durable Image Tasks, asset save/read and final Call Records |
| [`go-control-plane.md`](go-control-plane.md) | current | Native stage-2 management routes, compatible repositories, scheduling and rehearsal evidence |
| [`go-upstream-prototype.md`](go-upstream-prototype.md) | current | Standalone Go probe, Turnstile, sampled live image evidence and remaining compatibility gates |

## Documentation rules

- Each current document and PRD must carry the status defined by the owning
  documentation rule.
- A current document only describes verified behaviour and real routes.
- A PRD must name its non-goals, unique owners, Interfaces, acceptance criteria,
  and verification evidence.
- Removed routes and features must be removed from current documentation in the
  same change that removes them from the product.
- Do not add generated reports, one-off smoke logs, or local experiment notes to
  the current documentation set.

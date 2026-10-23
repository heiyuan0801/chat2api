# Product Requirements

Status: current

This directory owns requirements that describe intended product changes and
their acceptance criteria. It does not describe current implementation truth;
use the current implementation documents linked from
[`../README.md`](../README.md) for that purpose.

## Lifecycle

```text
draft -> approved -> implementing -> pending-verification -> implemented
   \-> rejected
```

| Status | Meaning |
| --- | --- |
| `draft` | Problem, scope, or ownership is still being discussed. |
| `approved` | The user has accepted the scope and acceptance criteria. |
| `implementing` | Work has started, but the document still expresses intent. |
| `pending-verification` | Implementation and automated checks are done; required product verification remains. |
| `implemented` | Acceptance evidence exists and affected current documentation is synchronized. |
| `rejected` | The proposal will not be implemented; the reason is recorded. |

Create a requirement from [`_template.md`](_template.md) and name it
`NNNN-short-name.md`. Keep active requirements here. Completed or rejected
requirements may move to `archive/` after links and this index are updated.

## Active requirements

| Requirement | Status | Scope |
| --- | --- | --- |
| [0001: Go backend migration](0001-go-backend-migration.md) | implementing | Retain Vue and API/data compatibility; stages 1-4 implemented locally with explicit validation limits; local Go packaging/update rehearsal included, production cutover pending; overall migration and user acceptance pending |
| [0002: Go upstream reuse](0002-go-upstream-reuse.md) | pending-verification | Local implementation and automated verification passed; live performance and user acceptance unverified, deployment excluded |

PRDs are planning evidence, never proof that the product already behaves as
described.

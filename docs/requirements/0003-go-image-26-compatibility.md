# 0003: Go Image 2.6 compatibility

Status: pending-verification

- Owner: Model Catalog and native image execution
- Created: 2026-11-05
- Scope confirmed: Go checkout, local implementation and tests

## Problem and evidence

The official [prompting guide](https://developers.openai.com/api/docs/guides/image-prompting)
documents `gpt-image-2.6-flare` and `gpt-image-2.6-sunburst`. Go currently rejects
both. This gateway uses ChatGPT Web rather than the official Images API. The
official guide does not establish a Web selector for either variant.

## Scope and unique owners

Model Catalog owns accepted names and the compatibility notice. WebImageRoute
owns routing and shared account execution evidence. Image validation owns
unsupported-parameter errors before normalization or account execution. Studio
renders the backend notice without inferring capabilities. Defaults, tasks,
account data, persistence and Codex behavior keep their existing owners.

## Non-goals and unresolved boundary

No deployment, publication, schema migration, new API-key provider, SVG/GIF
pipeline or guaranteed native 2.6 execution. Names use the existing `auto` route.
Exact variant selection and complexity require verified Web protocol evidence;
mocked delivery cannot establish real model identity.

## Acceptance and verification

| Criterion | Verification |
| --- | --- |
| New names in default catalogs and generation/edit requests | Isolated HTTP and transport tests |
| Configured catalogs, defaults and existing models preserved | Regression tests |
| Unsupported controls fail before upstream execution | JSON, multipart, chat and task tests |
| Studio renders backend compatibility notice | Frontend build and focused rendering check |
| Native 2.6 model identity | Unverified; requires Web protocol evidence |

No stored data changes; rollback is a source/binary rollback.

## Local validation

The targeted API/transport suite, Go race regression (SQLite and isolated
PostgreSQL 17), `go vet ./...`, `go build ./...` and Vue production build passed.
The first full run lacked `pg_dump` in PATH; the affected backup case passed
with the installed PostgreSQL tools. Redis-dependent cases were skipped.
Focused Studio Composer checks covered 1440x1000 and 390x844, light/dark themes
and notice visibility without real account requests. This is not a full console
browser regression or live-upstream verification. User acceptance is pending.

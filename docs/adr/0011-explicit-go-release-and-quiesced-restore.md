---
status: accepted
---

# Explicit Go release artifacts and quiesced operational restore

The user authorized Go release-readiness work before a separately authorized
canary. Extend ADR 0008 with explicit Go Docker/Compose and tag-gated CI release
artifacts. Keep the Python image and deployment entry point available; adding
Go artifacts does not change an existing installation's runtime or routing.

The container owns process startup and signal delivery; Compose owns service
health, dependencies and persistent volumes. The native server remains the sole
owner of API semantics, workers, repositories and tasks. Initial schema creation
is a separate short-lived command, never an implicit production startup change.
Container updates use immutable images and orchestrator restart; managed binary
replacement remains disabled in this deployment mode.

Operational backup/restore is a deployment action rather than a new domain
repository or public API. Quiesce the sole application writer before capturing
a consistent PostgreSQL custom dump and compatible task/asset files. Verify
checksums and archive paths before restoring. Restore only into an empty,
isolated database and data volume, leaving the original deployment and backup
intact. A partial restore must remain stopped and ineligible for service.
Redis is disposable successful-text cache and is not part of the backup.

## Consequences

- Run one Go process per Image Task/Asset directory. PostgreSQL and Redis do not
  make file workers safe for multiple active application replicas.
- No persistence transfer, dual writes, schema conversion or new HTTP routes.
- Source-only clean builds must include all Go production files and embedded
  specs, while excluding local test sources, secrets and generated artifacts.
- GitHub Releases and registry images require matching `v*` tag-push events;
  pull requests and manual workflow invocation perform verification only. The
  subsequent explicit user request authorizes verification-only branch pushes
  to `heiyuan0801/chat2api` and temporary downloadable Actions artifacts.
- The subsequently authorized GHCR publication uses a matching `v*-go.*`
  prerelease tag for Go-only images, skipping Python images and mixed-runtime
  Releases. Normal `v*` tags retain the combined release path. Deployment uses
  the existing Compose with a pinned registry image and no source build.
- Verify backup/restore and binary/image rollback on isolated synthetic data.
  Target-host acceptance, real vendor behavior, publication and production
  traffic switching remain separately reported gates.

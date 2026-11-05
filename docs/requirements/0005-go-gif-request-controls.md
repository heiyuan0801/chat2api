# 0005: Go GIF request controls

Status: pending-verification

- Owner: native image execution, Settings and existing Image Task / ImageAssets owners
- Created: 2026-11-05
- Scope: local implementation and verification; subsequent publication and sequential deployment authorized

## Problem and user outcome

GIF delivery preserves actual files, but callers cannot request GIF output and
animation hints through structured parameters. Add those controls while keeping
the existing default Base64 response and reusable image delivery lifecycle.

## Scope and Interfaces

Images generation, edits (JSON/form/multipart) and asynchronous Image Tasks accept
optional `output_format=gif`, `frame_count` (integer 1–120) and
`frame_duration_ms` (integer 10–10000). Frame controls require GIF output; absent
or null controls add no default hints. Non-GIF output-format strings are accepted
for legacy compatibility and add no format instructions or conversion. Invalid
types, frame ranges and frame controls without GIF fail before upstream execution.
Chat/Responses controls remain unsupported.

`image_gif_model_scope` is a persisted Settings enum: `image26` (default, the two
existing 2.6 compatibility names) or `all` (all supported image models). It
controls admission of explicit GIF parameters, not prompt-only GIF output or
upstream capabilities. No new model aliases, native model selector, forced URL
response, transcoding or frame enforcement is included.

## Unique owners and persistence

Public image contracts own parameter bounds. Native image execution owns scope
validation and upstream-only prompt hints; it retains the caller prompt for
logging and revised_prompt. Settings owns persistence, revision conflicts and
field metadata. Vue renders those options with existing Nanocat controls and
owns the settings draft, layout and focus. Image Task admission/execution,
retry/cancellation and ImageAssets persistence/download remain unchanged.
No migration, new dependency, storage owner or ADR is required.

## Acceptance and verification

| Criterion | Evidence | State |
| --- | --- | --- |
| Valid controls survive generation/edit/task normalization | HTTP fixture tests | passed locally |
| Invalid controls and model scope fail before upstream | HTTP and execution tests | passed locally |
| Scope persists, rejects invalid enums and applies at execution | Settings and queue regression | passed locally |
| Original prompt/default Base64, URL/SSE and actual GIF bytes survive | Delivery regression and local live request | passed locally |
| Desktop/mobile settings selection and persistence work | Browser plus Vue runtime/build | passed locally |
| Existing behavior remains usable | Go regression/race/build/vet | passed locally |

## Failure and compatibility boundaries

The controls are prompt hints. Actual format, frames and duration depend on the
upstream; Codex currently requests a PNG image tool and may not produce GIF.
Existing SSE returns completed files, not incremental animation frames. Scope
changes are checked again before queued execution, without a new account attempt
for disallowed requests. Invalid requests retain redacted Call Records. Existing
account credentials, keys and image files remain with their current owners.

## Documentation and rollback

Update current public execution documentation, backend map and CHANGELOG.
Rollback the local source/binary; the additive Settings key is safe to ignore by
older versions. Local checks, user acceptance and publication are separate gates.

## Local verification evidence (2026-11-05)

Go race regression passed 253 top-level tests (1,278 including subtests), with
2 optional Redis tests skipped because no Redis endpoint was configured. Go vet
and native build passed. Vue production build and a focused Settings transport
runtime check passed. The full frontend runtime command could not run because
the checkout lacks its referenced ignored test files.

Browser inspection covered Dashboard, Monitor, Studio, Accounts, Logs, Gallery,
Proxy and Settings at 1280×900 and 390×844 in dark mode. Scope selection,
confirmation, save/reload, keyboard restoration and hiding the control for an
older backend projection passed against isolated local data. No console/HTTP
errors or document-level overflow were observed; small existing internal overflow
in Studio/Logs remains outside this change.

Two live local requests both returned animated GIFs with 9 frames at 200 ms per
frame, plus upstream PNG previews. The 2.6-flare generation used default Base64
and took 104.213 s; the 2.5 edit used URL response with scope `all` and took
115.683 s. Public and authenticated downloads matched the GIF bytes, and
revised_prompt retained the original prompt. Local temporary settings were
restored. These samples do not guarantee upstream format/frame adherence.

The initial local verification did not include publication or deployment.
The user subsequently authorized commit/push and sequential updates of the three
Go services, preserving existing data and backing up accounts only. User-visible
acceptance and deployment verification remain separate evidence gates.

## Output-format compatibility correction (2026-11-05)

The user requested removing the GIF-only output-format enum after a legacy
multipart edit was rejected at admission. Image endpoints now accept any string
without adding format conversion; exact lowercase `gif` retains its existing
scope and frame checks. Non-string formats and invalid frame controls remain
invalid. No storage, response-format or account-scheduling behavior changes.

Local verification passed 128 compatibility cases across 2.5/2.6, generation,
JSON/form/multipart edits and asynchronous tasks, including original prompt,
reference bytes and default Base64 delivery. The race suite and its environment
repair rerun together passed 254 top-level tests (1,407 including subtests), with
2 optional Redis tests skipped. The PostgreSQL snapshot test needed the existing
`pg_dump` added to the test process PATH; its targeted race rerun passed. Go vet
and native build passed. The user then authorized commit/push, GitHub verification
and sequential server updates after CI passes; deployment is a separate gate.

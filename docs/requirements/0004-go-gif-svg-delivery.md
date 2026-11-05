# 0004: Go GIF and SVG delivery

Status: pending-verification

- Owner: native image execution, ImageAssets and Web upstream adapter
- Created: 2026-11-05
- Scope: user authorized local implementation and tests

## Problem and evidence

Live prompt-driven Web output included a GIF stored under a PNG suffix. The
SVG request returned a PNG before the same upstream turn created a downloadable
SVG attachment. The public API did not collect that attachment.

## Scope and unique owners

ImageAssets owns actual format validation, suffix, MIME, save/read, gallery,
WebDAV and download behavior. Native image execution preserves GIF animation
and SVG vectors across optional upscaling. The upstream adapter waits for final
SVG attachments when the prompt explicitly requests SVG output, using the current
turn and the original account. Format intent must distinguish an explicit file
output instruction from SVG mentioned in poster copy or capability descriptions;
negative instructions must not force SVG delivery. Existing editable-file download resolution is
reused without introducing a second persistent file owner. Task projections
carry optional MIME; Vue consumes it when constructing data URLs.
Browser image downloads use the response MIME to correct legacy/default image
suffixes while retaining other file types' naming behavior.

## Boundaries

No new official output_format/frame-count controls or exact Image 2.6 selector.
No SVG reference-image input, raster-to-vector conversion, or new generation
on attachment recovery. Normal raster request completion remains unchanged.
Only bounded passive SVG documents are accepted; active content, external
resources, embedded raster images and CSS stylesheets are rejected. Existing
account, key, database and image ownership is unchanged. No migration, server
update, commit or push is included. Historical PNG-named files remain readable.

## Acceptance and verification

- PNG/JPEG/GIF/WebP bytes determine suffix and response MIME; SVG uses XML validation.
- GIF original frames survive upscaling and maintenance compression.
- Prompt-driven SVG waits past a raster preview, resolves a current-turn sandbox
  attachment, and publishes the SVG through existing image routes and tasks.
- No SVG result is reported when only PNG, stale branch, source code or pending
  attachment exists; timeout remains resumable on the same upstream account.
- Ordinary PNG/GIF output survives quoted/descriptive SVG mentions and negative
  SVG instructions. Real committed SVG attachments remain discoverable.
- Missing, empty, whitespace-only and non-string prompts cannot call upstream;
  authenticated input failures produce one credential-free Call Record with
  field diagnostics and prompt length, preserving public error status/envelopes.
- Same-origin downloads retain authentication; third-party URLs do not receive it.
- Direct and admin downloads, base64/SSE/chat outputs, WebDAV and gallery previews
  preserve content type; SVG responses carry restrictive browser headers.
- Tests cover transport sequences, rejected SVG payloads, old paths and actual
  GIF/SVG samples, plus Go regression/build and Vue adapter/build checks.

## Documentation and rollback

Update current Go execution/upstream docs and CHANGELOG. No ADR is required:
existing storage ownership and task lifecycle remain in force. Rollback reverts
the binary; older binaries do not expose newly saved GIF/SVG paths, so preserve
files and use a forward fix when those assets must remain accessible.

User acceptance, publication and native Image 2.6 identity remain separate gates.

## Local evidence (2026-11-05)

Focused transport/storage tests cover delayed SVG extraction, stale branches,
download-resolution failures without a new generation, hostile XML, WebDAV MIME,
legacy GIF compression, base64/SSE/task delivery and byte-identical downloads.
Real upstream samples also passed storage/read/download checks with upscaling
enabled. Browser checks cover desktop/mobile rendering, MIME contract validation
and byte-identical downloads with corrected suffixes. Go race regression and
Vue production build are recorded separately in local verification artifacts.

Two live local requests each reduced freshly synchronized account quota by one:
GIF took 50.551 seconds and produced a 411x411 animation with 9 frames at 200 ms,
plus the upstream PNG sheet; SVG took 33.802 seconds and returned one 1024x1024
SVG document with a 512x512 viewBox. Public reads and authenticated downloads
matched the saved bytes.
These samples establish attachment delivery, not exact native model identity or
guaranteed output dimensions. User acceptance and publication are still pending.

---
status: accepted
---

# Extend Go rehearsal with public execution and file-backed Image Tasks

The user authorized stage 3 of PRD 0001 after the control plane. Extend ADR 0006
with public protocol adapters and native execution, enabled only with an explicit
rehearsal data directory. Preserve the established User Key, Account Pool,
settings and proxy owners rather than creating parallel state or forwarding to
Python. Existing production routing remains unchanged.

The public adapter owns HTTP validation/auth, protocol envelopes and streaming.
The execution owner coordinates one request deadline, output slots, account
switches and leases. The upstream client owns one native conversation, uploads,
search or read-only polling operation. The Image Asset owner saves/reads through
one seam and commits compatible files/catalog metadata. Image Tasks consume that
execution seam and uniquely own admission, idempotency, progress, terminal state,
retention and restart interruption. Vue retains interaction/rendering ownership.

Actual executions append a final Call Record through the existing independent
Application Database owner. Explicit rehearsal schema initialization may create
the compatible Call Record tables; opening the server never migrates schemas.
Request credentials and image input bytes are excluded. Admission and idempotent
task replay do not count as executions; query/metric management remains stage 4.

Keep `image_tasks.json`, image files, `image_index.json`, tombstones and spools
outside the Application Database under ADRs 0003/0004. Use atomic JSON replacement
and compatible backups/locks. A second Go task worker owner on the same task file
must fail instead of recovering another live process's tasks. Do not replay a
queued/running task after process restart, and do not persist resubmittable request payloads, input bytes or credentials in
task records; retain compatible result metadata such as revised_prompt. Resume polling never resubmits generation.
Preserving a conversation ID across reload corrects the baseline loader gap;
record it as a deliberate compatibility-preserving recovery improvement.

## Consequences

- No dual writes or automatic import/cutover. Verification uses isolated files
  and databases, synthetic credentials and replaceable transport/execution seams.
- Downloads and signed uploads isolate credentials and constrain the actual
  connection address, redirects, response sizes and timeouts.
- Runtime errors and partial outcomes retain their protocol/task meaning.
  A URL, HTTP success, fixture or build alone is not image-delivery evidence.
- Full gallery mutation, Call Record/metric management, remaining public file
  APIs and packaging remain later-stage owners. They are not task JSON fields.
- Live account verification, publication and deployment are independent gates.

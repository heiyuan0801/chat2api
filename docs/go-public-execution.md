# Go public execution and durable Image Tasks

Status: current

For the subsequent authorized PostgreSQL/Redis live account and public-image
samples, consult the [live-verification guide](go-live-verification.md), including
model-specific failures and the authenticated same-origin download repair.

This is stage 3 of [PRD 0001](requirements/0001-go-backend-migration.md), extending
[the control plane](go-control-plane.md) under [ADR 0007](adr/0007-go-public-execution-and-file-task-ownership.md).
The stage-3 delivery registered 51 of the baseline's 112 method/path pairs.
[Stage 4](go-remaining-operations.md) now completes all 112 with an explicit
absolute data directory. Without it, the 39 stage-2
management routes remain available. Python production traffic and Vue source are
unchanged. No deployment or production data cutover is performed.

## Execution and contracts

The native adapters implement models, chat completions, Responses, Anthropic
Messages, image generations/edits and search. They reuse the existing User Keys,
settings, Account Pool and Proxy References. JSON validation and error envelopes,
text snapshots, token usage, SSE framing and terminal events follow Python
fixtures. Text cache entries are bounded and scoped to each protocol; cache
hits still produce a final call record. Streaming starts after the first event
or a controlled early error and follows request/server cancellation.

The native Client retains explicit resource limits, including 1 MiB prompt/text
content and 1024 messages. These limits are additional to request schema checks;
very large baseline requests are not claimed compatible.

The upstream client supports Web image uploads/edits, multimodal text, thinking
hints, Codex image-tool results, search and known-conversation polling. One
execution owner coordinates image slots, account attempts, egress leases, the
common deadline, quota/counters, masks, output sizing and asset saving. A fallback
proxy is eligible only for an early connection failure before a conversation;
polling and already-started generation are never replayed through that fallback.

Input references accept the existing JSON, form and multipart shapes. Remote
images use public-address validation, pinned dialing, bounded redirects and
bodies, without ambient proxy credentials. Signed uploads do not inherit the
ChatGPT bearer token/cookies. Admin-configured WebDAV and AI-review destinations
use their explicit configuration and selected default egress. These trusted
configuration destinations can be internal services; arbitrary input URLs cannot.

Image masks replace alpha, using the last mask for remaining inputs. The
configured `sharp_lanczos3` path can reuse the existing optional Node/Sharp
helper (`scripts/image_upscale/upscale.mjs`): Node must be on PATH and its
existing dependencies installed with `npm ci` in that directory. Set
`CHATGPT2API_SHARP_SCRIPT` to its absolute path when the binary runs
outside the source checkout; otherwise only nearby checkout/executable paths are
checked. Missing/failed Sharp falls back to native Lanczos; `pillow_lanczos` uses
that native path directly. Native fallback corrects EXIF orientation and retains
JPEG/WebP/PNG containers. Go JPEG quality 95 and lossless WebP encoding differ
from Pillow/libwebp quality 95; pixel/byte equivalence is not claimed. Resizing
is serialized with cancellable admission, matching the baseline worker bound.
Decoded
pixels (64 MP, at most 32768 per dimension), resize intermediates, input counts
and encoded bodies are bounded. Output image bytes are saved even when returned
as base64.

## Durable ownership

| Owner | Data and behavior |
| --- | --- |
| Application Database | Existing six control-plane tables plus compatible `call_records` and `call_record_state`; final execution records append independently |
| Image Tasks | `image_tasks.json`, same-snapshot `.bak`, private edit spools and a lifetime owner lock; owner-scoped idempotency, bounded queue/workers, progress and terminal projection |
| Image Assets | Existing dated files, `image_index.json`, per-item/index locks and remote-delete tombstones; local/WebDAV/both save and public registered-file read |
| Vue | Existing adapters, task polling, interaction state, rendering and layout |

Task admission reserves bounded capacity before edit materialization and rolls
back failed admission. Input bytes and resubmittable payloads remain in memory or
private temporary spools, not task JSON. Compatible result metadata may include
`revised_prompt`. Shutdown cancels workers; startup marks unfinished records
interrupted, retains partial results and never submits generation again. Resume
polling reads an existing conversation. Unlike the baseline loader, Go preserves
`conversation_id` across restart; this is an explicit recovery correction.
Under [ADR 0010](adr/0010-bind-image-recovery-to-origin-account.md), the same task
file also retains an optional internal `upstream_account_id`. It is a stable
management reference, never a credential or a public Vue/request field. Resume
acquires only that account's image/egress lease using current credentials;
exhausted generation quota or tool cooldown does not block reading existing work.
Deleted, disabled or unusable accounts fail without replacement. Legacy tasks
can recover an unambiguous origin from a bounded set of the same User Key's
conversation Call Records; absent/conflicting evidence fails without remote
probing. A resume's own poll deadline remains resumable; caller cancellation
remains cancellation. Python readers accept the optional task metadata, and
existing task projection/routes and SQL schemas are unchanged.

Catalog writes retain unknown top-level fields. Public reads require a registered
entry and honor its local/WebDAV flags; an OS-rooted file boundary rejects path
and symlink escapes. Save and gallery mutation remain one catalog seam, while
bulk synchronization, compression, deletion, thumbnails and cleanup are described
in the stage-4 guide.
Do not run Python and Go schedulers or cleanup workers on the same live directory.

Every actual public execution or task execution/resume appends one final Call
Record. Auth/schema rejection and idempotent task replay are not executions.
Request credentials and image input bytes are excluded. Shutdown closes call admission, cancels execution and waits within its deadline
for final record writes before database closure; write/shutdown failures are
reported. Logs/metrics queries, retained aggregate handling and live monitoring
remain stage 4.

## Isolated use

Build `cmd/control-server` in `go-backend`. Supply the existing explicit database
URL and bootstrap-key environment variables described in the control-plane guide,
plus an isolated absolute `-data-dir` or `CHATGPT2API_DATA_DIR`. Use `-init-schema`
only for an intentionally supplied rehearsal database; opening the server alone
does not create or migrate tables. A new database's explicit initialization also
creates the compatible Call Record tables. A second Go worker owner of the same
task file fails startup.

No real token file is needed for offline verification. A development fixture may
replace the upstream transport/executor while retaining the real routes,
repositories, task worker and asset writer. Such a fixture is not live-upstream
or production evidence.

## Verification and remaining gates

The final local verification passed:

| Gate | Observed result |
| --- | --- |
| Public contracts | 38 Python HTTP goldens, 56 BPE/token comparisons, protocol errors/streaming, cache/concurrency and redirects |
| Durable compatibility | Python ↔ Go task files, asset catalog/files and Call Records; SQLite and PostgreSQL 17.11 SQL round trips, owners, idempotency, restart, partial results and read-only resume |
| Images and processing | Decodable native PNG through save/read, URL/base64 and WebDAV; four Pillow mask-mode goldens; EXIF/format goldens and actual Sharp byte comparisons; bounded invalid/helper output and cancellation |
| Final call lifecycle | One final record per actual execution, panic/stream failures, cache replay, shutdown wait/deadline and late producer completion |
| Full stages 1–3 regression | `go test -count=1 -race -coverpkg=./internal/... -json ./...`: 596 leaf cases / 115 top-level tests, no failures; aggregate internal statement coverage 80.5% |
| Static/build | `go vet ./...`; both CLIs built without CGO for Darwin arm64 and Linux amd64; resize integration also compiled on declared minimum Go 1.25.0 |
| Real processes | Six checks: 51-route startup, controlled no-account error, durable task/final records, second-owner rejection, clean SIGINT and idempotent restart replay |
| Existing Vue | Build passed; browser login, one text stream and two image tasks passed on 1440×1000 and 390×844 viewports; task/asset reads survived a real Go process restart |

Browser output used a synthetic upstream with the real native executor, Account
Pool, task worker and asset writer. The delivered 512×384 PNG decoded and its
HTTP bytes matched the fixture. Eight console pages were visited at both widths
without document horizontal overflow; basic Accounts/Proxy/Settings loaded.
At that stage, Dashboard/Monitor/Logs/Gallery, prompt-library, update and
remote-import sections still exposed missing APIs. Stage 4 now implements and
checks these surfaces; this section retains the historical stage-3 evidence.

Per repository rules, local test harnesses, fixtures, logs and screenshots remain
ignored or outside source control. Automated and browser checks do not constitute
user acceptance, live-upstream verification, publication or deployment.

The remaining 61 rows, frameworks, gallery, editable files, observability, backups
and local packaging are now covered by the stage-4 guide. Vue runtime test source
files are missing from this clone. Live text,
Codex/image edit/search and authenticated cross-restart recovery, PostgreSQL 18,
production throughput, production rollback and user acceptance remain separate
gates. Earlier sampled stage-1 live image success does not verify stage 3.

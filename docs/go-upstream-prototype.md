# Go upstream communication prototype

Status: current

The independent [Go module](../go-backend/go.mod) provides an explicitly invoked
CLI. Python remains the production HTTP service; Vue and application data
formats remain unchanged. The same Go module now also contains a separately
invoked [stage-2 control plane](go-control-plane.md) for isolated rehearsal. The migration sequence
is owned by [PRD 0001](requirements/0001-go-backend-migration.md), and the public
HTTP checklist is owned by [the compatibility inventory](go-api-compatibility.md).

## Implemented boundaries

| Module | Owns | Does not own |
| --- | --- | --- |
| `cmd/upstream-probe` | One CLI operation, signal cancellation, prompt input, JSON/NDJSON output and exit status | Public API authentication or HTTP serving |
| `internal/upstream/transport.go` | Browser-profile-aware TLS transport, explicit HTTP/SOCKS proxy configuration, per-client cookies and identity, controlled error metadata | Account Pool scheduling or credential refresh |
| `internal/upstream/client.go` | Bootstrap, Sentinel prepare/finalize, text conversation, single-image preparation, polling and signed URL resolution | Public protocol conversion, image persistence or asset download |
| `internal/upstream/pow.go` | Script/build discovery, requirements token and bounded SHA3-512 proof generation | Turnstile or Arkose execution |
| `internal/upstream/turnstile.go` | Bounded port of the Python Turnstile VM, fractional register aliases, nested queues and baseline browser symbols | JavaScript execution, network/filesystem access or Arkose |
| `internal/upstream/sse.go` and `events.go` | Bounded SSE framing, answer snapshots/patches, trusted tool output pointers and stream terminal evidence | Complete Python failure taxonomy, usage accounting or public SSE formats |

The transport is pinned to `tls-client v1.16.0`, `fhttp v0.6.9`, and the module's
`go.sum`. Minimum declared Go version is 1.25.0 after adding the stage-2 SQL drivers. The prototype was built and
tested with the locally installed Go 1.27.2. Source/API references:
[tls-client](https://github.com/bogdanfinn/tls-client/tree/v1.16.0),
[Go HTTP](https://pkg.go.dev/net/http), and
[the Python upstream owner](../services/openai_backend_api.py).

The default TLS profile `chrome_110` and Edge/Chrome 143 HTTP identity reproduce
the Python baseline's defaults; their mismatch is inherited, not a verified
modern browser identity. `-profile` and `GO_UPSTREAM_USER_AGENT` allow deliberate
transport experiments. Selecting a different profile alone does not update
the baseline Sec-CH-UA headers or prove that upstream will accept the client.

## Build and CLI

From the repository root:

```sh
cd go-backend
go build ./...
go run ./cmd/upstream-probe -help
```

The default mode is `handshake`, which performs bootstrap and requirements only.
Generation happens only with an explicit `text` or `image` mode. A deliberately
supplied Upstream Account Access Token is read from `GO_UPSTREAM_ACCESS_TOKEN`;
this is not the application's local `CHATGPT2API_AUTH_KEY`. Use an environment
manager or a private shell session to provide it; never put it in a CLI argument
or committed file. No token is loaded from an existing application database.

```sh
go run ./cmd/upstream-probe -mode handshake -timeout 30s
go run ./cmd/upstream-probe -mode models -timeout 30s
go run ./cmd/upstream-probe -mode text -prompt 'Reply with OK' -model auto
go run ./cmd/upstream-probe -mode text -prompt 'Reply with OK' -stream
go run ./cmd/upstream-probe -mode image -prompt 'A small green tree on white' -model gpt-image-2
go run ./cmd/upstream-probe -mode image-poll -conversation-id '<existing-conversation-id>'
```

`models` lists diagnostic upstream slugs. It does **not** implement
`GET /v1/models` or change the configured Model Catalog. `text -stream` emits
NDJSON answer snapshots and then one final result, not OpenAI SSE. Snapshots
preserve upstream replacement operations; a future protocol adapter will own
public delta conversion and text annotation sanitization.

`image` supports one Web generation with `gpt-image-2` or the three baseline 2.5
compatibility names. It returns `conversation_id` and resolved signed asset URLs
without downloading or storing image bytes. These URLs are sensitive output:
do not paste probe output into logs, source files or a public report. Finding
tool metadata or an input attachment is never enough to claim image success.

`image-poll` resumes discovery for an existing conversation using the supplied
credential. It issues only GET requests for conversation/task/output resolution;
it does not run the handshake or submit another generation request. It does not
create a durable application Image Task.

| Configuration | Behavior |
| --- | --- |
| `GO_UPSTREAM_ACCESS_TOKEN` | Optional for text/handshake/models; required for image |
| `GO_UPSTREAM_PROXY` / `-proxy` | Explicit HTTP, HTTPS, SOCKS5 or SOCKS5H proxy; no inherited application routing |
| `GO_UPSTREAM_DEVICE_ID`, `GO_UPSTREAM_SESSION_ID` | Optional stable identity per Client; otherwise random UUIDs |
| `GO_UPSTREAM_USER_AGENT` | Optional User-Agent override |
| `-profile` | Named TLS client profile, default `chrome_110` |
| `-timeout` | Overall operation deadline, default 300 seconds, maximum 30 minutes |
| prompt flag or stdin | UTF-8 text, maximum 1 MiB |

Other library limits: 4 MiB nonstream responses, 1 MiB SSE event/line bound,
32 MiB complete SSE transport, at most 500,000 PoW attempts, a 90-second image
stream sub-deadline, 5-second initial image poll wait and 3-second poll interval.
All operations on one Client serialize admission under the caller's deadline.
A stream sub-timeout or incomplete image stream may continue through polling
only after a valid conversation ID was observed and the overall deadline remains.

Turnstile uses the original requirements `p` as its XOR key and forwards the
solution through finalize and the conversation header. Limits are 1 MiB for the
encoded challenge and individual values, 20,000 cumulative instructions, 64
nesting levels, 8,192 registers and a 16 MiB value-assignment work budget.
Cancellation and malformed/unsolved challenges return controlled errors before
generation. This is the Python baseline's symbolic VM, not a general browser
or a generic Cloudflare challenge solver.

Exit 0 means the selected operation returned a result. Runtime/validation errors
exit 1; malformed flag syntax exits 2. Stderr contains controlled JSON fields
`kind`, `stage`, optional HTTP status and Retry-After seconds, without raw
response bodies, request tokens or proxy credentials.

## Offline verification

```sh
python3 scripts/check_go_api_inventory.py
cd go-backend
test -f tests/upstream_test.go
test -f tests/transport_test.go
test -f tests/turnstile_test.go
go test -count=1 -race ./...
go vet ./...
go build ./...
```

The inventory checker imports only the Python standard library and scans ASTs;
it does not start the application or open data files. Its 112 matching entries
prove inventory coverage only, not public request/response parity.

Local transcript tests cover bootstrap/finalize and PoW, auth/anonymous routing,
text snapshots and patches, trusted tool pointer filtering, conversation/task
polling, signed URL resolution, stream limits, cancellation, challenge rejection,
and redacted HTTP errors. Real loopback integration covers TLS/HTTP2 negotiation,
Chrome-profile cipher ordering, cookie isolation, HTTP CONNECT proxying and
rejection of an untrusted certificate. Loopback fixtures use test certificates;
the production constructor provides no certificate-verification bypass.

Turnstile fixtures compare 19 deterministic scenarios directly with
`utils/turnstile.py`, including decimal aliases, nested captures, Unicode,
ordered keys and JSON/string conversion. Integration checks verify finalize
and conversation token forwarding, execution/cancellation limits, the quoted
SSE `"v1"` encoding marker, explicit `end_turn: false` and read-only image polling.

As required by this repository's Git rules, regression source remains in the
ignored local `go-backend/tests/` directory. A clean clone does not contain these
fixtures, and an empty `go test` run must not be reported as protocol validation.
The checks above explicitly require the local fixture files before testing.

## Live verification boundary

On 2026-10-20, user-authorized sampling of one supplied credential confirmed
Sentinel/Turnstile acceptance and a complete Go image-generation operation with
one resolved asset. That image and an earlier submitted image were recovered
with `image-poll`; both PNG files were downloaded, fully decoded with Go's image
decoder and visually checked against the test prompt. This verifies the sampled account and
flow, not every credential, model, quota state or future challenge variant.

The local verification helper used bounded HTTPS downloads, public destination
IP pinning, certificate verification, no redirects and a 32 MiB byte limit.
Same-origin ChatGPT content required account authentication, matching Python;
credentials were not sent to third-party asset origins. The host's incorrect
system DNS answers were avoided using an HTTPS DNS query for this verification;
no system DNS or proxy configuration was changed. This helper is not a deployed
Image Asset service or a general-purpose downloader in the Go library.

## Remaining compatibility gates

- Live text generation, other accounts/models and SOCKS routing remain untested.
  A successful sample does not establish production availability or all-account
  quota eligibility.
- Arkose remains unsupported, as in the Python baseline. Unsupported or unsolved
  challenges return explicit errors before generation.
- No image editing/uploads, Codex route, search, PPT/PSD, token refresh, billing
  usage, full image failure policy, account switching or persistent tasks yet.
- The Go library discovers/resolves assets; production asset downloads,
  persistence and the ImageStorageService contract are still unimplemented.
  Downloading a sample through a local verifier does not close those gates.
- No application database is opened and no application configuration, task or
  image index is written. Only explicitly generated verification files are
  saved outside the repository. No Python/Vue application behavior is replaced.
- No frontend build/browser acceptance, CI publication, remote push or deployment
  is claimed by the prototype checks.

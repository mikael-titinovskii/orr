# Management REST API v1

`orr serve` hosts the management API on the proxy listener, with or without the
dashboard. `orr serve --no-tui` disables the dashboard and overrides `ORR_TUI`.
OpenRouter traffic remains under `/v1/*`; management lives under `/api/v1/*`.

Local management requests require no authentication header. ORR reads
`OPENROUTER_API_KEY` through its existing environment/dotenv configuration for
upstream operations; clients do not resend it. Responses use JSON and
`Cache-Control: no-store`. No CORS access is granted. The management API is
accessible to anyone who can reach `ORR_LISTEN`; keep the default loopback address
for local use.
`/health` remains the existing public liveness endpoint.

Public `GET`/`HEAD /api/docs` serves a searchable API reference with curl examples.
Public `GET`/`HEAD /api/openapi.json` serves the OpenAPI 3.1 schema for client import.
Both are embedded in the executable and work without a CDN or authentication.
The reference page only fetches the schema; it does not execute API operations.

## Resources

| Method | Path | Result |
| --- | --- | --- |
| GET | `/api/v1/stats` | Request counts, in-flight count, daily USD costs/token totals, and recent latency/throughput aggregates. |
| GET | `/api/v1/models` | `{ "models": [...] }`, sorted by model ID. |
| GET | `/api/v1/model?model=author/model` | One model, provider order, available provider tags, active provider, and manual pin. |
| PUT | `/api/v1/pin?model=author/model` | Set a manual pin with `{ "provider": "tag" }`; returns the model. |
| DELETE | `/api/v1/pin?model=author/model` | Clear only the manual override, restoring automatic selection; returns the model. |
| POST | `/api/v1/refresh?model=author/model` | Start a provider-order refresh job. No body. |
| POST | `/api/v1/benchmarks?model=author/model` | Benchmark the model's providers and apply existing ranking rules. No body. |
| GET | `/api/v1/jobs/{id}` | Read job status. |

Models must already be known to the running proxy. Model identifiers are query
parameters because model IDs and provider tags can contain slashes. Pin providers
must appear in the model's current provider list. Manual pins are persisted only
to `providers.yaml`; the dotenv file is never changed. Refreshes preserve manual
pins; benchmarks retain revision checks and automatic-pin persistence rules.

Model objects contain `id`, `order`, `providers`, `active_provider`, and
`manual_pin`. Lists are empty arrays when empty; absent pins/providers are null.
Stats fields are `request_count` (persisted total), `session_request_count`,
`in_flight`, `daily_costs_usd`, `daily_tokens`, `average_ttft_ms`,
`average_tokens_per_second`, `latency_p50_ms`, and `latency_p95_ms`. Aggregates
use the existing bounded recent-record snapshot; missing measurements are null.
Known counts may be zero. No keys, prompts, request bodies, or response bodies
are included.

## Jobs and errors

Job submissions return 202, a `Location` polling URL, and a job object containing
`id`, `kind`, `model`, `status`, `created_at`, `finished_at`, and `error`.
Timestamps are UTC RFC3339; unfinished timestamps and absent errors are null.
States are `running`, `succeeded`, and `failed`. Only one management job runs at
a time; duplicate submissions for the same model/kind return that running job,
and other submissions return 409. The last 64 jobs are retained in memory;
completed jobs are evicted oldest first and all jobs disappear on restart.
An unknown/evicted job returns 404. Shutdown closes job admission and waits for
admitted jobs before the final stats save. Benchmarks use the existing manual
10-second deadline and at most four concurrent provider tests; they make billable upstream
requests. Partial benchmark results may update routing, but incomplete or wholly
unsuccessful runs report failure. Refresh failures and stale revision conflicts
report failure. Refresh requests have a 30-second timeout. Jobs are not canceled
when the submitting client disconnects.

Errors have the existing envelope `{ "error": { "message": "...", "type":
"orr_error" } }`. Status codes: 400 invalid input,
404 unknown resource/model, 405 unsupported method (with `Allow`), 409 job
conflict, 413 body over 4 KiB, 415 non-JSON pin body, 500 persistence failure,
503 shutdown. Pin bodies reject unknown fields and trailing JSON. Internal or
upstream error details are not exposed.

```sh
curl 'http://127.0.0.1:8787/api/v1/model?model=moonshotai/kimi-k3'
curl -X PUT \
  -H 'Content-Type: application/json' -d '{"provider":"fireworks"}' \
  'http://127.0.0.1:8787/api/v1/pin?model=moonshotai/kimi-k3'
```

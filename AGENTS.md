# Repository guide for coding agents

## Layout

- `cmd/orr`: the executable entry point; keep it thin.
- `internal/app`: CLI commands, configuration, proxy, metrics, routing state, and TUI.
- `internal/store`: the SQLite database — schema migrations, row types, and the one-time legacy import.
- `e2e`: end-to-end proxy, lifecycle, routing, and CLI tests.
- Repository root: user documentation, the dotenv example, installers, the Docker build file, and Go module files.

## Working conventions

- Run `gofmt` on changed Go files.
- Run `go test ./...` and `go vet ./...` before handing off Go code changes.
- Keep API keys, prompts, and request/response bodies out of logs and persisted stats.
- Preserve streaming behavior: response inspection must be passive and bounded.
- Never rewrite `.env` from the dashboard. Persist manual provider pins in the database's `models.manual_pin`; keep automatic pins in its `provider_pins` table.
- The database schema is versioned by goose migrations under `internal/store/migrations`; add a new numbered migration instead of editing an applied one.
- Provider-order updates must preserve `manual_pin` and retain that provider in the model's refreshed `order`.
- Provider failure recovery applies only to automatic pins. Keep the 429 threshold configurable, immediately retry an attributable pre-stream 404 when a single selected endpoint no longer serves the model, and coalesce recovery benchmarks per model.
- Degradation recovery also applies only to automatic pins. Compare a provider with its own earlier client traffic — never benchmark pings, never the field — and run only the bounded refresh+benchmark path, with a cooldown separate from provider failure recovery. The `d` toggle persists in the database's `state` row; `.env` only seeds it.
- Do not add migration or backward-compatibility behavior unless explicitly requested; reject unsupported persisted formats. The one exception is the one-time `stats.json` import in `internal/store/legacy.go`.
- Assume a valid OpenRouter API key is always configured for supported runtime use. The application does not support keyless operation; do not add unauthenticated fallbacks.
- Maintain macOS, Linux, and Windows compatibility; do not depend on Unix-only terminal handling. The SQLite driver must stay pure Go (`modernc.org/sqlite`) because `Dockerfile` builds with `CGO_ENABLED=0`.
- The installers and `orr upgrade` build with Docker when Go is unavailable. Keep `.dockerignore` an allowlist so `.env`, the database, and `.git` never enter the build context.
- Leave missing data blank in dashboards and CLI tables. Do not use `0`, `0%`, `0.0%`, `-`, or `—` as no-data placeholders; an empty cell is fine. Keep zero API/tool error rates blank as well.

## Documentation

- Update `README.md` for user-visible behavior.
- Update `.env.example` only when environment-variable configuration changes. Comments there must directly describe the adjacent variable.

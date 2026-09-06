# Repository guide for coding agents

## Layout

- `cmd/orr`: the executable entry point; keep it thin.
- `internal/app`: CLI commands, configuration, proxy, metrics, routing state, and TUI.
- `e2e`: end-to-end proxy, lifecycle, routing, and CLI tests.
- Repository root: user documentation, the dotenv example, installers, and Go module files.

## Working conventions

- Run `gofmt` on changed Go files.
- Run `go test ./...` and `go vet ./...` before handing off Go code changes.
- Keep API keys, prompts, and request/response bodies out of logs and persisted stats.
- Preserve streaming behavior: response inspection must be passive and bounded.
- Never rewrite `.env` from the dashboard. Persist manual provider pins only in the Git-ignored `providers.yaml`; keep automatic pins in the platform stats file.
- Keep the shareable providers schema versioned; use `providersFileVersion` as the source of truth.
- Provider-order updates must preserve `manual_pin` and retain that provider in the model's refreshed `order`.
- Provider failure recovery applies only to automatic pins. Keep the 429 threshold configurable, immediately retry an attributable pre-stream 404 when a single selected endpoint no longer serves the model, and coalesce recovery benchmarks per model.
- Do not add migration or backward-compatibility behavior unless explicitly requested; reject unsupported persisted formats.
- Assume a valid OpenRouter API key is always configured for supported runtime use. The application does not support keyless operation; do not add unauthenticated fallbacks.
- Maintain macOS, Linux, and Windows compatibility; do not depend on Unix-only terminal handling.
- Leave missing data blank in dashboards and CLI tables. Do not use `0`, `0%`, `0.0%`, `-`, or `—` as no-data placeholders; an empty cell is fine. Keep zero API/tool error rates blank as well.

## Documentation

- Update `README.md` for user-visible behavior.
- Update `.env.example` only when environment-variable configuration changes. Comments there must directly describe the adjacent variable.

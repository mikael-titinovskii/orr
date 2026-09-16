# orr

`orr` is a local OpenRouter proxy. It keeps the model selected by your client
and applies provider preferences from `providers.yaml` to generation requests.

It gives you provider routing, manual pins, live performance and spend data, and
a terminal dashboard without recording prompts or API keys.

`orr` is an independent project and is not affiliated with or endorsed by
OpenRouter. OpenRouter is a trademark of OpenRouter, Inc.

Process environment variables override values from the dotenv file by default.
Use `--prefer-env-file` when the selected file should win for variables it
defines:

```sh
orr serve --prefer-env-file
orr update --env ./local.env --prefer-env-file
```

## Dashboard

![orr dashboard](docs/assets/dashboard.png)

An interactive terminal opens the dashboard; redirected output uses plain logs.
Set `ORR_TUI=false` to always use plain logs.

The useful controls are:

| Key | Action |
| --- | --- |
| `↑` / `↓` | Select a provider. |
| `Enter` | Toggle a manual pin for the selected provider. |
| `Tab` | Switch between recently used models. |
| `r` | Refresh and rank providers for the current model. |
| `t` | Benchmark the selected provider. |
| `a` | Benchmark every listed provider. |
| `s` | Start or stop the spend stopwatch. |
| `q` / `Ctrl+C` | Stop the proxy cleanly. |

The provider table includes OpenRouter's published `GPQA` Diamond and `Tau`
Airline benchmark scores, plus `Tool` and `Json` error rates. Benchmark
scores use OpenRouter's rolling 32-day window; error rates match the website's
one-week average of available daily percentages. `mApi` and `mTool`
continue to show errors observed by this proxy.
The legend above the table identifies token rates, latency in milliseconds, OpenRouter benchmarks, and
weekly average OpenRouter tool/JSON error rates, and measured API/tool error rates.
`Ts` and `Lat` show OpenRouter's throughput and latency; `mTs` and `mLat`
show measurements from this proxy.
The columns after `TTFT` are `GPQA`, `Tau`, `Tool`, `Json`, `mApi`, and
`mTool`. `GPQA` and `Tau` percentages are colored on the
same red–green–teal scale as throughput: higher scores are better, relative to
the listed providers for that model. The provider table shows the cache-read
price in `Cache`; cache-hit percentages are shown in the request log.
The table scrolls with provider selection to keep the selected provider's
metrics visible within the pane.
`mApi`, `mTool`, `Tool`, and `Json` use pale red for the lowest nonzero rates, increasing
to the current error red for the highest rates, comparing each column
independently. Equal nonzero rates use the strongest red.

These metrics load in the background, refresh every 15 minutes, and refresh
again after `r`. Missing data stays blank, and zero error rates stay blank.
Scores are matched to exact provider endpoints; ambiguous deployments stay
blank. The data comes from OpenRouter's website API, which can change separately
from its documented API. Fetch failures leave unavailable fields blank. The
metrics are displayed in memory and do not change provider rankings or pins.
See [OpenRouter's Auto Exacto documentation](https://openrouter.ai/docs/guides/routing/auto-exacto)
for how the benchmarks and tool-call errors are measured.

Manual pins stay until removed. Automatic pins select the best measured
provider, expire after `ORR_PIN_TTL` (one hour by default), and can fail over
after provider-specific 429s or an unavailable endpoint.

> A valid OpenRouter API key is required. `orr` does not support keyless use.

## Quick start

Install from the public repository (Git and Go 1.23+ are required):

```sh
git clone https://github.com/mikael-titinovskii/orr.git
cd orr
./install.sh
orr serve
```

On Windows PowerShell:

```powershell
git clone https://github.com/mikael-titinovskii/orr.git
Set-Location orr
.\install.ps1
orr serve
```

The installer asks for `OPENROUTER_API_KEY`, builds `orr`, and adds it to
`PATH` where needed. Set `ORR_OPENROUTER_API_KEY` beforehand for unattended
installs. It also configures installed Kimi Code and OpenCode clients when it
finds them.

Point any OpenAI-compatible client to:

```text
http://127.0.0.1:8787/v1
```

## Configure routing

Keep secrets and runtime options in `.env`; keep provider order and manual pins
in `providers.yaml`. Both are ignored by Git.

```dotenv
# .env
OPENROUTER_API_KEY=sk-or-...
ORR_LISTEN=127.0.0.1:8787
ORR_TUI=true
```

```yaml
# providers.yaml
version: 1
models:
  moonshotai/kimi-k3:
    order:
      - fireworks
      - deepinfra
    allow_fallbacks: false
```

The model key is its exact OpenRouter ID. Providers are tried in order.
`allow_fallbacks: false` prevents OpenRouter from using providers outside this
list. `orr` creates an empty entry for a new model and refreshes compatible
providers automatically.

Keep `ORR_LISTEN` on `127.0.0.1` unless you deliberately want network access.

## Everyday commands

| Command | Purpose |
| --- | --- |
| `orr serve` | Start the local proxy and dashboard. |
| `orr providers <model>` | List compatible OpenRouter providers. |
| `orr update` | Refresh provider orders for every known model. |
| `orr update --cache-only` | Keep only providers with prompt caching. |
| `orr reset` | Remove generated provider and stats state; preserves `.env`. |
| `orr upgrade` | Pull or download the latest source and rebuild the running persistent binary. |
| `orr completion <shell>` | Print completion setup for bash, zsh, fish, or PowerShell. |

The dashboard's release check accounts for release tags already included in the
running build's Git revision. When using `go run`, it checks the current checkout.
`go run ./cmd/orr upgrade` is rejected because it would replace a temporary Go
executable. Update that checkout with `git pull --ff-only`, then restart
`go run ./cmd/orr serve`. To use self-upgrades, first build a persistent binary
with `go build -o orr.exe ./cmd/orr` on Windows (`go build -o orr ./cmd/orr` on
macOS/Linux), and run that binary instead.

Use a different dotenv file when needed:

```sh
orr serve --env ./local.env
orr update --max 3 --env ./local.env
```

## Integrations

`orr integrate` updates Kimi Code and OpenCode after either client is installed.
You can also change only the OpenRouter base URL yourself.

Kimi Code uses the standalone `kimi` command and `~/.kimi-code/config.toml`
(or `$KIMI_CODE_HOME/config.toml` when that environment variable is set).
`orr integrate` creates missing configs for installed clients. A new or empty
Kimi config gets an OpenRouter provider with the configured `OPENROUTER_API_KEY`
and `moonshotai/kimi-k3` as its default model, with a 1,048,576-token context window.
Existing Kimi configs keep their model selections and provider protocols; only
OpenRouter providers are patched. Anthropic providers use the proxy's host root
because their SDK adds `/v1/messages`; OpenAI-compatible providers use `/v1`.
If none exists, add OpenRouter using Kimi's
`/provider` command, select a model, then rerun `orr integrate`.

For an existing OpenRouter provider, its endpoint should be:

```toml
[providers.openrouter]
type = "openai"
base_url = "http://127.0.0.1:8787/v1"
```

The old Python Kimi CLI's `~/.kimi` directory and `KIMI_SHARE_DIR` are not targeted.
Unsupported OpenRouter provider protocols, including `openai_legacy`, `google-genai`,
and `vertexai`, are rejected without changing the file; current Kimi Code uses
`openai` for Chat Completions. Existing files are
backed up to `<config path>.orr-backup` before changes.

OpenCode (`~/.config/opencode/opencode.json`):

```json
{
  "provider": {
    "openrouter": {
      "options": { "baseURL": "http://127.0.0.1:8787/v1" }
    }
  }
}
```

Keep the client’s existing OpenRouter key and model selection. `orr` only adds
routing preferences for each selected model.

## Runtime options

| Variable | Default | Purpose |
| --- | --- | --- |
| `ORR_TUI` | `true` | Enable the interactive dashboard. |
| `ORR_PIN_TTL` | `1h` | Lifetime of automatic pins. |
| `ORR_429_FAILOVER_THRESHOLD` | `2` | Consecutive provider 429s before automatic failover. |
| `ORR_LOG_REQUESTS` | `true` | Emit completed request lines in plain-log mode. |
| `ORR_UPDATE_MAX_PROVIDERS` | `20` | Maximum providers kept after a refresh. |
| `ORR_UPDATE_CACHE_ONLY` | `false` | Prefer only prompt-cache-capable providers. |

`OPENROUTER_API_KEY` also enables benchmarks, usage tracking, and account
metrics. Client requests still use the client’s own authorization header.

## Provider selection

`orr` compares healthy, tool-capable providers using estimated request cost and
measured response time. It favors the lowest cost-delay tradeoff, avoids
unusably slow choices, and keeps a small margin to prevent pin churn. Manual
pins always win.

Benchmarks make real, billable OpenRouter requests. The dashboard stores stats,
automatic pins, and benchmark results outside the repository; `orr reset`
removes that generated state.

## Development

```sh
go test ./...
go vet ./...
```

The end-to-end suite uses local mock upstreams and does not need a real OpenRouter
key or network access.

## License

This project is licensed under [Apache-2.0](LICENSE).

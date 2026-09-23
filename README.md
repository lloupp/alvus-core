# Alvus Core

Alvus Core is a reliability-first local inference gateway for OpenAI-compatible providers. Agents and IDEs talk to one local endpoint while Alvus Core handles provider credentials, model aliases, fallback, circuit breaking and protocol translation.

## Current capabilities

- OpenAI-compatible `/v1/*` proxy with model rewriting.
- Provider adapters for generic OpenAI, NVIDIA NIM, OpenRouter, Groq and Together.
- Provider-specific error classification instead of treating key, model and provider failures as the same thing.
- Per-provider credential pools with round-robin selection, cooldown and invalid-key disable.
- Ordered model routes, model-level circuit breakers and optional per-model attempt timeouts.
- OpenAI SSE streaming passthrough.
- Anthropic Messages compatibility at `POST /v1/messages` including text, tools, tool results, tool choice and SSE tool-call translation.
- `POST /v1/messages/count_tokens` transport-level token estimate for Anthropic-compatible clients. It is intentionally not presented as tokenizer-exact because providers do not share one tokenizer.
- Transactional hot reload: a new configuration is fully loaded and validated before an atomic state swap. Invalid reloads leave the previous runtime untouched, and in-flight requests continue on their captured state.
- Request body limits, upstream timeouts, redirect refusal and hop-by-hop header filtering.
- Optional proxy and admin authentication.
- `/healthz`, `/readyz`, `/metrics` and `/v1/models`.
- JSON structured logs.
- Optional bounded in-memory response cache, scoped to selected provider kinds (NVIDIA by default).
- Cache hit/miss/store metrics without persisting prompts, responses or credentials to disk.
- Race-detector tests in CI.

## Configuration

Copy the example:

```bash
cp config.example.json alvus.json
```

Keep credentials in environment variables, not in the JSON file. The default example is NVIDIA-only:

```bash
export NVIDIA_API_KEYS="nvapi-one,nvapi-two"
```

The gateway still supports OpenAI-compatible, OpenRouter, Groq and Together providers when you add them to your own configuration.

Then run:

```bash
go run ./cmd/alvus-core -config alvus.json
```

By default the process watches the JSON file every second. Saving a valid configuration atomically replaces the routing state; saving an invalid configuration is rejected without disrupting the active state. Changing `listen` requires a restart because the listening socket itself cannot be atomically moved.

Disable file watching with:

```bash
go run ./cmd/alvus-core -config alvus.json -watch=false
```

### Precedence

```text
Environment variables > JSON configuration > built-in defaults
```

Alvus Core never clears or rewrites the process environment while reloading.

Global overrides:

```text
ALVUS_CONFIG
ALVUS_LISTEN
ALVUS_PROXY_TOKEN
ALVUS_ADMIN_TOKEN
ALVUS_BODY_LIMIT_BYTES
ALVUS_REQUEST_TIMEOUT
```

`request_timeout` is the total budget available while Alvus Core selects/falls back between upstream models. A model can define a smaller `attempt_timeout`; the remaining total request budget always wins if it is smaller. For streaming, the attempt timeout bounds the wait for upstream response headers and does not cap an already-started stream.

Provider credentials are resolved through each provider's `api_key_env` field.

### Response cache

The response cache is opt-in and disabled by default. The built-in defaults target NVIDIA providers only, keep entries in process memory, expire them after one hour, cap the cache at 256 entries / 64 MiB total body bytes, and skip individual responses larger than 1 MiB.

```json
{
  "cache": {
    "responses": {
      "enabled": true,
      "ttl": "1h",
      "max_entries": 256,
      "max_body_bytes": 1048576,
      "max_bytes": 67108864,
      "provider_kinds": ["nvidia"]
    }
  }
}
```

Only successful non-streaming `/v1/chat/completions` responses are eligible. Requests that expose tool/function calling fields bypass the response cache, preventing cached tool calls from replaying agent actions. Cache keys include provider, resolved upstream model, HTTP method, path/query and the fully patched request body, so model defaults and request parameters participate in identity. Streaming remains pass-through. Entries are discarded on process restart or configuration reload; no prompts, responses, hashes or provider credentials are written to disk.

Operational counters are exposed through `/metrics`: `cache_hits`, `cache_misses`, `cache_stores`, `cache_entries`, `cache_bytes`, `cache_hit_rate`, and `response_cache_on`. The same endpoint exposes per-model attempts, successes, failures, timeouts, fallbacks, success rate, average/last latency, last HTTP status and last failure reason under `models`.

## NVIDIA quality routes

The default NVIDIA-only example exposes task-oriented routes:

- `quality`: Kimi-K3 → GLM-5.3 → Nemotron 3 Ultra → Nemotron 3 Super → Nemotron 3.5 Lightning.
- `coding`: Nemotron 3 Ultra → Nemotron 3 Super → Kimi-K3 → GLM-5.3 → Nemotron 3.5 Lightning.
- `reasoning`: Nemotron 3 Super → Nemotron 3 Ultra → GLM-5.3 → Kimi-K3 → Nemotron 3.5 Lightning.
- `fast`: Nemotron 3 Super → Nemotron 3 Ultra → Nemotron 3.5 Lightning.
- `vision`: Kimi-K3 → GLM-5.3 Flash.
- `auto` / `default`: Nemotron 3 Super → Nemotron 3 Ultra → GLM-5.3 → Kimi-K3 → Nemotron 3.5 Lightning.

The default example now favors measured backend responsiveness for general agent traffic while keeping slower frontier models available in `quality` and later fallbacks. GLM-5.3 Flash remains outside general routes after isolated live validation produced only one useful HTTP 200 in eight attempts, with ~146 s latency plus a 240 s timeout. Per-model attempt budgets in the example prevent a single unhealthy or slow candidate from consuming the entire fallback window.

Model entries can define `params`. These are applied as defaults after routing, while explicit client parameters win. This lets Alvus Core request a model's preferred reasoning mode without forcing Pi Agent, Claude-compatible clients or OpenAI-compatible clients to know provider-specific knobs.

A client can simply use `model: "auto"`, `"coding"`, `"reasoning"`, `"fast"` or `"vision"`. Capacity and NVIDIA model-availability failures move to the next candidate without poisoning otherwise healthy credentials.

## Anthropic-compatible clients

Point an Anthropic Messages client at the local server and send requests to:

```text
POST http://127.0.0.1:3000/v1/messages
```

Alvus Core converts Anthropic message blocks/tools into OpenAI chat-completions format, routes the request, and translates the response back. Text streaming and streamed tool calls are translated into Anthropic SSE events.

`/v1/messages/count_tokens` is a conservative size estimate rather than a provider tokenizer result. It exists to keep clients operational across heterogeneous upstreams; exact accounting should be added through provider tokenizer adapters when available.


## Live NVIDIA smoke test

For a real credential and streaming check, keep temporary keys only in the environment:

```bash
export NVIDIA_API_KEYS="nvapi-first,nvapi-second"
./scripts/e2e-nvidia.sh
```

The harness tests both keys independently with a non-streaming request and an SSE streaming request. It never prints or writes the keys. Its default model is `nvidia/nemotron-3-super-120b-a12b`; override it with `NVIDIA_E2E_MODEL` if needed.

For an isolated benchmark through Alvus Core itself, request a direct model alias so no route fallback can hide the model's own latency or failures. For GLM-5.3 Flash:

```bash
ALVUS_BENCH_MODEL=glm53_flash \
ALVUS_BENCH_RUNS=5 \
ALVUS_BENCH_STREAM_RUNS=3 \
python3 scripts/benchmark-alvus-route.py
```

The benchmark records useful/empty HTTP 200 responses, exact prompt compliance, total latency, streaming TTFT and completion of the SSE stream. It uses the local gateway at `http://127.0.0.1:3000/v1` by default and never reads provider credentials.


## Security

The default listener is loopback. Set `ALVUS_PROXY_TOKEN` before exposing `/v1/*` outside a trusted host. Incoming client authorization is never forwarded as the provider credential; Alvus Core injects the selected provider key after routing.

Protect `/metrics` with `ALVUS_ADMIN_TOKEN` when operational data should not be public.

## Development

```bash
gofmt -w .
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build -trimpath -o alvus-core ./cmd/alvus-core
```

The Go version is defined once in `go.mod`; CI reads the same file.

## Architecture

```text
client
  -> gateway
      -> protocol adapter (OpenAI / Anthropic)
      -> router / model circuit breaker
          -> provider adapter
              -> credential pool
                  -> upstream provider
```

The key design rule is that credential health, model capacity and provider health are separate facts.

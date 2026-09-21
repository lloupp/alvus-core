# Alvus Core

Alvus Core is a reliability-first local inference gateway for OpenAI-compatible providers. Agents and IDEs talk to one local endpoint while Alvus Core handles provider credentials, model aliases, fallback, circuit breaking and protocol translation.

## Current capabilities

- OpenAI-compatible `/v1/*` proxy with model rewriting.
- Provider adapters for generic OpenAI, NVIDIA NIM, OpenRouter, Groq and Together.
- Provider-specific error classification instead of treating key, model and provider failures as the same thing.
- Per-provider credential pools with round-robin selection, cooldown and invalid-key disable.
- Ordered model routes and model-level circuit breakers.
- OpenAI SSE streaming passthrough.
- Anthropic Messages compatibility at `POST /v1/messages` including text, tools, tool results, tool choice and SSE tool-call translation.
- `POST /v1/messages/count_tokens` transport-level token estimate for Anthropic-compatible clients. It is intentionally not presented as tokenizer-exact because providers do not share one tokenizer.
- Transactional hot reload: a new configuration is fully loaded and validated before an atomic state swap. Invalid reloads leave the previous runtime untouched, and in-flight requests continue on their captured state.
- Request body limits, upstream timeouts, redirect refusal and hop-by-hop header filtering.
- Optional proxy and admin authentication.
- `/healthz`, `/readyz`, `/metrics` and `/v1/models`.
- JSON structured logs.
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

Provider credentials are resolved through each provider's `api_key_env` field.

## NVIDIA quality routes

The default NVIDIA-only example exposes task-oriented routes:

- `quality`: Nemotron 3 Ultra → Kimi-K3 → Nemotron 3 Super → GLM-5.3 → Nemotron 3.5 Lightning → GLM-5.3 Flash.
- `coding`: Nemotron 3 Ultra → Kimi-K3 → Nemotron 3 Super → Nemotron 3.5 Lightning → GLM-5.3 → GLM-5.3 Flash.
- `reasoning`: Nemotron 3 Super → GLM-5.3 → Nemotron 3 Ultra → Kimi-K3 → Nemotron 3.5 Lightning → GLM-5.3 Flash.
- `fast`: Nemotron 3 Super → Nemotron 3 Ultra → Nemotron 3.5 Lightning → GLM-5.3 → Kimi-K3 → GLM-5.3 Flash.
- `vision`: Kimi-K3 → GLM-5.3 Flash.
- `auto` / `default`: Nemotron 3 Super → Nemotron 3 Ultra → Kimi-K3 → GLM-5.3 → Nemotron 3.5 Lightning → GLM-5.3 Flash.

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

# Alvus Core

Alvus Core is a reliability-first local inference gateway. It presents an OpenAI-compatible endpoint to agents and IDEs while routing requests across models, providers, and API credentials behind the scenes.

This repository is a clean redesign of the original Alvus idea. The first milestone focuses on correctness, security boundaries, model-level circuit breaking, explicit configuration precedence, bounded request memory, and testable routing.

## What already works

- OpenAI-compatible `/v1/*` proxy for JSON requests that contain a `model`.
- Model aliases and ordered fallback routes.
- Multiple OpenAI-compatible providers.
- Per-provider credential pools with round-robin selection.
- Model-level circuit breakers separated from credential health.
- 429/502/503/529 fallback behavior and `Retry-After` support.
- 401 credential disable behavior.
- Context-length errors skip only the incompatible model for that request.
- SSE/chunked streaming passthrough.
- Configurable request-body limit instead of unbounded `io.ReadAll`.
- Optional proxy authentication.
- `/healthz`, `/readyz`, `/metrics`, and `/v1/models`.
- JSON structured logs.
- `go test -race`, vet, format and build in CI.

## Configuration

Copy the example:

```bash
cp config.example.json alvus.json
```

Keep keys in environment variables, not in the JSON file:

```bash
export NVIDIA_API_KEYS="nvapi-one,nvapi-two"
export OPENROUTER_API_KEYS="sk-or-one"
```

Then run:

```bash
go run ./cmd/alvus-core -config alvus.json
```

By default the example listens only on `127.0.0.1:3000`.

### Precedence

Configuration is deterministic and never mutates the process environment:

```text
Environment variables > alvus.json > built-in defaults
```

Global overrides currently supported:

```text
ALVUS_CONFIG
ALVUS_LISTEN
ALVUS_PROXY_TOKEN
ALVUS_ADMIN_TOKEN
ALVUS_BODY_LIMIT_BYTES
ALVUS_REQUEST_TIMEOUT
```

Provider keys are resolved through each provider's `api_key_env` field.

## Routes

The example configuration exposes `auto`:

```json
"routes": {
  "auto": ["kimi", "deepseek"]
}
```

A client can therefore use:

```json
{
  "model": "auto",
  "messages": [{"role":"user","content":"hello"}]
}
```

If `kimi` is rate-limited or temporarily unavailable, the router can move to `deepseek` without treating healthy API credentials as broken.

## Security model

The default listen address is loopback. Set `ALVUS_PROXY_TOKEN` if clients should authenticate to `/v1/*`; the incoming token is never forwarded upstream. Provider credentials are injected only after routing.

`/metrics` can be protected independently with `ALVUS_ADMIN_TOKEN`, supplied as `X-Alvus-Admin-Token`.

Do not expose an unauthenticated listener on an untrusted network.

## Development

```bash
gofmt -w .
go vet ./...
go test -race ./...
go build ./cmd/alvus-core
```

Go version is defined once in `go.mod`; CI reads that same file.

## Architecture

```text
client
  -> gateway
     -> router / model circuit breaker
        -> provider
           -> credential pool
              -> upstream OpenAI-compatible API
```

The separation is intentional: model/provider capacity failures and credential failures are different facts and should not poison each other.

## Next milestones

- Provider adapters for NIM, OpenRouter, Groq and Together-specific error semantics.
- Transactional hot reload with validation before atomic config swap.
- Anthropic Messages API translation.
- OpenAI Responses API normalization.
- Prometheus/OpenTelemetry export.
- Weighted and latency-aware routing.
- Disk-backed replay for very large request bodies instead of keeping them entirely in RAM.
- Release matrix with checksums and SBOM.

## License

MIT.

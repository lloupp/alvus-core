# Security Policy

Alvus Core handles upstream API credentials and should be treated as security-sensitive infrastructure.

## Defaults

- Bind to loopback unless remote access is intentional.
- Keep provider keys in environment variables referenced by `api_key_env`.
- Use `ALVUS_PROXY_TOKEN` before exposing `/v1/*` beyond a trusted host.
- Use `ALVUS_ADMIN_TOKEN` to protect operational metrics.
- Never commit `alvus.json` if it contains literal credentials.

## Reporting

Please report suspected credential exposure, authentication bypasses, request-smuggling issues, SSRF paths, or routing bugs privately to the repository owner before public disclosure.

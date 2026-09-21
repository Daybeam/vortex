# Security Policy

## Reporting a vulnerability

Do NOT open a public issue for a security vulnerability. Report it via GitHub
Security Advisories (Repository → Security → Advisories → New advisory) or
email security@daybeam.dev. We will acknowledge within 48 hours and ship a
fix with credit once it is released.

## API keys and secrets

- Never commit API keys or secrets. Use environment variables
  (ProviderConfig.api_key_env), never an inline api_key.
- CI runs a gitleaks scan on every PR to catch accidental key commits.

## Sandbox honesty

The sandboxed code-execution surface has real isolation on Windows (Job
Objects). Linux cgroups and macOS sandboxing are placeholders (no-op) in the
open core. Do not run untrusted code on Linux/macOS in the open core without an
hardened sandbox.

# Security policy

Relay stores OAuth tokens that give access to people's calendars. We take reports seriously.

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub:
**Security → Report a vulnerability** on this repository ([private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)).

Please include the affected version (`relay --version`), steps to reproduce, and the impact you see. You will get an acknowledgement within 3 business days and a fix or mitigation plan as soon as the issue is confirmed. We will credit you in the advisory unless you prefer otherwise.

## Supported versions

| Version | Supported |
| --- | --- |
| 0.1.x | Yes |

## Scope

In scope: this repository's code, the published binaries and `ghcr.io/sebasbetancourt/relay` images.

Out of scope:

- vulnerabilities in Google or Microsoft services themselves;
- deployments that ignore the documented setup, e.g. a leaked `ENCRYPTION_KEY`, or `RELAY_ENV=test` in production.

## How Relay protects data

- OAuth tokens are encrypted at rest with AES-256-GCM. Each value is bound to its tenant, provider account and column through AAD, and the key never touches the database.
- API keys are stored only as SHA-256 hashes and shown once.
- The OAuth flow uses PKCE (S256) and a one-time, 10-minute `state`.
- Provider hosts are fixed by configuration: client input never chooses where Relay sends a user's token.
- Tokens, API keys, OAuth states, code verifiers and sync cursors (`syncToken`, `deltaLink`) are never logged.
- `synced_events` holds a copy of the user's calendar. Disconnect deletes that copy and its cursor; deleting the account row does the same through `ON DELETE CASCADE`.
- CI runs `govulncheck` on every change, and Dependabot keeps dependencies current.

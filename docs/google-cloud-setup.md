# Google Cloud setup (development)

Relay needs its own OAuth client at Google before it can ask a user "Do you allow Relay to read your calendar?" and receive tokens. This guide creates that client for local development.

You only do this once per environment. The result is three values in your `.env`: `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` and `GOOGLE_REDIRECT_URL`.

## 1. Create a project

1. Open [console.cloud.google.com](https://console.cloud.google.com/).
2. Project picker → **New project** → name it `relay-dev` → **Create**.
3. Make sure `relay-dev` is the selected project for the next steps.

## 2. Enable the Google Calendar API

**APIs & Services → Library** → search **Google Calendar API** → **Enable**.

Without this, token exchange works but every Calendar call fails with `403 accessNotConfigured`.

## 3. Configure the OAuth consent screen

**APIs & Services → OAuth consent screen**:

| Setting | Value | Why |
| --- | --- | --- |
| User type | **External** | Lets any Google account (not only a Workspace org) connect. |
| Publishing status | **Testing** | No Google review needed while developing. |
| Test users | Your own Google account | In Testing mode only listed users can grant consent. |
| Scopes | `openid` and `https://www.googleapis.com/auth/calendar.readonly` | `openid` gives an `id_token` with `sub` (stable account id). `calendar.readonly` is the only calendar permission v0.1 needs. No extra Google verification for `openid`. |
| Scope to write events (optional) | `https://www.googleapis.com/auth/calendar.events` | Only asked for with `?features=calendar.write`. Add it to the consent screen. Google classes it as sensitive: leaving Testing mode needs app verification. |

Testing mode limits: at most 100 test users, and refresh tokens expire after 7 days. Both go away after Google verification (see [Production](#production)).

## 4. Create the OAuth client

**APIs & Services → Credentials → Create credentials → OAuth client ID**:

- Application type: **Web application**
- Name: `relay-local`
- Authorized redirect URIs: `http://localhost:8080/v1/oauth/google/callback`

The redirect URI must match `GOOGLE_REDIRECT_URL` **exactly** (scheme, host, port and path), or Google answers `redirect_uri_mismatch`.

## 5. Put the credentials in `.env`

```bash
GOOGLE_CLIENT_ID=<client id>.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=<client secret>
GOOGLE_REDIRECT_URL=http://localhost:8080/v1/oauth/google/callback
```

- `.env` is gitignored. Never commit these values or paste them in issues or chat.
- The API refuses to start if any of them is missing or if the redirect URL is not an absolute `http(s)` URL (see `internal/config`).
- If the secret leaks: **Credentials → your client → Reset secret**, then update `.env`.

## Production

What changes beyond local development:

| Development | Production | Why |
| --- | --- | --- |
| One `relay-dev` project | One project per environment (`relay-dev`, `relay-staging`, `relay-prod`), managed with Terraform | Breaking dev never touches prod; infra is reviewable code. |
| Secrets in `.env` | Secret Manager / Vault, read at startup | No hand-copied secrets; access is audited. |
| Consent screen in Testing | Google verification, own domain, privacy policy | Removes the 100-user cap and the 7-day refresh token expiry. |
| `http://localhost` redirect | `https://` redirect on your domain | Google requires HTTPS outside localhost. |

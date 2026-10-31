# Microsoft (Entra ID) setup (development)

Relay needs its own app registration in Microsoft Entra ID (formerly Azure AD) before it can ask a user "Do you allow Relay to read your calendar?" and call Microsoft Graph on their behalf. This guide creates that registration for local development.

You do this once per environment. The result is a few values in your `.env`: `MICROSOFT_CLIENT_ID`, `MICROSOFT_CLIENT_SECRET`, `MICROSOFT_REDIRECT_URL`, `MICROSOFT_TENANT_ID` and, optionally, `MICROSOFT_EXPIRATION_DATE`.

Microsoft is optional: Relay starts with Google only, Microsoft only, or both. Configure at least one.

## 1. Register the app

1. Open [entra.microsoft.com](https://entra.microsoft.com/) → **Applications → App registrations → New registration**.
2. Name: `relay-dev`.
3. **Supported account types:** *Accounts in any organizational directory (Any Microsoft Entra ID tenant - Multitenant) and personal Microsoft accounts (e.g. Skype, Xbox)*.
   This covers both Microsoft 365 work/school mailboxes and personal Outlook.com accounts, and matches `MICROSOFT_TENANT_ID=common`.
   If the app already exists with another account type, change it in **Manifest**: first `api.requestedAccessTokenVersion: 2`, then `signInAudience: "AzureADandPersonalMicrosoftAccount"` (the portal rejects the second change without the first).
4. **Redirect URI:** platform **Web**, value `http://localhost:8080/v1/oauth/microsoft/callback`.
5. **Register.**

| Account type you picked | `MICROSOFT_TENANT_ID` |
| --- | --- |
| Any directory + personal accounts (recommended) | `common` |
| Any directory, no personal accounts | `organizations` |
| Personal accounts only | `consumers` |
| Only your own directory (single tenant) | your **Directory (tenant) ID** (a GUID) or its domain |

## 2. Copy the client ID

On the app's **Overview** page, copy **Application (client) ID** into `MICROSOFT_CLIENT_ID`.

## 3. Create a client secret

**Certificates & secrets → Client secrets → New client secret**:

- Description: `relay-local`.
- Expires: the shortest period you can live with (Entra ID allows at most 24 months).

Copy the **Value** column into `MICROSOFT_CLIENT_SECRET` right away: Entra ID shows it only once. Do **not** copy the *Secret ID*; it looks similar but is not the secret.

Put the expiry date shown in the table into `MICROSOFT_EXPIRATION_DATE` (format `YYYY-MM-DD`):

- 30 days before that date, the API logs a warning at startup.
- From that date on, the API refuses to start with a clear message, instead of failing every connect with `AADSTS7000222`.

To rotate:

1. Create a new secret.
2. Update `MICROSOFT_CLIENT_SECRET` and `MICROSOFT_EXPIRATION_DATE`.
3. Restart the API.
4. Delete the old secret.

Both secrets work at the same time, so there is no downtime.

## 4. Add API permissions

**API permissions → Add a permission → Microsoft Graph → Delegated permissions**:

| Permission            | Why                                                                                                                 |
| --------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `Calendars.Read`      | Read the user's events.                                                                                             |
| `Calendars.ReadWrite` | Only to write events. Asked for with `?features=calendar.write`, and added here so the app's consent page lists it. |
| `User.Read`           | `GET /me`: the account's stable `id` and its email. Usually present by default.                                     |
| `offline_access`      | Microsoft returns a refresh token only with this scope.                                                             |
| `openid`, `email`     | Standard sign-in scopes.                                                                                            |

None of these require admin consent for personal accounts. Some organizations block user consent; in that case an admin must use **Grant admin consent** for their directory.

## 5. Put the values in `.env`

```bash
MICROSOFT_CLIENT_ID=<Application (client) ID>
MICROSOFT_CLIENT_SECRET=<secret Value>
MICROSOFT_REDIRECT_URL=http://localhost:8080/v1/oauth/microsoft/callback
MICROSOFT_TENANT_ID=common
MICROSOFT_EXPIRATION_DATE=<YYYY-MM-DD>
```

- `.env` is gitignored. Never commit these values or paste them in issues or chat.
- The API refuses to start if Microsoft is half configured (an id without a secret, or the reverse), if the redirect URL is not absolute, or if the tenant is not one of the values above.
- On startup the log lists the enabled providers: `"providers":["google","microsoft"]`.

## Troubleshooting

| Error | Cause | Fix |
| --- | --- | --- |
| `AADSTS50011` redirect URI mismatch | `MICROSOFT_REDIRECT_URL` differs from the registered URI (scheme, host, port or path) | Make them identical |
| `AADSTS7000215` invalid client secret | Copied the *Secret ID* instead of the *Value*, or a typo | Create a new secret and copy its Value |
| `AADSTS7000222` expired client secret | The secret passed its expiry date | Rotate it (section 3) |
| `AADSTS700016` application not found in the directory | Tenant mismatch, e.g. a single-tenant app used with `common` | Fix the account type or `MICROSOFT_TENANT_ID` |
| `AADSTS50020` user account does not exist in tenant | Personal account signing in to an app that does not allow personal accounts | Use the multitenant + personal account type |
| `unauthorized_client: The client does not exist or is not enabled for consumers` | The app does not accept personal accounts, but someone signed in with Outlook.com/Hotmail through `common` | In **Manifest**, set `api.requestedAccessTokenVersion` to `2` first, then `signInAudience` to `AzureADandPersonalMicrosoftAccount`, and save. Or use `MICROSOFT_TENANT_ID=organizations` with a work/school account |
| `AADSTS65001` consent not granted | The organization requires admin consent | An admin grants consent in **API permissions** |

## Production

| Development | Production | Why |
| --- | --- | --- |
| One `relay-dev` registration | One registration per environment (`relay-dev`, `relay-staging`, `relay-prod`), managed as code (Terraform `azuread` provider) | Breaking dev never touches prod; the config is reviewable |
| Client secret in `.env` | Certificate credential, or the secret in Key Vault / a secret manager | Secrets leak and expire; certificates are harder to exfiltrate |
| `http://localhost` redirect | `https://` redirect on your domain | Entra ID requires HTTPS outside localhost |
| Unverified publisher | **Publisher verification** (Microsoft Partner Network) | Users see a verified badge instead of an "unverified" warning, and many organizations require it for user consent |

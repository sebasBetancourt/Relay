# Upgrade from 0.1 to 1.0

Your data, API keys and connected accounts carry over. `ENCRYPTION_KEY` does not change, so stored tokens stay readable. Back up the database first, as with any upgrade.

## 1. Update your API client

Two changes break clients. v0.1 made no stability promise, so they ship here, before the contract freezes.

| Where | 0.1 | 1.0 |
| --- | --- | --- |
| `GET /v1/accounts` query | `limit`, `cursor` | `max_results`, `page_token` |
| `GET /v1/accounts` response | `next_cursor` | `next_page_token` |
| Server failure | `{"error":"internal error"}` | `{"error":"internal_error"}` |

**Check your paging.** An old `cursor` parameter is ignored, not refused, so a client that was not updated gets the first page again and may loop forever. A cursor from 0.1 still works as a `page_token`.

Events also gained a `version` field. Ignore it if you do not need it.

## 2. Apply the migrations

Migrations 5 to 8 add subscriptions, the sync tables, the rate limit override and idempotency keys. They only add to the schema.

With Compose, they run on their own:

```bash
docker compose up -d --build
```

With a released image, run them before you start the new version:

```bash
docker run --rm -e DATABASE_URL ghcr.io/sebasbetancourt/relay:v1.0.0 migrate
```

Going back to 0.1 after that needs the `down` migrations, which delete the data in those tables.

## 3. Review the new settings

All of them are optional. With none of them, Relay 1.0 behaves like 0.1, plus a rate limit local to each replica.

| Variable | Default | What it turns on |
| --- | --- | --- |
| `RATE_LIMIT_PER_MINUTE`, `RATE_LIMIT_BURST` | `600`, `100` | The budget per tenant. **This is new, so a busy client can now get `429`** |
| `RATE_LIMIT_IP_PER_MINUTE`, `RATE_LIMIT_IP_BURST` | `1200`, `200` | The budget per IP, for the OAuth callback and invalid keys |
| `REDIS_URL` | empty | One rate limit budget across replicas. Compose sets it |
| `CLIENT_IP_HEADER` | empty | **Set it behind a proxy**, or every client shares one IP budget |
| `NATS_URL` | empty | Delta sync, and the queue push uses. Compose sets it |
| `WEBHOOK_BASE_URL` | empty | Calendar push. Needs a public `https` origin and `NATS_URL` |
| `REFRESH_AHEAD`, `REFRESH_CONCURRENCY` | `10m`, `8` | Background token refresh, on by default |

Running more than one replica without `REDIS_URL` multiplies the real rate limit by the number of replicas.

## 4. Writing events needs consent again

Reading keeps working with no action. Accounts connected on 0.1 only granted reading, so a write answers `403 insufficient_scope` until the user accepts more:

1. Add `calendar.events` to Google's consent screen, and `Calendars.ReadWrite` to the Entra ID app. See the [Google](google-cloud-setup.md) and [Microsoft](microsoft-setup.md) guides.
2. Send the user through the `reconnect_url` in the `403`, which is `/v1/connect/{provider}?features=calendar.write`.

The account keeps its id. Only tenants that want to write need this.

## 5. Check it

```bash
curl -s localhost:8080/readyz
```

It must answer `ready`. Then list your accounts and read an account's events with an existing API key.

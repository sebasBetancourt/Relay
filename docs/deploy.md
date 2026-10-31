# Deploy Relay

This guide runs Relay on [Fly.io](https://docs.fly.io/) with a public HTTPS address, private Postgres and NATS, and daily encrypted backups. [ADR 0012](adr/0012-deploy-on-fly.md) explains the choices. Any host that runs containers works: Relay needs Postgres, the env vars in the [README](../README.md#configuration), and `relay migrate` before each new version.

Fly has no free tier. At the time of writing, these three small machines and two 1 GB volumes cost about **$8 per month** in `iad`. See [current pricing](https://docs.fly.io/about/pricing).

The names below match the `fly.toml` files. Pick your own and change `app` in all three files, and choose before step 5: your redirect URIs depend on the name, and changing it means registering them again with both providers.

## 1. Set up

Install [flyctl](https://docs.fly.io/flyctl/install/) and sign in:

```bash
fly auth login
```

## 2. Postgres

Create the app and its volume:

```bash
fly apps create relay-lpfw2a-db
```

```bash
fly volumes create relay_db --app relay-lpfw2a-db --region iad --size 1 --yes
```

Generate a password. Keep this shell open: step 4 uses it.

```bash
DB_PASSWORD="$(openssl rand -hex 24)"
```

```bash
fly secrets set POSTGRES_PASSWORD="$DB_PASSWORD" --app relay-lpfw2a-db --stage
```

```bash
fly deploy --config deploy/postgres/fly.toml --ha=false
```

## 3. NATS

Needed for delta sync and calendar push. Skip it to stay on polling, and leave out `NATS_URL` and `WEBHOOK_BASE_URL` in step 4.

```bash
fly apps create relay-lpfw2a-nats
```

```bash
fly volumes create relay_nats --app relay-lpfw2a-nats --region iad --size 1 --yes
```

```bash
fly deploy --config deploy/nats/fly.toml --ha=false
```

## 4. The API

Create the app, and generate the encryption key:

```bash
fly apps create relay-lpfw2a
```

```bash
ENCRYPTION_KEY="$(openssl rand -base64 32)"
```

> **Store `ENCRYPTION_KEY` in a password manager now.** It is not in any backup on purpose. Without it, the tokens in a restored database cannot be read, and every account must connect again.

```bash
fly secrets set --app relay-lpfw2a --stage \
  ENCRYPTION_KEY="$ENCRYPTION_KEY" \
  DATABASE_URL="postgres://relay:$DB_PASSWORD@relay-lpfw2a-db.internal:5432/relay?sslmode=disable" \
  NATS_URL="nats://relay-lpfw2a-nats.internal:4222" \
  WEBHOOK_BASE_URL="https://relay-lpfw2a.fly.dev"
```

`sslmode=disable` is safe here: `.internal` traffic stays inside Fly's WireGuard network. Set the provider variables the same way, without putting them in your shell history. `fly secrets import` reads them from standard input:

```bash
fly secrets import --app relay-lpfw2a --stage < provider-secrets.env
```

That file holds `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `GOOGLE_REDIRECT_URL`, and the `MICROSOFT_` equivalents. Delete it afterwards. The redirect URLs are the ones in the next step.

Deploy. `--ha=false` keeps one machine: a second one would need Redis to share rate limits.

```bash
fly deploy --ha=false
```

This builds from your checkout. Once a release exists, pin it instead, and never use `latest`:

```bash
fly deploy --ha=false --image ghcr.io/sebasbetancourt/relay:v0.2.0
```

## 5. Register the redirect URIs

| Provider | Where | URI |
| --- | --- | --- |
| Google | [Credentials](google-cloud-setup.md) | `https://relay-lpfw2a.fly.dev/v1/oauth/google/callback` |
| Microsoft | [App registration](microsoft-setup.md) | `https://relay-lpfw2a.fly.dev/v1/oauth/microsoft/callback` |

While a Google app is in Testing mode, it takes at most 100 test users and their refresh tokens expire after 7 days. Accounts will show as revoked until the app is verified.

## 6. Check it

```bash
curl -s https://relay-lpfw2a.fly.dev/readyz
```

It must answer `ready`. Then confirm Postgres and NATS have no public address:

```bash
fly ips list --app relay-lpfw2a-db
```

```bash
fly ips list --app relay-lpfw2a-nats
```

Both lists must be empty. Also scan the API's address from outside. All of these ports must be closed or filtered:

```bash
nmap -Pn -p 22,4222,5432,6379,8222 relay-lpfw2a.fly.dev
```

Finally, confirm a client cannot fake its address. This sends a new `Fly-Client-IP` on each request, past the default burst of 200. If Fly overwrites the header, as `CLIENT_IP_HEADER` requires, the output includes `429`:

```bash
for i in $(seq 1 260); do curl -s -o /dev/null -w '%{http_code}\n' -H "Fly-Client-IP: 198.51.100.$((i % 250))" https://relay-lpfw2a.fly.dev/v1/me; done | sort | uniq -c
```

If every answer is `401`, remove `CLIENT_IP_HEADER` from `fly.toml`: the limit by IP would be useless.

## 7. Create a tenant

Reach the database through a local proxy. Leave this running in another terminal:

```bash
fly proxy 15432:5432 --app relay-lpfw2a-db
```

```bash
DATABASE_URL="postgres://relay:$DB_PASSWORD@localhost:15432/relay?sslmode=disable" RELAY_ENV=live go run ./cmd/relayctl tenant create --name "Demo" > demo.key
```

Never publish that key. Try the whole flow against the public URL, with your own accounts only:

```bash
RELAY_URL=https://relay-lpfw2a.fly.dev RELAY_KEY="$(cat demo.key)" scripts/demo.sh
```

## Backups

Fly snapshots volumes daily, but a snapshot lives on the same platform as the data. [`backup.yml`](../.github/workflows/backup.yml) adds a daily `pg_dump`, encrypted with [age](https://github.com/FiloSottile/age) and kept for 30 days as a workflow artifact. Encryption is required: **anyone signed in to GitHub can download artifacts of a public repository.**

It stays off until configured. Generate a key pair and store the private key with `ENCRYPTION_KEY`, outside Fly and GitHub:

```bash
age-keygen -o relay-backup.key
```

Give GitHub the public key (the `age1…` line it prints), the database app, and the secrets:

```bash
gh variable set BACKUP_AGE_RECIPIENT --body "age1..."
```

```bash
gh variable set BACKUP_DB_APP --body "relay-lpfw2a-db"
```

```bash
fly tokens create deploy --app relay-lpfw2a-db | gh secret set FLY_API_TOKEN
```

```bash
printf '%s' "$DB_PASSWORD" | gh secret set BACKUP_DB_PASSWORD
```

Run it once now:

```bash
gh workflow run backup.yml
```

### Restore

A backup only counts once it has been restored. Try this on a schedule, against a clean local database. Download the newest artifact, then:

```bash
age -d -i relay-backup.key relay.dump.age > relay.dump
```

```bash
docker compose up -d --wait postgres
```

```bash
docker compose exec -T postgres pg_restore -U relay -d relay --clean --if-exists --no-owner < relay.dump
```

Then start the API against it, with the production `ENCRYPTION_KEY`, and read an account's events. To restore on Fly, send the same dump through `fly proxy`.

## Operating it

- **Logs:** `fly logs --app relay-lpfw2a` shows the structured JSON. Fly keeps them for a short time.
- **Health:** Fly polls `/readyz` and marks the machine as failed when Postgres is down.
- **Restarts:** machines come back on their own after a host restart or crash, and tokens refresh as soon as the API is back.
- **New version:** `fly deploy` runs `relay migrate` first and stops if it fails, so a bad migration never reaches traffic.
- **One server:** each piece runs once, in one region. A host failure means downtime until Fly reschedules it. That is an accepted limit for now.
